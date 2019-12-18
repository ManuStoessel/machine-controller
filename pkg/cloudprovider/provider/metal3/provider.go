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

	bmh "github.com/metal3-io/baremetal-operator/pkg/apis/metal3/v1alpha1"

	"github.com/kubermatic/machine-controller/pkg/apis/cluster/common"
	"github.com/kubermatic/machine-controller/pkg/apis/cluster/v1alpha1"
	cloudprovidererrors "github.com/kubermatic/machine-controller/pkg/cloudprovider/errors"
	"github.com/kubermatic/machine-controller/pkg/cloudprovider/instance"
	metal3types "github.com/kubermatic/machine-controller/pkg/cloudprovider/provider/metal3/types"
	cloudprovidertypes "github.com/kubermatic/machine-controller/pkg/cloudprovider/types"
	"github.com/kubermatic/machine-controller/pkg/providerconfig"
	providerconfigtypes "github.com/kubermatic/machine-controller/pkg/providerconfig/types"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// HostAnnotation marks a machine resource as managed by this provider
	HostAnnotation = "metal3.io/BareMetalHost"
)

type provider struct {
	configVarResolver *providerconfig.ConfigVarResolver
}

// New returns a Metal3 provider
func New(configVarResolver *providerconfig.ConfigVarResolver) cloudprovidertypes.Provider {
	return &provider{configVarResolver: configVarResolver}
}

type Config struct {
	Kubeconfig     rest.Config
	Namespace      string
	BMCIP          string
	BootMacAddress string
	ImageURL       string
	ImageCheckSum  string
	Metal3Image    string
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
	config := Config{}
	configString, err := p.configVarResolver.GetConfigVarStringValueOrEnv(rawConfig.Kubeconfig, "MEATL3_KUBECONFIG")
	if err != nil {
		return nil, nil, fmt.Errorf(`failed to get value of "config" field: %v`, err)
	}
	config.Namespace, err = p.configVarResolver.GetConfigVarStringValue(rawConfig.Namespace)
	if err != nil {
		return nil, nil, fmt.Errorf(`failed to get value of "namespace" field: %v`, err)
	}
	config.BMCIP, err = p.configVarResolver.GetConfigVarStringValue(rawConfig.BMCIP)
	if err != nil {
		return nil, nil, fmt.Errorf(`failed to get value of "bmcip" field: %v`, err)
	}
	config.BootMacAddress, err = p.configVarResolver.GetConfigVarStringValue(rawConfig.BootMacAddress)
	if err != nil {
		return nil, nil, fmt.Errorf(`failed to get value of "bootmacaddress" field: %v`, err)
	}
	config.ImageURL, err = p.configVarResolver.GetConfigVarStringValue(rawConfig.ImageURL)
	if err != nil {
		return nil, nil, fmt.Errorf(`failed to get value of "imageurl" field: %v`, err)
	}
	config.ImageCheckSum, err = p.configVarResolver.GetConfigVarStringValue(rawConfig.ImageCheckSum)
	if err != nil {
		return nil, nil, fmt.Errorf(`failed to get value of "imagechecksum" field: %v`, err)
	}
	config.Metal3Image, err = p.configVarResolver.GetConfigVarStringValue(rawConfig.Metal3Image)
	if err != nil {
		return nil, nil, fmt.Errorf(`failed to get value of "metal3image" field: %v`, err)
	}
	restConfig, err := clientcmd.RESTConfigFromKubeConfig([]byte(configString))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decode kubeconfig: %v", err)
	}
	config.Kubeconfig = *restConfig

	return &config, &pconfig, nil
}

func (p *provider) getHost(ctx context.Context, machine *v1alpha1.Machine) (*bmh.BareMetalHost, error) {
	annotations := machine.ObjectMeta.GetAnnotations()
	if annotations == nil {
		return nil, nil
	}
	hostKey, ok := annotations[HostAnnotation]
	if !ok {
		return nil, nil
	}
	hostNamespace, hostName, err := cache.SplitMetaNamespaceKey(hostKey)
	if err != nil {
		return nil, fmt.Errorf("failed parsing annotation value \"%s\": %v", hostKey, err)
	}

	c, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return nil, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, due to %v", err),
		}
	}
	sigClient, err := client.New(&c.Kubeconfig, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("failed to get metal3 client: %v", err)
	}

	host := bmh.BareMetalHost{}
	key := client.ObjectKey{
		Name:      hostName,
		Namespace: hostNamespace,
	}
	err = sigClient.Get(ctx, key, &host)
	if kerrors.IsNotFound(err) {
		return nil, cloudprovidererrors.ErrInstanceNotFound
	} else if err != nil {
		return nil, fmt.Errorf("failed to get baremetalhost: %v", err)
	}
	return &host, nil
}

func (p *provider) Get(machine *v1alpha1.Machine, _ *cloudprovidertypes.ProviderData) (instance.Instance, error) {
	ctx := context.Background()

	host, err := p.getHost(ctx, machine)
	if err != nil {
		return nil, err
	}

	if host.DeletionTimestamp != nil {
		return nil, cloudprovidererrors.ErrInstanceNotFound
	}

	return &bmhInstance{host: host}, nil
}

// MigrateUID TODO: evaluate if this function is really needed in this context
func (p *provider) MigrateUID(machine *v1alpha1.Machine, new types.UID) error {
	return nil
}

func (p *provider) Validate(spec v1alpha1.MachineSpec) error {
	c, _, err := p.getConfig(spec.ProviderSpec)
	if err != nil {
		return fmt.Errorf("failed to parse config: %v", err)
	}
	sigClient, err := client.New(&c.Kubeconfig, client.Options{})
	if err != nil {
		return fmt.Errorf("failed to get kubevirt client: %v", err)
	}
	// Check if we can reach the API of the target cluster
	host := &bmh.BareMetalHost{}
	if err := sigClient.Get(context.Background(), types.NamespacedName{Namespace: c.Namespace, Name: "not-expected-to-exist"}, host); err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("failed to request BareMetalHost: %v", err)
	}

	return nil
}

func (p *provider) AddDefaults(spec v1alpha1.MachineSpec) (v1alpha1.MachineSpec, error) {
	return spec, nil
}

func (p *provider) GetCloudConfig(spec v1alpha1.MachineSpec) (config string, name string, err error) {
	return "", "", nil
}

// MachineMetricsLabels is not implemented since you're not supposed to do API
// calls here. But using Metal3 we would need to fetch the BareMetalHost.Status
// to know about e.g. Memory and CPU
func (p *provider) MachineMetricsLabels(machine *v1alpha1.Machine) (map[string]string, error) {
	return nil, nil
}

func (p *provider) Create(machine *v1alpha1.Machine, _ *cloudprovidertypes.ProviderData, userdata string) (instance.Instance, error) {
	c, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return nil, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, due to %v", err),
		}
	}

	userDataSecretName := fmt.Sprintf("userdata-%s", machine.Name)

	host := &bmh.BareMetalHost{}

	if host.Spec.Image == nil {
		host.Spec.Image = &bmh.Image{
			URL:      c.ImageURL,
			Checksum: c.ImageCheckSum,
		}
		host.Spec.UserData.Name = userDataSecretName
		host.Spec.UserData.Namespace = machine.Namespace
	}

	host.Spec.ConsumerRef = &corev1.ObjectReference{
		Kind:       "Machine",
		Name:       machine.Name,
		Namespace:  machine.Namespace,
		APIVersion: machine.APIVersion,
	}

	host.Spec.Online = true

	sigClient, err := client.New(&c.Kubeconfig, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("failed to get kubevirt client: %v", err)
	}
	ctx := context.Background()

	exists, err := bareMetalOperatorExists(ctx, sigClient, "kube-system")
	if err != nil {
		return nil, fmt.Errorf("failed to check for baremetal operator deployment: %v", err)
	}
	if !exists {
		err = deployBareMetalOperator(ctx, sigClient, "kube-system", "quay.io/metal3-io/baremetal-operator:master") // TODO: use stable metal3 baremetal-operator image
		if err != nil {
			return nil, fmt.Errorf("failed to create baremetal operator deployment: %v", err)
		}
	}


	if err := sigClient.Create(ctx, host); err != nil {
		return nil, fmt.Errorf("failed to create baremetalhost: %v", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            userDataSecretName,
			Namespace:       host.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(host, schema.GroupVersionKind{Group: "metal3.io", Version: "v1alpha1", Kind: "BareMetalHost"})},
		},
		Data: map[string][]byte{"userdata": []byte(userdata)},
	}
	if err := sigClient.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("failed to create secret for userdata: %v", err)
	}

	return &bmhInstance{host: host}, nil

}

func (p *provider) Cleanup(machine *v1alpha1.Machine, _ *cloudprovidertypes.ProviderData) (bool, error) {
	c, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return false, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, due to %v", err),
		}
	}
	sigClient, err := client.New(&c.Kubeconfig, client.Options{})
	if err != nil {
		return false, fmt.Errorf("failed to get metal3 client: %v", err)
	}
	ctx := context.Background()

	host := &bmh.BareMetalHost{}
	if err := sigClient.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: machine.Name}, host); err != nil {
		if !kerrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to get BareMetalHost %s: %v", machine.Name, err)
		}
		// BareMetalHost is gone
		return true, nil
	}

	return false, sigClient.Delete(ctx, host)
}

func (p *provider) SetMetricsForMachines(machines v1alpha1.MachineList) error {
	return nil
}

func bareMetalOperatorExists(ctx context.Context, c client.Client, ns string) (bool, error) {
	deployment := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "metal3-baremetal-operator"}, deployment); err != nil {
		if !kerrors.IsNotFound(err) {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

func deployBareMetalOperator(ctx context.Context, c client.Client, ns string, image string) error {
	// TODO: learn how ironic works and deploy all the shit from here: https://github.com/metal3-io/baremetal-operator/tree/master/deploy
	operator := &appsv1.Deployment{}
	operator.Name = "metal3-baremetal-operator"
	operator.Namespace = ns
	operator.Spec.Replicas = func(i int32) *int32 { return &i }(1) // just don't ask...
	operator.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app":"metal3-baremetal-operator"}}
	operator.Spec.Template = corev1.PodTemplateSpec{
		// TODO
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:         "baremetal-operator",
					Image:        image,
					Args:         []string{"bla", "foo"},
				},
			},
		},
	}
	if err := c.Create(ctx, operator); err != nil {
		return err
	}
	return nil
}

type bmhInstance struct {
	host *bmh.BareMetalHost
}

func (b *bmhInstance) Name() string {
	return b.host.Name
}

func (b *bmhInstance) ID() string {
	return b.host.Status.Provisioning.ID
}

func (b *bmhInstance) Addresses() []string {
	addresses := make([]string, len(b.host.Status.HardwareDetails.NIC))
	for _, nic := range b.host.Status.HardwareDetails.NIC {
		addresses = append(addresses, nic.IP)
	}

	return addresses
}

func (b *bmhInstance) Status() instance.Status {
	if b.host.Spec.Online && b.host.Status.PoweredOn && b.host.Status.OperationalStatus == "OK" {
		return instance.StatusRunning
	}

	return instance.StatusUnknown
}