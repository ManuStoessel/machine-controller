/*
Copyright 2017 The Kubernetes Authors.

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

package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	listerscorev1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"

	"github.com/golang/glog"
	"github.com/heptiolabs/healthcheck"
	machineclientset "github.com/kubermatic/machine-controller/pkg/client/clientset/versioned"
	machineinformers "github.com/kubermatic/machine-controller/pkg/client/informers/externalversions"
	machinelistersv1alpha1 "github.com/kubermatic/machine-controller/pkg/client/listers/machines/v1alpha1"
	"github.com/kubermatic/machine-controller/pkg/clusterinfo"
	"github.com/kubermatic/machine-controller/pkg/controller"
	machinehealth "github.com/kubermatic/machine-controller/pkg/health"
	"github.com/kubermatic/machine-controller/pkg/machines"
	"github.com/kubermatic/machine-controller/pkg/signals"
	"github.com/kubermatic/machine-controller/pkg/ssh"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	masterURL     string
	kubeconfig    string
	sshKeyName    string
	clusterDNSIPs string
	listenAddress string
	workerCount   int
)

const (
	controllerName                     = "machine-controller"
	defaultLeaderElectionNamespace     = "kube-system"
	defaultLeaderElectionLeaseDuration = 15 * time.Second
	defaultLeaderElectionRenewDeadline = 10 * time.Second
	defaultLeaderElectionRetryPeriod   = 2 * time.Second
)

// controllerRunOptions holds data that are required to create and run machine controller
type controllerRunOptions struct {
	// kubeClient a client that knows how to consume kubernetes API
	kubeClient *kubernetes.Clientset

	// extClient a client that knows how to consume kubernetes extension API
	extClient *apiextclient.Clientset

	// machineClient a client that knows how to consume Machine resources
	machineClient *machineclientset.Clientset

	// sshKeyPair sets a trust between the controller and a machine by
	// pre-installing public part of that key on a machine.
	sshKeyPair *ssh.PrivateKey

	// this essentially sets the cluster DNS IP addresses. The list is passed to kubelet and then down to pods.
	clusterDNSIPs []net.IP

	// metrics a struct that holds all metrics we want to collect
	metrics *MachineControllerMetrics

	// leaderElectionClient holds a client that is used by the leader election library
	leaderElectionClient *kubernetes.Clientset

	// nodeInformer holds a shared informer for Nodes
	nodeInformer cache.SharedIndexInformer

	// nodeLister holds a lister that knows how to list Nodes from a cache
	nodeLister listerscorev1.NodeLister

	// configMapLister holds a lister that knows how to list ConfigMaps from a cache
	configMapLister listerscorev1.ConfigMapLister

	// machineInformer holds a shared informer for Machines
	machineInformer cache.SharedIndexInformer

	// machineLister holds a lister that knows how to list Machines from a cache
	machineLister machinelistersv1alpha1.MachineLister

	// kubeconfigProvider knows how to get cluster information stored under a ConfigMap
	kubeconfigProvider controller.KubeconfigProvider
}

func main() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&sshKeyName, "ssh-key-name", "machine-controller", "The name of the private key. This name will be used when a public key will be created at the cloud provider.")
	flag.StringVar(&clusterDNSIPs, "cluster-dns", "10.10.10.10", "Comma-separated list of DNS server IP address.")
	flag.IntVar(&workerCount, "worker-count", 5, "Number of workers to process machines. Using a high number with a lot of machines might cause getting rate-limited from your cloud provider.")
	flag.StringVar(&listenAddress, "internal-listen-address", "127.0.0.1:8085", "The address on which the http server will listen on. The server exposes metrics on /metrics, liveness check on /live and readiness check on /ready")

	flag.Parse()

	ips, err := parseClusterDNSIPs(clusterDNSIPs)
	if err != nil {
		glog.Fatalf("invalid cluster dns specified: %v", err)
	}

	// TODO: add graceful shutdown, propagate stopCh to run method and to http server
	stopCh := signals.SetupSignalHandler()

	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		glog.Fatalf("error building kubeconfig: %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		glog.Fatalf("error building kubernetes clientset for kubeClient: %v", err)
	}

	extClient, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		glog.Fatalf("error building kubernetes clientset for extClient: %v", err)
	}

	machineClient, err := machineclientset.NewForConfig(cfg)
	if err != nil {
		glog.Fatalf("error building example clientset for machineClient: %v", err)
	}

	leaderElectionClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		glog.Fatalf("error building kubernetes clientset for leaderElectionClient: %v", err)
	}

	err = machines.EnsureCustomResourceDefinitions(extClient)
	if err != nil {
		glog.Fatalf("failed to create CustomResourceDefinition: %v", err)
	}

	key, err := ssh.EnsureSSHKeypairSecret(sshKeyName, kubeClient)
	if err != nil {
		glog.Fatalf("failed to get/create ssh key configmap: %v", err)
	}

	// before we acquire a lock we actually warm up caches mirroring the state of the API server
	machineInformerFactory := machineinformers.NewSharedInformerFactory(machineClient, time.Second*30)
	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Second*30)
	kubePublicKubeInformerFactory := kubeinformers.NewFilteredSharedInformerFactory(kubeClient, time.Second*30, metav1.NamespacePublic, nil)
	defaultKubeInformerFactory := kubeinformers.NewFilteredSharedInformerFactory(kubeClient, time.Second*30, metav1.NamespaceDefault, nil)

	kubeconfigProvider := clusterinfo.New(cfg, kubePublicKubeInformerFactory.Core().V1().ConfigMaps().Lister(), defaultKubeInformerFactory.Core().V1().Endpoints().Lister())
	runOptions := controllerRunOptions{
		kubeClient:           kubeClient,
		extClient:            extClient,
		machineClient:        machineClient,
		sshKeyPair:           key,
		metrics:              NewMachineControllerMetrics(),
		clusterDNSIPs:        ips,
		leaderElectionClient: leaderElectionClient,
		nodeInformer:         kubeInformerFactory.Core().V1().Nodes().Informer(),
		nodeLister:           kubeInformerFactory.Core().V1().Nodes().Lister(),
		configMapLister:      kubePublicKubeInformerFactory.Core().V1().ConfigMaps().Lister(),
		machineInformer:      machineInformerFactory.Machine().V1alpha1().Machines().Informer(),
		machineLister:        machineInformerFactory.Machine().V1alpha1().Machines().Lister(),
		kubeconfigProvider:   kubeconfigProvider,
	}

	go kubeInformerFactory.Start(stopCh)
	go kubePublicKubeInformerFactory.Start(stopCh)
	go defaultKubeInformerFactory.Start(stopCh)
	go machineInformerFactory.Start(stopCh)

	for _, syncsMap := range []map[reflect.Type]bool{kubeInformerFactory.WaitForCacheSync(stopCh), kubePublicKubeInformerFactory.WaitForCacheSync(stopCh), machineInformerFactory.WaitForCacheSync(stopCh), defaultKubeInformerFactory.WaitForCacheSync(stopCh)} {
		for key, synced := range syncsMap {
			if !synced {
				glog.Fatalf("unable to sync %s", key)
			}
		}
	}

	startUtilHttpServerOrDie(kubeClient, kubeconfigProvider, stopCh)
	startControllerViaLeaderElectionOrDie(runOptions)
}

// startControllerViaLeaderElectionOrDie starts machine controller only if a proper lock was acquired.
// This essentially means that we can have multiple instances and at the same time only one is operational.
// The program terminates when the leadership was lost.
func startControllerViaLeaderElectionOrDie(runOptions controllerRunOptions) {
	id, err := os.Hostname()
	if err != nil {
		glog.Fatalf("error getting hostname: %s", err.Error())
	}
	// add a seed to the id, so that two processes on the same host don't accidentally both become active
	id = id + "_" + string(uuid.NewUUID())

	rl := resourcelock.EndpointsLock{
		EndpointsMeta: metav1.ObjectMeta{
			Namespace: defaultLeaderElectionNamespace,
			Name:      controllerName,
		},
		Client: runOptions.leaderElectionClient.CoreV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity:      id + fmt.Sprintf("-%s", controllerName),
			EventRecorder: createRecorder(runOptions.kubeClient),
		},
	}

	run := func(stopCh <-chan struct{}) {
		machineController := controller.NewMachineController(
			runOptions.kubeClient,
			runOptions.machineClient,
			runOptions.nodeInformer,
			runOptions.nodeLister,
			runOptions.configMapLister,
			runOptions.machineInformer,
			runOptions.machineLister,
			runOptions.sshKeyPair,
			runOptions.clusterDNSIPs,
			controller.MetricsCollection{
				Machines:            runOptions.metrics.Machines,
				Workers:             runOptions.metrics.Workers,
				Errors:              runOptions.metrics.Errors,
				Nodes:               runOptions.metrics.Nodes,
				ControllerOperation: runOptions.metrics.ControllerOperation,
				NodeJoinDuration:    runOptions.metrics.NodeJoinDuration,
			},
			runOptions.kubeconfigProvider)

		if err = machineController.Run(workerCount, stopCh); err != nil {
			glog.Fatalf("error running controller: %v", err)
		}
	}

	leaderelection.RunOrDie(leaderelection.LeaderElectionConfig{
		Lock:          &rl,
		LeaseDuration: defaultLeaderElectionLeaseDuration,
		RenewDeadline: defaultLeaderElectionRenewDeadline,
		RetryPeriod:   defaultLeaderElectionRetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: run,
			OnStoppedLeading: func() {
				glog.Fatalf("leaderelection lost, closing the app")
			},
		},
	})
}

// startUtilHttpServer starts a new HTTP server asynchronously
func startUtilHttpServerOrDie(kubeClient *kubernetes.Clientset, kubeconfigProvider controller.KubeconfigProvider, stopCh <-chan struct{}) {
	health := healthcheck.NewHandler()
	health.AddReadinessCheck("apiserver-connection", machinehealth.ApiserverReachable(kubeClient))

	for name, c := range readinessChecks(kubeconfigProvider) {
		health.AddReadinessCheck(name, c)
	}

	serveUtilHttpServer := func(health healthcheck.Handler) {
		m := http.NewServeMux()
		m.Handle("/metrics", promhttp.Handler())
		m.Handle("/live", http.HandlerFunc(health.LiveEndpoint))
		m.Handle("/ready", http.HandlerFunc(health.ReadyEndpoint))

		s := http.Server{
			Addr:    listenAddress,
			Handler: m,
		}
		glog.V(4).Infof("serving util http server on %s", listenAddress)
		glog.Fatalf("util http server died: %v", s.ListenAndServe())
	}

	go serveUtilHttpServer(health)
}

func readinessChecks(kubeconfigProvider controller.KubeconfigProvider) map[string]healthcheck.Check {
	return map[string]healthcheck.Check{
		"valid-info-kubeconfig": func() error {
			cm, err := kubeconfigProvider.GetKubeconfig()
			if err != nil {
				return err
			}
			if len(cm.Clusters) != 1 {
				err := errors.New("invalid kubeconfig: no clusters found")
				glog.V(2).Info(err)
				return err
			}
			for name, c := range cm.Clusters {
				if len(c.CertificateAuthorityData) == 0 {
					err := fmt.Errorf("invalid kubeconfig: no certificate authority data was specified for kuberconfig.clusters.['%s']", name)
					glog.V(2).Info(err)
					return err
				}
				if len(c.Server) == 0 {
					err := fmt.Errorf("invalid kubeconfig: no server was specified for kuberconfig.clusters.['%s']", name)
					glog.V(2).Info(err)
					return err
				}
			}
			return nil
		},
	}
}

// createRecorder creates a new event recorder which is later used by the leader election
// library to broadcast events
func createRecorder(kubeClient *kubernetes.Clientset) record.EventRecorder {
	glog.V(4).Info("creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.V(4).Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	return eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerName})
}

func parseClusterDNSIPs(s string) ([]net.IP, error) {
	var ips []net.IP
	sips := strings.Split(s, ",")
	for _, sip := range sips {
		ip := net.ParseIP(strings.TrimSpace(sip))
		if ip == nil {
			return nil, fmt.Errorf("unable to parse ip %s", sip)
		}
		ips = append(ips, ip)
	}
	return ips, nil
}
