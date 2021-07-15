/*
Copyright 2021.

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

package controllers

import (
	"context"
	"net"
	"strings"

	"github.com/go-logr/logr"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/signer"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/instances"
	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
)

//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=configmaps/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=configmaps/finalizers,verbs=update

const (
	// BYOHAnnotation is an an anotation that should be applied to all Windows nodes not associated with a Machine.
	BYOHAnnotation = "windowsmachineconfig.openshift.io/byoh"
	// UsernameAnnotation is a node annotation that contains the username used to log into the Windows instance
	UsernameAnnotation = "windowsmachineconfig.openshift.io/username"
	// InstanceConfigMap is the name of the ConfigMap where VMs to be configured should be described.
	InstanceConfigMap = "windows-instances"
)

// ConfigMapReconciler reconciles a ConfigMap object
type ConfigMapReconciler struct {
	instanceReconciler
}

// NewConfigMapReconciler returns a pointer to a ConfigMapReconciler
func NewConfigMapReconciler(mgr manager.Manager, clusterConfig cluster.Config, watchNamespace string) (*ConfigMapReconciler, error) {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, errors.Wrap(err, "error creating kubernetes clientset")
	}

	// Initialize prometheus configuration
	pc, err := metrics.NewPrometheusNodeConfig(clientset, watchNamespace)
	if err != nil {
		return nil, errors.Wrap(err, "unable to initialize Prometheus configuration")
	}
	return &ConfigMapReconciler{
		instanceReconciler: instanceReconciler{
			client:               mgr.GetClient(),
			k8sclientset:         clientset,
			clusterServiceCIDR:   clusterConfig.Network().GetServiceCIDR(),
			log:                  ctrl.Log.WithName("controllers").WithName("ConfigMap"),
			watchNamespace:       watchNamespace,
			recorder:             mgr.GetEventRecorderFor("configmap"),
			vxlanPort:            clusterConfig.Network().VXLANPort(),
			prometheusNodeConfig: pc,
		},
	}, nil
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.2/pkg/reconcile
func (r *ConfigMapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = r.log.WithValues("configmap", req.NamespacedName)

	// Fetch the ConfigMap. The predicate will have filtered out any ConfigMaps that we should not reconcile
	// so it is safe to assume that all ConfigMaps being reconciled describe hosts that need to be present in the
	// cluster. This also handles the case when the reconciliation is kicked off by the InstanceConfigMap being deleted.
	// In the deletion case, an empty InstanceConfigMap will be reconciled now resulting in all existing BYOH nodes
	// being deleted.
	configMap, err := r.ensure(context.TODO(), req.NamespacedName)
	if err != nil {
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.reconcileNodes(ctx, configMap)
}

// parseHosts gets the lists of hosts specified in the configmap's data
func (r *ConfigMapReconciler) parseHosts(configMapData map[string]string) ([]*instances.InstanceInfo, error) {
	hosts := make([]*instances.InstanceInfo, 0)
	// Get information about the hosts from each entry. The expected key/value format for each entry is:
	// <address>: username=<username>
	for address, data := range configMapData {
		if err := validateAddress(address); err != nil {
			return nil, errors.Wrapf(err, "invalid address %s", address)
		}
		splitData := strings.SplitN(data, "=", 2)
		if len(splitData) == 0 || splitData[0] != "username" {
			return hosts, errors.Errorf("data for entry %s has an incorrect format", address)
		}

		hosts = append(hosts, instances.NewInstanceInfo(address, splitData[1], ""))
	}
	return hosts, nil
}

// validateAddress checks that the given address is either an ipv4 address, or resolves to any ip address
func validateAddress(address string) error {
	// first check if address is an IP address
	if parsedAddr := net.ParseIP(address); parsedAddr != nil {
		if parsedAddr.To4() != nil {
			return nil
		}
		// if the address parses into an IP but is not ipv4 it must be ipv6
		return errors.Errorf("ipv6 is not supported")
	}
	// Do a check that the DNS provided is valid
	addressList, err := net.LookupHost(address)
	if err != nil {
		return errors.Wrapf(err, "error looking up DNS")
	}
	if len(addressList) == 0 {
		return errors.Errorf("DNS did not resolve to an address")
	}
	return nil
}

// reconcileNodes corrects the discrepancy between the "expected" hosts slice, and the "actual" nodelist
func (r *ConfigMapReconciler) reconcileNodes(ctx context.Context, instances *core.ConfigMap) error {
	var err error
	// Get the list of instances that are expected to be Nodes
	hosts, err := r.parseHosts(instances.Data)
	if err != nil {
		return errors.Wrapf(err, "unable to parse hosts from configmap")
	}

	nodes := &core.NodeList{}
	// Why are we not doing r.client.List(ctx, nodes, []client.ListOption{client.MatchingLabels{core.LabelOSStable: "=windows"}}...)?
	if err := r.client.List(ctx, nodes); err != nil {
		return errors.Wrap(err, "error listing nodes")
	}

	var byohNodes []core.Node
	for _, node := range nodes.Items {
		if node.GetAnnotations()[BYOHAnnotation] == "true" {
			byohNodes = append(byohNodes, node)
		}
	}

	// No instances are present in InstanceConfigMap and no Nodes are present in the cluster which implies that we don't
	// need to do any reconciliation
	if len(hosts) == 0 && len(byohNodes) == 0 {
		return nil
	}

	// Create a new signer using the private key that the instances will be configured with
	r.signer, err = signer.Create(kubeTypes.NamespacedName{Namespace: r.watchNamespace,
		Name: secrets.PrivateKeySecret}, r.client)
	if err != nil {
		return errors.Wrapf(err, "unable to create signer from private key secret")
	}

	// For each host, ensure that it is configured into a node. On error of any host joining, return error and requeue.
	// It is better to return early like this, instead of trying to configure as many nodes as possible in a single
	// reconcile call, as it simplifies error collection. The order the map is read from is psuedo-random, so the
	// configuration effort for configurable hosts will not be blocked by a specific host that has issues with
	// configuration.
	for _, host := range hosts {
		err := r.ensureInstanceIsConfigured(host, nodes)
		if err != nil {
			r.recorder.Eventf(instances, core.EventTypeWarning, "InstanceSetupFailure",
				"unable to join instance with address %s to the cluster", host.Address)
			return errors.Wrapf(err, "error configuring host with address %s", host.Address)
		}
	}

	// Ensure that only instances currently specified by the ConfigMap are joined to the cluster as nodes
	if err = r.deconfigureInstances(hosts, nodes); err != nil {
		return errors.Wrap(err, "error removing undesired nodes from cluster")
	}

	// Once all the proper Nodes are in the cluster, configure the prometheus endpoints.
	if err := r.prometheusNodeConfig.Configure(); err != nil {
		return errors.Wrap(err, "unable to configure Prometheus")
	}
	return nil
}

// ensureInstanceIsConfigured ensures that the given instance has an associated Node
func (r *ConfigMapReconciler) ensureInstanceIsConfigured(instance *instances.InstanceInfo, nodes *core.NodeList) error {
	node, found := findNode(instance.Address, nodes)
	if found {
		// Version annotation being present means that the node has been fully configured
		if _, present := node.Annotations[nodeconfig.VersionAnnotation]; present {
			// TODO: Check version for upgrade case https://issues.redhat.com/browse/WINC-580 and remove and re-add the node
			//       if needed. Possibly also do this if the node is not in the `Ready` state.
			return nil
		}
	}

	if err := r.configureInstance(instance, map[string]string{BYOHAnnotation: "true",
		UsernameAnnotation: instance.Username}); err != nil {
		return errors.Wrap(err, "error configuring node")
	}

	return nil
}

// deconfigureInstances removes all BYOH nodes that are not specified in the given instances slice, and
// deconfigures the instances associated with them.
func (r *ConfigMapReconciler) deconfigureInstances(instances []*instances.InstanceInfo, nodes *core.NodeList) error {
	for _, node := range nodes.Items {
		// Only looking at BYOH nodes
		if _, present := node.Annotations[BYOHAnnotation]; !present {
			continue
		}
		// Check for instances associated with this node
		if hasEntry := hasAssociatedInstance(&node, instances); hasEntry {
			continue
		}
		// no instance found in the provided list, remove the node from the cluster
		if err := r.deconfigureInstance(&node); err != nil {
			return errors.Wrapf(err, "unable to deconfigure instance with node %s", node.GetName())
		}
	}
	return nil
}

// findNode returns a pointer to the node with an address matching the given address and a bool indicating if the node
// was found or not.
func findNode(address string, nodes *core.NodeList) (*core.Node, bool) {
	for _, node := range nodes.Items {
		for _, nodeAddress := range node.Status.Addresses {
			if address == nodeAddress.Address {
				return &node, true
			}
		}
	}
	return nil, false
}

// hasAssociatedInstance returns true if the given node is associated with an instance in the given slice
func hasAssociatedInstance(node *core.Node, instances []*instances.InstanceInfo) bool {
	for _, instance := range instances {
		for _, nodeAddress := range node.Status.Addresses {
			if instance.Address == nodeAddress.Address {
				return true
			}
		}
	}
	return false
}

// mapToConfigMap fulfills the MapFn type, while always returning a request to the windows-instance ConfigMap
func (r *ConfigMapReconciler) mapToConfigMap(_ client.Object) []reconcile.Request {
	return []reconcile.Request{{
		NamespacedName: kubeTypes.NamespacedName{Namespace: r.watchNamespace, Name: InstanceConfigMap},
	}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConfigMapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	configMapPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return r.isValidConfigMap(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return r.isValidConfigMap(e.ObjectNew)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// If DeleteStateUnknown is true it implies that the Delete event was missed  and we can ignore it
			if e.DeleteStateUnknown {
				return false
			}
			return r.isValidConfigMap(e.Object)
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&core.ConfigMap{}, builder.WithPredicates(configMapPredicate)).
		Watches(&source.Kind{Type: &core.Node{}}, handler.EnqueueRequestsFromMapFunc(r.mapToConfigMap),
			builder.WithPredicates(windowsNodePredicate(true))).
		Complete(r)
}

// isValidConfigMap returns true if the ConfigMap object is the InstanceConfigMap
func (r *ConfigMapReconciler) isValidConfigMap(o client.Object) bool {
	return o.GetNamespace() == r.watchNamespace && o.GetName() == InstanceConfigMap
}

// ensure returns the InstanceConfigMap if present. If it is not present, it creates an empty one and returns
// it.
func (r *ConfigMapReconciler) ensure(ctx context.Context, namespacedName kubeTypes.NamespacedName) (
	*core.ConfigMap, error) {
	windowsInstances := &core.ConfigMap{}
	var err error
	if err = r.client.Get(ctx, namespacedName, windowsInstances); err != nil {
		if k8sapierrors.IsNotFound(err) {
			windowsInstances.SetNamespace(namespacedName.Namespace)
			windowsInstances.SetName(namespacedName.Name)
			if err = r.client.Create(ctx, windowsInstances); err != nil {
				return nil, err
			}
			r.log.Info("Created", "ConfigMap", namespacedName)
			if err = r.client.Get(ctx, namespacedName, windowsInstances); err != nil {
				return nil, err
			}
		}
	}
	return windowsInstances, err
}

// EnsureWindowsInstancesConfigMap ensures that the InstanceConfigMap is present on the cluster during operator bootup.
// ConfigMapReconciler.ensure() cannot be called in its stead as the cache has not been populated yet, which is
// why the typed client is used here as it calls the API server directly.
func EnsureWindowsInstancesConfigMap(log logr.Logger, cfg *rest.Config, namespace string) error {
	if cfg == nil {
		return errors.New("config should not be nil")
	}

	var err error
	k8sClientSet, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return errors.Wrap(err, "error creating config client")
	}

	windowsInstances := &core.ConfigMap{}
	if windowsInstances, err = k8sClientSet.CoreV1().ConfigMaps(namespace).Get(context.TODO(), InstanceConfigMap,
		meta.GetOptions{}); err != nil {
		if k8sapierrors.IsNotFound(err) {
			windowsInstances.SetNamespace(namespace)
			windowsInstances.SetName(InstanceConfigMap)
			if _, err = k8sClientSet.CoreV1().ConfigMaps(namespace).Create(context.TODO(), windowsInstances,
				meta.CreateOptions{}); err != nil {
				return err
			}
			log.Info("Created", "ConfigMap", kubeTypes.NamespacedName{Namespace: namespace,
				Name: InstanceConfigMap})
		}
	}
	return err
}
