// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	apimachinery_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	controller "sigs.k8s.io/controller-runtime/pkg/controller"
	ctrl_event "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrl_handler "sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	ctrl_source "sigs.k8s.io/controller-runtime/pkg/source"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/dcptun"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type tunnelProxyStateInitializerFunc = stateInitializerFunc[
	apiv1.ContainerNetworkTunnelProxy, *apiv1.ContainerNetworkTunnelProxy,
	ContainerNetworkTunnelProxyReconciler, *ContainerNetworkTunnelProxyReconciler,
	apiv1.ContainerNetworkTunnelProxyState,
	containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData,
]

// In case of ContainerNetworkTunnelProxy, the "state key" for its ObjectStateMap is the ContainerNetworkTunnelProxy's namespaced name;
// we do not use the state key for manipulating the tunnel proxy data, but it must be unique for each tunnel proxy.
type tunnelProxyDataMap = ObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData, *containerNetworkTunnelProxyData]

const (
	tunnelProxyContainerNameProperty = ".spec.containerNetworkName"
)

var (
	tunnelProxyFinalizer string = fmt.Sprintf("%s/tunnel-proxy-reconciler", apiv1.GroupVersion.Group)

	tunnelProxyStateInitializers = map[apiv1.ContainerNetworkTunnelProxyState]tunnelProxyStateInitializerFunc{
		apiv1.ContainerNetworkTunnelProxyStateEmpty:         handleNewTunnelProxy,
		apiv1.ContainerNetworkTunnelProxyStatePending:       handleNewTunnelProxy,
		apiv1.ContainerNetworkTunnelProxyStateBuildingImage: ensureTunnelProxyBuildingImageState,
		apiv1.ContainerNetworkTunnelProxyStateStarting:      ensureTunnelProxyStartingState,
		apiv1.ContainerNetworkTunnelProxyStateRunning:       ensureTunnelProxyRunningState,
		apiv1.ContainerNetworkTunnelProxyStateFailed:        ensureTunnelProxyFailedState,
	}
)

type ContainerNetworkTunnelProxyReconciler struct {
	ctrl_client.Client
	log                 logr.Logger
	reconciliationSeqNo uint32

	// Reconciler lifetime context.
	lifetimeCtx context.Context

	// Orchestrator used to manage containers.
	orchestrator containers.ContainerOrchestrator

	// In-memory state map for ContainerNetworkTunnelProxy objects.
	proxyData *tunnelProxyDataMap

	// A work queue for long-running operations.
	workQueue *resiliency.WorkQueue

	// Debouncer for scheduling reconciliations.
	debouncer *reconcilerDebouncer[struct{}]

	// A channel used to trigger reconciliations.
	reconciliationTriggerChan *concurrency.UnboundedChan[ctrl_event.GenericEvent]
}

func NewContainerNetworkTunnelProxyReconciler(
	lifetimeCtx context.Context,
	client ctrl_client.Client,
	orchestrator containers.ContainerOrchestrator,
	log logr.Logger,
) *ContainerNetworkTunnelProxyReconciler {
	return &ContainerNetworkTunnelProxyReconciler{
		Client:                    client,
		log:                       log,
		lifetimeCtx:               lifetimeCtx,
		orchestrator:              orchestrator,
		proxyData:                 NewObjectStateMap[types.NamespacedName, containerNetworkTunnelProxyData](),
		workQueue:                 resiliency.NewWorkQueue(lifetimeCtx, resiliency.DefaultConcurrency),
		debouncer:                 newReconcilerDebouncer[struct{}](),
		reconciliationTriggerChan: concurrency.NewUnboundedChan[ctrl_event.GenericEvent](lifetimeCtx),
	}
}

func (r *ContainerNetworkTunnelProxyReconciler) SetupWithManager(mgr ctrl.Manager, name string) error {
	// Setup a client-side index to allow quickly finding all ContainerNetworkTunnelProxies referencing a specific ContainerNetwork.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &apiv1.ContainerNetworkTunnelProxy{}, tunnelProxyContainerNameProperty, func(rawObj ctrl_client.Object) []string {
		cntp := rawObj.(*apiv1.ContainerNetworkTunnelProxy)
		if cntp.Spec.ContainerNetworkName == "" {
			return nil
		} else {
			return []string{cntp.Spec.ContainerNetworkName}
		}
	}); err != nil {
		r.log.Error(err, "Failed to create index for ContainerNetworkTunnelProxy.Spec.ContainerNetworkName field")
		return err
	}

	src := ctrl_source.Channel(r.reconciliationTriggerChan.Out, &ctrl_handler.EnqueueRequestForObject{})

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: MaxConcurrentReconciles}).
		For(&apiv1.ContainerNetworkTunnelProxy{}).
		Watches(&apiv1.ContainerNetwork{}, handler.EnqueueRequestsFromMapFunc(r.requestReconcileForContainerNetworkTunnelProxy), builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		WatchesRawSource(src).
		Named(name).
		Complete(r)
}

func (r *ContainerNetworkTunnelProxyReconciler) requestReconcileForContainerNetworkTunnelProxy(ctx context.Context, obj ctrl_client.Object) []reconcile.Request {
	network := obj.(*apiv1.ContainerNetwork)

	// Find all ContainerNetworkTunnelProxies that reference this ContainerNetwork
	var tunnelProxies apiv1.ContainerNetworkTunnelProxyList
	listOpts := []ctrl_client.ListOption{
		ctrl_client.MatchingFields{tunnelProxyContainerNameProperty: network.Name},
	}

	if err := r.List(ctx, &tunnelProxies, listOpts...); err != nil {
		r.log.Error(err, "Failed to list ContainerNetworkTunnelProxies for ContainerNetwork", "ContainerNetwork", network.Name)
		return nil
	}

	requests := make([]reconcile.Request, len(tunnelProxies.Items))
	for i, tunnelProxy := range tunnelProxies.Items {
		requests[i] = reconcile.Request{
			NamespacedName: tunnelProxy.NamespacedName(),
		}
	}

	if len(requests) > 0 {
		r.log.V(1).Info("Enqueuing ContainerNetworkTunnelProxy reconciliation requests due to ContainerNetwork change",
			"ContainerNetwork", network.Name,
			"AffectedTunnelProxies", slices.Map[reconcile.Request, string](
				requests, func(req reconcile.Request) string { return req.NamespacedName.String() },
			),
		)
	}

	return requests
}

func (r *ContainerNetworkTunnelProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.log.WithValues(
		"ContainerNetworkTunnelProxy", req.NamespacedName.String(),
		"Reconciliation", atomic.AddUint32(&r.reconciliationSeqNo, 1),
	)

	if ctx.Err() != nil {
		log.V(1).Info("Request context expired, nothing to do...")
		return ctrl.Result{}, nil
	}

	tproxy := apiv1.ContainerNetworkTunnelProxy{}
	err := r.Get(ctx, req.NamespacedName, &tproxy)

	if err != nil {
		if apimachinery_errors.IsNotFound(err) {
			log.V(1).Info("ContainerNetworkTunnelProxy object was not found")
			getNotFoundCounter.Add(ctx, 1)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "Failed to Get() the ContainerNetworkTunnelProxy object")
			getFailedCounter.Add(ctx, 1)
			return ctrl.Result{}, err
		}
	} else {
		getSucceededCounter.Add(ctx, 1)
	}

	r.proxyData.RunDeferredOps(req.NamespacedName)

	var change objectChange
	patch := ctrl_client.MergeFromWithOptions(tproxy.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})

	if tproxy.DeletionTimestamp != nil && !tproxy.DeletionTimestamp.IsZero() {
		log.Info("ContainerNetworkTunnelProxy object is being deleted")
		r.releaseProxyResources(ctx, &tproxy, log)
		change = deleteFinalizer(&tproxy, tunnelProxyFinalizer, log)
	} else {
		change = ensureFinalizer(&tproxy, tunnelProxyFinalizer, log)
		if change == noChange {
			change = r.manageTunnelProxy(ctx, &tproxy, log)
		}
	}

	result, err := saveChanges(r.Client, ctx, &tproxy, patch, change, nil, log)
	return result, err
}

func (r *ContainerNetworkTunnelProxyReconciler) releaseProxyResources(_ context.Context, _ *apiv1.ContainerNetworkTunnelProxy, log logr.Logger) {
	// TODO: for now, we just log the cleanup. Later phases will implement actual cleanup.
	log.V(1).Info("Cleaning up ContainerNetworkTunnelProxy resources")
}

func (r *ContainerNetworkTunnelProxyReconciler) manageTunnelProxy(ctx context.Context, tunnelProxy *apiv1.ContainerNetworkTunnelProxy, log logr.Logger) objectChange {
	targetProxyState := tunnelProxy.Status.State
	_, pd := r.proxyData.BorrowByNamespacedName(tunnelProxy.NamespacedName())
	if pd != nil {
		targetProxyState = pd.State
	}

	initializer := getStateInitializer(tunnelProxyStateInitializers, targetProxyState, log)
	change := initializer(ctx, r, tunnelProxy, targetProxyState, pd, log)

	if pd != nil {
		r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
	}

	return change
}

func (r *ContainerNetworkTunnelProxyReconciler) setTunnelProxyState(tproxy *apiv1.ContainerNetworkTunnelProxy, state apiv1.ContainerNetworkTunnelProxyState) objectChange {
	change := noChange

	if tproxy.Status.State != state {
		tproxy.Status.State = state
		change = statusChanged
	}

	return change
}

// STATE INITIALIZER FUNCTIONS

func handleNewTunnelProxy(
	ctx context.Context,
	r *ContainerNetworkTunnelProxyReconciler,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	_ apiv1.ContainerNetworkTunnelProxyState,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	containerNetworkName := commonapi.AsNamespacedName(tunnelProxy.Spec.ContainerNetworkName, tunnelProxy.Namespace)
	containerNetwork := apiv1.ContainerNetwork{}
	tryAgain := false
	err := r.Get(ctx, containerNetworkName, &containerNetwork)

	switch {
	case apimachinery_errors.IsNotFound(err):
		tryAgain = true
		log.V(1).Info("Referenced ContainerNetwork not found", "ContainerNetwork", containerNetworkName.String())

	case err != nil:
		tryAgain = true
		log.Error(err, "Failed to get referenced ContainerNetwork", "ContainerNetwork", containerNetworkName.String())

	case containerNetwork.Status.State != apiv1.ContainerNetworkStateRunning || containerNetwork.Status.ID == "":
		tryAgain = true
		log.V(1).Info("Referenced ContainerNetwork is not in Running state",
			"ContainerNetwork", containerNetworkName.String(),
			"NetworkState", containerNetwork.Status.State,
			"NetworkID", containerNetwork.Status.ID)
	}

	if tryAgain {
		change := r.setTunnelProxyState(tunnelProxy, apiv1.ContainerNetworkTunnelProxyStatePending)
		return change | additionalReconciliationNeeded
	}

	return r.setTunnelProxyState(tunnelProxy, apiv1.ContainerNetworkTunnelProxyStateBuildingImage)
}

func ensureTunnelProxyBuildingImageState(
	ctx context.Context,
	r *ContainerNetworkTunnelProxyReconciler,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	_ apiv1.ContainerNetworkTunnelProxyState,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	change := noChange

	if pd == nil {
		log.V(1).Info("Making sure the container proxy image is up to date...")
		pd = &containerNetworkTunnelProxyData{
			ContainerNetworkTunnelProxyStatus: apiv1.ContainerNetworkTunnelProxyStatus{
				State: apiv1.ContainerNetworkTunnelProxyStateBuildingImage,
			},
		}

		startImgCheckErr := r.workQueue.Enqueue(r.ensureContainerProxyImage(tunnelProxy, pd.Clone(), log))
		if startImgCheckErr != nil {
			log.Error(startImgCheckErr, "Container image check for container network tunnel could not be queued, possibly because the workload is shutting down")
			change |= additionalReconciliationNeeded
		}

		r.proxyData.Store(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
	}

	// Regardless whether we just scheduled an image check, or it has been going for a while,
	// we need to ensure that the object state is correct.
	return change | pd.applyTo(tunnelProxy)
}

func ensureTunnelProxyStartingState(
	ctx context.Context,
	r *ContainerNetworkTunnelProxyReconciler,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	_ apiv1.ContainerNetworkTunnelProxyState,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	change := noChange

	if pd == nil { // Should never happen when we reach this state
		log.Error(fmt.Errorf("the data about ContainerNetworkTunnelProxy object is missing"), "",
			"CurrentState", apiv1.ContainerNetworkTunnelProxyStateStarting,
		)
		return r.setTunnelProxyState(tunnelProxy, apiv1.ContainerNetworkTunnelProxyStateFailed)
	}

	if !pd.startupScheduled {
		log.V(1).Info("Starting tunnel proxy...")

		startupErr := r.workQueue.Enqueue(r.startProxyPair(tunnelProxy, pd.Clone(), log))
		if startupErr != nil {
			log.Error(startupErr, "Failed to start tunnel proxy pair, possibly because the workload is shutting down")
			change |= additionalReconciliationNeeded
		} else {
			pd.startupScheduled = true
			_ = r.proxyData.Update(tunnelProxy.NamespacedName(), tunnelProxy.NamespacedName(), pd)
		}
	}

	return change | pd.applyTo(tunnelProxy)
}

func ensureTunnelProxyRunningState(
	ctx context.Context,
	r *ContainerNetworkTunnelProxyReconciler,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	_ apiv1.ContainerNetworkTunnelProxyState,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	// TODO
	return pd.applyTo(tunnelProxy)
}

func ensureTunnelProxyFailedState(
	ctx context.Context,
	r *ContainerNetworkTunnelProxyReconciler,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	_ apiv1.ContainerNetworkTunnelProxyState,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) objectChange {
	// TODO
	return pd.applyTo(tunnelProxy)
}

// INITIALIZATION AND SHUTDOWN HELPER METHODS

// Returns a function that ensures the container proxy image is up to date.
// The method is called as part of the reconciliation loop, but the returned function is executed asynchronously.
// The passed proxy data is a clone independent from what is stored in r.proxyData map.
func (r *ContainerNetworkTunnelProxyReconciler) ensureContainerProxyImage(
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) func(context.Context) {
	return func(ctx context.Context) {
		opts := dcptun.BuildClientProxyImageOptions{
			// TODO: set StreamCommandOptions here to capture the logs of the image build process
		}

		image, imageCheckErr := dcptun.EnsureClientProxyImage(ctx, opts, r.orchestrator, log)
		if imageCheckErr != nil {
			log.Error(imageCheckErr, "Container image check for container network tunnel could not be queued")
			pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		} else {
			log.V(1).Info("Container image check for container network tunnel completed successfully", "Image", image)
			pd.State = apiv1.ContainerNetworkTunnelProxyStateStarting
			pd.ClientProxyContainerImage = image
		}

		nn := tunnelProxy.NamespacedName()
		r.proxyData.QueueDeferredOp(nn, func(_ types.NamespacedName, _ types.NamespacedName) {
			r.proxyData.Update(nn, nn, pd)
		})
		r.scheduleReconciliation(nn)
	}
}

// Returns a function that starts the tunnel proxy pair.
// The method is called as part of the reconciliation loop, but the returned function is executed asynchronously.
// The passed proxy data is a clone independent from what is stored in r.proxyData map.
func (r *ContainerNetworkTunnelProxyReconciler) startProxyPair(
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) func(context.Context) {
	return func(ctx context.Context) {
		clientCtrCreated, reconciliationKind := r.startClientProxy(ctx, tunnelProxy, pd, log)

		// TODO: if client proxy container is created successfully, create server proxy too.
		// Only transition to Running state if both client and server proxies are created successfully.

		if clientCtrCreated {
			log.V(1).Info("Client proxy container started successfully, scheduling reconciliation")
			pd.State = apiv1.ContainerNetworkTunnelProxyStateRunning
		}

		if reconciliationKind == reconciliationKindDelayed {
			delay := defaultAdditionalReconciliationDelay + time.Duration(mathrand.Int63n(int64(defaultAdditionalReconciliationJitter)))
			time.Sleep(delay)
		}

		nn := tunnelProxy.NamespacedName()
		r.proxyData.QueueDeferredOp(nn, func(_ types.NamespacedName, _ types.NamespacedName) {
			r.proxyData.Update(nn, nn, pd)
		})
		r.scheduleReconciliation(nn)
	}
}

type reconciliationKind uint8

const (
	reconciliationKindImmediate reconciliationKind = 0
	reconciliationKindDelayed   reconciliationKind = 1
)

// Starts the client proxy container.
// The passed containerNetworkTunnelProxy data will be updated, reflecting success or failure of the client proxy start.
// In either case the caller should schedule a reconciliation of the given tunnel proxy object.
// Return value indicates whether the start was successful or not,
// and whether the reconciliation should be scheduled immediately, or after a delay.
func (r *ContainerNetworkTunnelProxyReconciler) startClientProxy(
	ctx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) (bool, reconciliationKind) {
	clientProxyCtrName, _, nameErr := MakeUniqueName(tunnelProxy.Name)
	if nameErr != nil {
		// This would be quite unusual and mean the random number generator failed.
		log.Error(nameErr, "Failed to create a unique name for the client proxy container")
		pd.startupScheduled = false // Reset startupScheduled flag as means of forcing a retry after potentially transient error.
		return false, reconciliationKindDelayed
	}

	containerNetworkName := commonapi.AsNamespacedName(tunnelProxy.Spec.ContainerNetworkName, tunnelProxy.Namespace)
	containerNetwork := apiv1.ContainerNetwork{}
	cnErr := r.Get(ctx, containerNetworkName, &containerNetwork)
	if cnErr != nil {
		log.Error(cnErr, "Failed to retrieve ContainerNetwork data necessary for starting the client proxy container")
		pd.startupScheduled = false
		return false, reconciliationKindDelayed
	}
	if containerNetwork.Status.State != apiv1.ContainerNetworkStateRunning || containerNetwork.Status.ID == "" {
		log.V(1).Info("Referenced ContainerNetwork is not in Running state, cannot start the client proxy container")
		pd.startupScheduled = false
		return false, reconciliationKindDelayed
	}

	log.V(1).Info("Starting client proxy container...")
	createOpts := containers.CreateContainerOptions{
		ContainerSpec: apiv1.ContainerSpec{
			Image:   pd.ClientProxyContainerImage,
			Command: dcptun.ClientProxyBinaryPath,
			Args: []string{
				"client",
				"--client-control-address", networking.IPv4AllInterfaceAddress,
				"--client-control-port", strconv.Itoa(dcptun.DefaultContainerProxyControlPort),
				"--client-data-address", networking.IPv4AllInterfaceAddress,
				"--client-data-port", strconv.Itoa(dcptun.DefaultContainerProxyDataPort),
			},
			Ports: []apiv1.ContainerPort{
				{ContainerPort: dcptun.DefaultContainerProxyControlPort},
				{ContainerPort: dcptun.DefaultContainerProxyDataPort},
			},
		},
		Name:    clientProxyCtrName,
		Network: containerNetwork.Status.ID,
	}
	created, createErr := createContainer(ctx, r.orchestrator, createOpts)
	if createErr != nil {
		log.Error(createErr, "Failed to create client proxy container")
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false, reconciliationKindImmediate
	}

	pd.ClientProxyContainerID = created.Id
	cleanupContainer := false
	defer func() {
		if !cleanupContainer {
			return
		}
		removeCtx, removeCtxCancel := context.WithTimeout(ctx, 5*time.Second)
		defer removeCtxCancel()
		_ = removeContainer(removeCtx, r.orchestrator, created.Id) // Best effort
	}()

	started, startErr := startContainer(ctx, r.orchestrator, clientProxyCtrName, created.Id, containers.StreamCommandOptions{})
	if startErr != nil {
		log.Error(startErr, "Failed to start client proxy container")
		cleanupContainer = true
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false, reconciliationKindImmediate
	}

	_, controlEndpointHostPort, controlEndpointErr := getHostAddressAndPortForContainerPort(
		createOpts.ContainerSpec, dcptun.DefaultContainerProxyControlPort, started, log,
	)
	if controlEndpointErr != nil {
		// Error already logged
		cleanupContainer = true
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false, reconciliationKindImmediate
	}

	_, dataEndpointHostPort, dataEndpointErr := getHostAddressAndPortForContainerPort(
		createOpts.ContainerSpec, dcptun.DefaultContainerProxyDataPort, started, log,
	)
	if dataEndpointErr != nil {
		// Error already logged
		cleanupContainer = true
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false, reconciliationKindImmediate
	}

	pd.ClientProxyControlPort = controlEndpointHostPort
	pd.ClientProxyDataPort = dataEndpointHostPort
	return true, reconciliationKindImmediate
}

func (r ContainerNetworkTunnelProxyReconciler) scheduleReconciliation(tunnelProxyName types.NamespacedName) {
	r.debouncer.ReconciliationNeeded(r.lifetimeCtx, tunnelProxyName, struct{}{}, func(rti reconcileTriggerInput[struct{}]) {
		event := ctrl_event.GenericEvent{
			Object: &apiv1.ContainerNetworkTunnelProxy{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: tunnelProxyName.Namespace,
					Name:      tunnelProxyName.Name,
				},
			},
		}
		r.reconciliationTriggerChan.In <- event
	})
}
