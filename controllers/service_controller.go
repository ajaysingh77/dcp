// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/go-logr/logr"
	"github.com/smallnest/chanx"
	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	ctrl_event "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	ctrl_source "sigs.k8s.io/controller-runtime/pkg/source"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/internal/proxy"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

type proxyInstanceData struct {
	proxy     *proxy.Proxy
	stopProxy context.CancelFunc
}

type ServiceReconciler struct {
	ctrl_client.Client
	Log                 logr.Logger
	reconciliationSeqNo uint32
	ProcessExecutor     process.Executor
	ProxyConfigDir      string
	proxyData           *syncmap.Map[types.NamespacedName, []proxyInstanceData]

	// Channel used to trigger reconciliation function when underlying run status changes.
	notifyProxyRunChanged *chanx.UnboundedChan[ctrl_event.GenericEvent]

	// Debouncer used to schedule reconciliations. Extra data carried is the finished PID.
	debouncer *reconcilerDebouncer[process.Pid_t]

	lifetimeCtx context.Context
}

var (
	serviceFinalizer string = fmt.Sprintf("%s/service-reconciler", apiv1.GroupVersion.Group)
)

func NewServiceReconciler(lifetimeCtx context.Context, client ctrl_client.Client, log logr.Logger, processExecutor process.Executor) *ServiceReconciler {
	r := ServiceReconciler{
		Client:                client,
		Log:                   log,
		ProcessExecutor:       processExecutor,
		ProxyConfigDir:        filepath.Join(os.TempDir(), "usvc-servicecontroller-serviceconfig"),
		proxyData:             &syncmap.Map[types.NamespacedName, []proxyInstanceData]{},
		notifyProxyRunChanged: chanx.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx, 1),
		debouncer:             newReconcilerDebouncer[process.Pid_t](reconciliationDebounceDelay),
		lifetimeCtx:           lifetimeCtx,
	}
	return &r
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &apiv1.Endpoint{}, ".metadata.serviceNamespace", func(rawObj ctrl_client.Object) []string {
		endpoint := rawObj.(*apiv1.Endpoint)
		return []string{endpoint.Spec.ServiceNamespace}
	}); err != nil {
		r.Log.Error(err, "failed to create serviceNamespace index for Endpoint")
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &apiv1.Endpoint{}, ".metadata.serviceName", func(rawObj ctrl_client.Object) []string {
		endpoint := rawObj.(*apiv1.Endpoint)
		return []string{endpoint.Spec.ServiceName}
	}); err != nil {
		r.Log.Error(err, "failed to create serviceName index for Endpoint")
		return err
	}

	src := ctrl_source.Channel{
		Source: r.notifyProxyRunChanged.Out,
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1.Service{}).
		Watches(&apiv1.Endpoint{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForEndpoint), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(&src, &handler.EnqueueRequestForObject{}).
		Complete(r)
}

func (r *ServiceReconciler) requestReconcileForEndpoint(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	endpoint := obj.(*apiv1.Endpoint)
	serviceNamespaceName := types.NamespacedName{
		Namespace: endpoint.Spec.ServiceNamespace,
		Name:      endpoint.Spec.ServiceName,
	}

	r.Log.V(1).Info("endpoint updated, requesting service reconciliation", "Endpoint", endpoint, "ServiceName", serviceNamespaceName)
	return []reconcile.Request{
		{
			NamespacedName: serviceNamespaceName,
		},
	}
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("ServiceName", req.NamespacedName).WithValues("Reconciliation", atomic.AddUint32(&r.reconciliationSeqNo, 1))

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	svc := apiv1.Service{}
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		if apimachinery_errors.IsNotFound(err) {
			log.Info("the Service object does not exist yet or was deleted")
			r.stopAllProxies(req.NamespacedName, log)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to Get() the Service object")
			return ctrl.Result{}, err
		}
	}

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(svc.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if svc.DeletionTimestamp != nil && !svc.DeletionTimestamp.IsZero() {
		log.Info("Service object is being deleted")
		r.stopAllProxies(svc.NamespacedName(), log)
		change = deleteFinalizer(&svc, serviceFinalizer, log)
	} else {
		change = ensureFinalizer(&svc, serviceFinalizer, log)
		// If we added a finalizer, we'll do the additional reconciliation next call
		if change == noChange {
			change |= r.ensureServiceEffectiveAddressAndPort(ctx, &svc, log)
		}
	}

	result, err := saveChanges(r.Client, ctx, &svc, patch, change, nil, log)
	return result, err
}

func (r *ServiceReconciler) stopAllProxies(svcName types.NamespacedName, log logr.Logger) {
	if proxyData, ok := r.proxyData.LoadAndDelete(svcName); ok && len(proxyData) > 0 {
		log.Info("stopping all proxies...")
		for _, data := range proxyData {
			data.stopProxy()
		}
	}
}

func (r *ServiceReconciler) ensureServiceEffectiveAddressAndPort(ctx context.Context, svc *apiv1.Service, log logr.Logger) objectChange {
	oldPid := svc.Status.ProxyProcessPid
	oldState := svc.Status.State
	oldEffectiveAddress := svc.Status.EffectiveAddress
	oldEffectivePort := svc.Status.EffectivePort
	oldEndpointNamespacedName := types.NamespacedName{
		Namespace: svc.Status.ProxylessEndpointNamespace,
		Name:      svc.Status.ProxylessEndpointName,
	}

	change := noChange

	serviceEndpoints, err := r.getServiceEndpoints(ctx, svc, log)
	if err != nil {
		return additionalReconciliationNeeded
	}

	if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeProxyless {
		// If using Proxyless allocation mode
		if len(serviceEndpoints.Items) == 0 {
			// No Endpoints are available. Empty out the proxyless Endpoint namespace and name, and effective address and port.
			svc.Status.State = apiv1.ServiceStateNotReady
			svc.Status.ProxylessEndpointNamespace = ""
			svc.Status.ProxylessEndpointName = ""
			svc.Status.EffectiveAddress = ""
			svc.Status.EffectivePort = 0
		} else {
			// At least one Endpoint exists.
			svc.Status.State = apiv1.ServiceStateReady

			// If an Endpoint was previously chosen, we need to ensure it is still valid, and if not choose another.
			if svc.Status.ProxylessEndpointNamespace != "" && svc.Status.ProxylessEndpointName != "" {
				// Ensure the previously chosen Endpoint still exists
				endpointStillExists := slices.Any(serviceEndpoints.Items, func(endpoint apiv1.Endpoint) bool {
					return endpoint.ObjectMeta.Namespace == svc.Status.ProxylessEndpointNamespace && endpoint.ObjectMeta.Name == svc.Status.ProxylessEndpointName
				})

				if !endpointStillExists {
					svc.Status.ProxylessEndpointNamespace = ""
					svc.Status.ProxylessEndpointName = ""
					svc.Status.EffectiveAddress = ""
					svc.Status.EffectivePort = 0
				}
			}

			if svc.Status.ProxylessEndpointNamespace == "" || svc.Status.ProxylessEndpointName == "" {
				// No proxyless Endpoint has been chosen yet (or the chosen one no longer exists), so we need to choose one
				svc.Status.ProxylessEndpointNamespace = serviceEndpoints.Items[0].ObjectMeta.Namespace
				svc.Status.ProxylessEndpointName = serviceEndpoints.Items[0].ObjectMeta.Name
				svc.Status.EffectiveAddress = serviceEndpoints.Items[0].Spec.Address
				svc.Status.EffectivePort = serviceEndpoints.Items[0].Spec.Port
			}
		}
	} else {
		// Else using a regular allocation mode which will use a proxy

		svc.Status.State = apiv1.ServiceStateNotReady

		err := r.startProxyIfNeeded(ctx, svc, log)
		if err != nil {
			log.Error(err, "could not start the proxy")
			change |= additionalReconciliationNeeded
		} else {
			serviceProxyData, found := r.proxyData.Load(svc.NamespacedName())

			if found && len(serviceEndpoints.Items) > 0 {
				svc.Status.State = apiv1.ServiceStateReady

				config := proxy.ProxyConfig{
					Endpoints: []proxy.Endpoint{},
				}

				for _, endpoint := range serviceEndpoints.Items {
					config.Endpoints = append(config.Endpoints, proxy.Endpoint{
						Address: endpoint.Spec.Address,
						Port:    endpoint.Spec.Port,
					})
				}

				for _, proxyInstanceData := range serviceProxyData {
					err := proxyInstanceData.proxy.Configure(config)
					if err != nil {
						log.Error(err, "could not configure the proxy")
					}
				}
			}
		}
	}

	if svc.Status.ProxyProcessPid != oldPid {
		if svc.Status.ProxyProcessPid != apiv1.UnknownPID {
			log.Info(fmt.Sprintf("proxy process has been started for service %s (PID %d)", svc.NamespacedName(), *svc.Status.ProxyProcessPid))
		}
		change |= statusChanged
	}

	if svc.Status.State != oldState {
		log.Info(fmt.Sprintf("service %s is now in state %s", svc.NamespacedName(), svc.Status.State))
		change |= statusChanged
	}

	if svc.Status.EffectiveAddress != oldEffectiveAddress || svc.Status.EffectivePort != oldEffectivePort {
		log.Info(fmt.Sprintf("service %s is now running on %s:%d", svc.NamespacedName(), svc.Status.EffectiveAddress, svc.Status.EffectivePort))
		change |= statusChanged
	}

	if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeProxyless && (svc.Status.ProxylessEndpointNamespace != oldEndpointNamespacedName.Namespace || svc.Status.ProxylessEndpointName != oldEndpointNamespacedName.Name) {
		if svc.Status.EffectiveAddress != "" || svc.Status.EffectivePort != 0 {
			log.Info(fmt.Sprintf("service %s is now running on %s:%d", svc.NamespacedName(), svc.Status.EffectiveAddress, svc.Status.EffectivePort))
		}
		change |= statusChanged
	}

	return change
}

func (r *ServiceReconciler) getServiceEndpoints(ctx context.Context, svc *apiv1.Service, log logr.Logger) (apiv1.EndpointList, error) {
	var serviceEndpoints apiv1.EndpointList
	if err := r.List(ctx, &serviceEndpoints, ctrl_client.MatchingFields{".metadata.serviceNamespace": svc.ObjectMeta.Namespace}, ctrl_client.MatchingFields{".metadata.serviceName": svc.ObjectMeta.Name}); err != nil {
		log.Error(err, "could not get associated endpoints")
		return apiv1.EndpointList{}, fmt.Errorf("could not get associated endpoints: %w", err)
	} else {
		return serviceEndpoints, nil
	}
}

// startProxyIfNeeded starts a proxy process if needed for the given service.
// It returns the error if any.
func (r *ServiceReconciler) startProxyIfNeeded(ctx context.Context, svc *apiv1.Service, log logr.Logger) error {
	serviceProxyData, found := r.proxyData.Load(svc.NamespacedName())

	if found {
		// Make sure the status reflects the real world
		// We might bind to multiple addresses if the address specified by the spec is "localhost".
		// It should not matter which proxy we pick here, so we just pick the first one.
		// We do not want to use just "localhost", because then the port will be ambiguous.
		svc.Status.EffectivePort = serviceProxyData[0].proxy.EffectivePort
		svc.Status.EffectiveAddress = serviceProxyData[0].proxy.EffectiveAddress
		return nil
	}

	// Reset the overall status for the service
	r.proxyData.Delete(svc.NamespacedName())
	svc.Status.ProxyProcessPid = apiv1.UnknownPID
	svc.Status.EffectiveAddress = ""
	svc.Status.EffectivePort = 0

	proxyAddress := svc.Spec.Address
	if proxyAddress == "" {
		if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeLocalhost || svc.Spec.AddressAllocationMode == "" {
			proxyAddress = "localhost"
		} else if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeIPv4ZeroOne {
			proxyAddress = "127.0.0.1"
		} else if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeIPv4Loopback {
			proxyAddress = fmt.Sprintf("127.%d.%d.%d", rand.Intn(254)+1, rand.Intn(254)+1, rand.Intn(254)+1)
		} else if svc.Spec.AddressAllocationMode == apiv1.AddressAllocationModeIPv6ZeroOne {
			proxyAddress = "[::1]"
		} else {
			return fmt.Errorf("unsupported address allocation mode: %s", svc.Spec.AddressAllocationMode)
		}
	}

	var err error
	proxies := []proxyInstanceData{}
	var getProxyPort func(proxyAddress string) (int32, error)
	if svc.Spec.Port == 0 {
		getProxyPort = func(proxyAddress string) (int32, error) {
			return networking.GetFreePort(svc.Spec.Protocol, proxyAddress)
		}
	} else {
		getProxyPort = func(_ string) (int32, error) {
			return svc.Spec.Port, nil
		}
	}

	if proxyAddress == "localhost" {
		// Bind to all applicable IPs (IPv4 and IPv6) for the proxy address
		ips, err := net.LookupIP(proxyAddress)
		if err != nil {
			return fmt.Errorf("could not obtain IP address(es) for %s: %w", proxyAddress, err)
		}
		if len(ips) == 0 {
			return fmt.Errorf("could not obtain IP address(es) for %s", proxyAddress)
		}

		for _, ip := range ips {
			var proxyInstanceAddress string

			if ip4 := ip.To4(); len(ip4) == net.IPv4len {
				proxyInstanceAddress = ip4.String()
			} else if ip6 := ip.To16(); len(ip6) == net.IPv6len {
				proxyInstanceAddress = fmt.Sprintf("[%s]", ip6.String())
			} else {
				// Fallback to just the IP address as a raw string
				proxyInstanceAddress = ip.String()
			}

			proxyPort, portAllocationErr := getProxyPort(proxyInstanceAddress)

			if portAllocationErr != nil {
				err = errors.Join(err, portAllocationErr)
			} else {
				proxyCtx, cancelFunc := context.WithCancel(r.lifetimeCtx)
				proxies = append(proxies, proxyInstanceData{
					proxy:     proxy.NewProxy(svc.Spec.Protocol, proxyInstanceAddress, proxyPort, proxyCtx, log),
					stopProxy: cancelFunc,
				})
			}
		}
	} else {
		// Bind to just the proxy address

		proxyPort, portAllocationErr := getProxyPort(proxyAddress)
		if portAllocationErr != nil {
			err = portAllocationErr
		} else {
			proxyCtx, cancelFunc := context.WithCancel(r.lifetimeCtx)
			proxies = append(proxies, proxyInstanceData{
				proxy:     proxy.NewProxy(svc.Spec.Protocol, proxyAddress, proxyPort, proxyCtx, log),
				stopProxy: cancelFunc,
			})
		}
	}

	stopAllProxies := func() {
		for _, proxyInstanceData := range proxies {
			proxyInstanceData.stopProxy()
		}
	}

	if err != nil {
		stopAllProxies()
		return fmt.Errorf("cound not create the proxy for the service: %w", err)
	}

	for _, proxyInstanceData := range proxies {
		err := proxyInstanceData.proxy.Start()
		if err != nil {
			stopAllProxies()
			return fmt.Errorf("cound not start the proxy for the service: %w", err)
		}
	}

	r.proxyData.Store(svc.NamespacedName(), proxies)

	// See the comment at the beginning of the method for why we pick the first proxy
	// to report the effective address and port.
	svc.Status.EffectivePort = serviceProxyData[0].proxy.EffectivePort
	svc.Status.EffectiveAddress = serviceProxyData[0].proxy.EffectiveAddress
	r.Log.Info("service proxy started",
		"EffectiveAddress", svc.Status.EffectiveAddress,
		"EffectivePort", svc.Status.EffectivePort,
	)

	return nil
}
