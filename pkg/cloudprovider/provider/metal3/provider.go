/*
Copyright 2019 The Machine Controller Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metal3

import (
	"context"
	"encoding/json"
	"fmt"

	metal3v1alpha1 "github.com/metal3-io/baremetal-operator/pkg/apis/metal3/v1alpha1"

	"github.com/kubermatic/machine-controller/pkg/apis/cluster/common"
	"github.com/kubermatic/machine-controller/pkg/apis/cluster/v1alpha1"
	cloudprovidererrors "github.com/kubermatic/machine-controller/pkg/cloudprovider/errors"
	"github.com/kubermatic/machine-controller/pkg/cloudprovider/instance"
	metal3types "github.com/kubermatic/machine-controller/pkg/cloudprovider/provider/metal3/types"
	cloudprovidertypes "github.com/kubermatic/machine-controller/pkg/cloudprovider/types"
	"github.com/kubermatic/machine-controller/pkg/providerconfig"
	providerconfigtypes "github.com/kubermatic/machine-controller/pkg/providerconfig/types"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	dynscheme *runtime.Scheme = runtime.NewScheme()
)

func init() {
	// Workaround for fixing this error: 'no kind is registered for the type v1alpha1.BareMetalHost in scheme "k8s.io/client-go/kubernetes/scheme/register.go:65"'
	metav1.AddToGroupVersion(dynscheme, metal3v1alpha1.SchemeGroupVersion)
	dynscheme.AddKnownTypes(metal3v1alpha1.SchemeGroupVersion, &metal3v1alpha1.BareMetalHost{}, &metal3v1alpha1.BareMetalHostList{})
}

const (
	bmcSecretName      string = "metal3-bmh-bmc-credentials"
	userDataSecretName string = "metal3-bmh-userdata"
)

type provider struct {
	configVarResolver *providerconfig.ConfigVarResolver
}

// New returns a Metal3 provider
func New(configVarResolver *providerconfig.ConfigVarResolver) cloudprovidertypes.Provider {
	return &provider{configVarResolver: configVarResolver}
}

type Config struct {
	BMCAddress    string
	BMCUser       string
	BMCPassword   string
	ImageURL      string
	ImageChecksum string
	Kubeconfig    rest.Config
	Labels        map[string]string
}

func (p *provider) getConfig(s v1alpha1.ProviderSpec) (*Config, *providerconfigtypes.Config, error) {
	if s.Value == nil {
		return nil, nil, fmt.Errorf("machine.spec.providerconfig.value is nil")
	}
	pconfig := providerconfigtypes.Config{}
	err := json.Unmarshal(s.Value.Raw, &pconfig)
	if err != nil {
		return nil, nil, err
	}

	rawConfig := metal3types.RawConfig{}
	if err = json.Unmarshal(pconfig.CloudProviderSpec.Raw, &rawConfig); err != nil {
		return nil, nil, err
	}

	c := Config{}
	c.BMCPassword, err = p.configVarResolver.GetConfigVarStringValueOrEnv(rawConfig.BMCPassword, "M3_BMCPASSWORD")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"bmcpassword\" field, error = %v", err)
	}
	c.BMCUser, err = p.configVarResolver.GetConfigVarStringValueOrEnv(rawConfig.BMCUser, "M3_BMCUSER")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"bmcuser\" field, error = %v", err)
	}
	c.BMCAddress, err = p.configVarResolver.GetConfigVarStringValueOrEnv(rawConfig.BMCAddress, "M3_BMCADDRESS")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"bmcaddress\" field, error = %v", err)
	}
	c.ImageURL, err = p.configVarResolver.GetConfigVarStringValueOrEnv(rawConfig.ImageURL, "M3_IMAGEURL")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"imageurl\" field, error = %v", err)
	}
	c.ImageChecksum, err = p.configVarResolver.GetConfigVarStringValueOrEnv(rawConfig.ImageChecksum, "M3_IMAGECHECKSUM")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"imagechecksum\" field, error = %v", err)
	}
	configString, err := p.configVarResolver.GetConfigVarStringValueOrEnv(rawConfig.Kubeconfig, "M3_KUBECONFIG")
	if err != nil {
		return nil, nil, fmt.Errorf(`failed to get value of "kubeconfig" field: %v`, err)
	}
	restConfig, err := clientcmd.RESTConfigFromKubeConfig([]byte(configString))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decode kubeconfig: %v", err)
	}
	c.Kubeconfig = *restConfig
	c.Labels = rawConfig.Labels
	return &c, &pconfig, err
}

func (p *provider) Validate(spec v1alpha1.MachineSpec) error {
	_, _, err := p.getConfig(spec.ProviderSpec)
	if err != nil {
		return fmt.Errorf("failed to parse config: %v", err)
	}

	// TODO: validate more

	return nil
}

func (p *provider) Create(machine *v1alpha1.Machine, _ *cloudprovidertypes.ProviderData, userdata string) (instance.Instance, error) {
	c, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return nil, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, due to %v", err),
		}
	}
	sigClient, err := client.New(&c.Kubeconfig, client.Options{Scheme: dynscheme})
	if err != nil {
		return nil, fmt.Errorf("failed to get metal3 client: %v", err)
	}
	ctx := context.Background()

	bmh := &metal3v1alpha1.BareMetalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machine.Name,
			Namespace: machine.Namespace,
			Labels:    c.Labels,
		},
		Spec: metal3v1alpha1.BareMetalHostSpec{
			BMC: metal3v1alpha1.BMCDetails{
				Address:         c.BMCAddress,
				CredentialsName: bmcSecretName,
			},
			Online:                true,
			ExternallyProvisioned: false,
			Image: &metal3v1alpha1.Image{
				URL:      c.ImageURL,
				Checksum: c.ImageChecksum,
			},
			Taints: machine.Spec.Taints,
			UserData: &corev1.SecretReference{
				Name:      userDataSecretName,
				Namespace: machine.Namespace,
			},
			ConsumerRef: &corev1.ObjectReference{
				Kind:       "Machine",
				Namespace:  machine.Namespace,
				Name:       machine.Name,
				UID:        machine.UID, // no idea...
				APIVersion: "metal3.io/v1alpha1",
			},
		},
	}
	if err := sigClient.Create(ctx, bmh); err != nil {
		return nil, fmt.Errorf("failed to create bmh: %v", err)
	}

	bmcSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            bmcSecretName,
			Namespace:       bmh.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(bmh, schema.GroupVersionKind{Group: "metal3.io", Version: "v1alpha1", Kind: "BareMetalHost"})},
		},
		Data: map[string][]byte{
			"username": []byte(c.BMCUser),
			"password": []byte(c.BMCPassword),
		},
	}
	if err := sigClient.Create(ctx, bmcSecret); err != nil {
		return nil, fmt.Errorf("failed to create bmc credentials secret: %v", err)
	}

	userDataSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            userDataSecretName,
			Namespace:       bmh.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(bmh, schema.GroupVersionKind{Group: "metal3.io", Version: "v1alpha1", Kind: "BareMetalHost"})},
		},
		Data: map[string][]byte{
			"userData": []byte(userdata),
		},
	}
	if err := sigClient.Create(ctx, userDataSecret); err != nil {
		return nil, fmt.Errorf("failed to create userdata secret: %v", err)
	}

	return metal3Host{host: bmh}, nil
}

func (p *provider) Cleanup(machine *v1alpha1.Machine, data *cloudprovidertypes.ProviderData) (bool, error) {
	c, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return false, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, due to %v", err),
		}
	}
	sigClient, err := client.New(&c.Kubeconfig, client.Options{Scheme: dynscheme})
	if err != nil {
		return false, fmt.Errorf("failed to get metal3 client: %v", err)
	}
	ctx := context.Background()

	bmh := &metal3v1alpha1.BareMetalHost{}
	if err := sigClient.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: machine.Name}, bmh); err != nil {
		if !kerrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to get BareMetalHost %s: %v", machine.Name, err)
		}
		// BareMetalHost is already gone
		return true, nil
	}

	return false, sigClient.Delete(ctx, bmh)
}

func (p *provider) AddDefaults(spec v1alpha1.MachineSpec) (v1alpha1.MachineSpec, error) {
	return spec, nil
}

func (p *provider) Get(machine *v1alpha1.Machine, _ *cloudprovidertypes.ProviderData) (instance.Instance, error) {
	c, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return nil, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, due to %v", err),
		}
	}
	sigClient, err := client.New(&c.Kubeconfig, client.Options{Scheme: dynscheme})
	if err != nil {
		return nil, fmt.Errorf("failed to get metal3 client: %v", err)
	}
	ctx := context.Background()

	bmh := &metal3v1alpha1.BareMetalHost{}
	if err := sigClient.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: machine.Name}, bmh); err != nil {
		if !kerrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get BareMetalHost %s: %v", machine.Name, err)
		}
		return nil, cloudprovidererrors.ErrInstanceNotFound
	}

	return metal3Host{host: bmh}, nil
}

func (p *provider) MigrateUID(machine *v1alpha1.Machine, new types.UID) error {
	// Probably doesn't need to be implemented since we dont have external UIDs (BareMetalHost is a CRD)

	return nil
}

func (p *provider) GetCloudConfig(spec v1alpha1.MachineSpec) (config string, name string, err error) {
	return "", "", nil
}

func (p *provider) MachineMetricsLabels(machine *v1alpha1.Machine) (map[string]string, error) {
	// TODO maybe?
	return nil, nil
}

func (p *provider) SetMetricsForMachines(machines v1alpha1.MachineList) error {
	return nil
}

type metal3Host struct {
	host *metal3v1alpha1.BareMetalHost
}

func (m metal3Host) Name() string {
	return m.host.Name
}

func (m metal3Host) ID() string {
	return string(m.host.UID)
}

func (m metal3Host) Addresses() []string {
	var addresses []string
	if m.host.Status.HardwareDetails.NIC == nil {
		return addresses
	}

	for _, nic := range m.host.Status.HardwareDetails.NIC {
		addresses = append(addresses, nic.IP)
	}

	return addresses
}

func (m metal3Host) Status() instance.Status {
	switch m.host.Status.Provisioning.State {
	case metal3v1alpha1.StateProvisioning:
		return instance.StatusCreating
	case metal3v1alpha1.StateReady:
		return instance.StatusRunning
	case metal3v1alpha1.StateDeleting:
		return instance.StatusDeleting
	default:
		return instance.StatusUnknown
	}
}
