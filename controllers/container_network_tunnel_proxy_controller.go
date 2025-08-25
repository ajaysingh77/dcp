// Copyright (c) Microsoft Corporation. All rights reserved.

package controllers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
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
	"github.com/microsoft/usvc-apiserver/internal/dcppaths"
	"github.com/microsoft/usvc-apiserver/internal/dcpproc"
	"github.com/microsoft/usvc-apiserver/internal/dcptun"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
	"github.com/microsoft/usvc-apiserver/pkg/process"
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

	clientProxyContainerCleanupTimeout = 5 * time.Second
	serverProxyConfigReadTimeout       = 10 * time.Second
)

type ContainerNetworkTunnelProxyReconcilerConfig struct {
	Orchestrator    containers.ContainerOrchestrator // Mandatory
	ProcessExecutor process.Executor                 // Mandatory

	// Overrides the most recent image builds file path.
	// Used primarily for testing purposes.
	MostRecentImageBuildsFilePath string
}

type ContainerNetworkTunnelProxyReconciler struct {
	ctrl_client.Client
	log                 logr.Logger
	reconciliationSeqNo uint32

	// Reconciler lifetime context.
	lifetimeCtx context.Context

	config ContainerNetworkTunnelProxyReconcilerConfig

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
	config ContainerNetworkTunnelProxyReconcilerConfig,
	log logr.Logger,
) *ContainerNetworkTunnelProxyReconciler {
	if config.Orchestrator == nil {
		panic("ContainerNetworkTunnelProxyReconcilerConfig.Orchestrator must not be nil")
	}
	if config.ProcessExecutor == nil {
		panic("ContainerNetworkTunnelProxyReconcilerConfig.ProcessExecutor must not be nil")
	}

	return &ContainerNetworkTunnelProxyReconciler{
		Client:                    client,
		log:                       log,
		lifetimeCtx:               lifetimeCtx,
		config:                    config,
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
			MostRecentImageBuildsFilePath: r.config.MostRecentImageBuildsFilePath,
		}

		image, imageCheckErr := dcptun.EnsureClientProxyImage(ctx, opts, r.config.Orchestrator, log)
		if imageCheckErr != nil {
			log.Error(imageCheckErr, "Container image check for container network tunnel could not be queued")
			pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		} else {
			log.V(1).Info("Container image check for container network tunnel completed successfully", "Image", image)
			pd.State = apiv1.ContainerNetworkTunnelProxyStateStarting
			pd.ClientProxyContainerImage = image
		}

		nn := tunnelProxy.NamespacedName()
		pdMap := r.proxyData
		pdMap.QueueDeferredOp(nn, func(_ types.NamespacedName, _ types.NamespacedName) {
			pdMap.Update(nn, nn, pd)
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

		if clientCtrCreated {
			// Start server proxy now that client proxy ports are known
			serverStarted := r.startServerProxy(ctx, tunnelProxy, pd, log)
			if serverStarted {
				log.V(1).Info("Server proxy started successfully, scheduling reconciliation")
				pd.State = apiv1.ContainerNetworkTunnelProxyStateRunning
			}
		}

		if reconciliationKind == reconciliationKindDelayed {
			delay := defaultAdditionalReconciliationDelay + time.Duration(mathrand.Int63n(int64(defaultAdditionalReconciliationJitter)))
			time.Sleep(delay)
		}

		nn := tunnelProxy.NamespacedName()
		pdMap := r.proxyData
		pdMap.QueueDeferredOp(nn, func(_ types.NamespacedName, _ types.NamespacedName) {
			pdMap.Update(nn, nn, pd)
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

	thisProcess, thisProcessErr := process.This()
	if thisProcessErr != nil {
		log.Error(thisProcessErr, "could not get the current process information; container will not have creator process information")
	} else {
		createOpts.ContainerSpec.Labels = append(createOpts.ContainerSpec.Labels, apiv1.ContainerLabel{
			Key:   CreatorProcessIdLabel,
			Value: fmt.Sprintf("%d", thisProcess.Pid),
		})
		createOpts.ContainerSpec.Labels = append(createOpts.ContainerSpec.Labels, apiv1.ContainerLabel{
			Key:   CreatorProcessStartTimeLabel,
			Value: thisProcess.CreationTime.Format(osutil.RFC3339MiliTimestampFormat),
		})
	}

	created, createErr := createContainer(ctx, r.config.Orchestrator, createOpts)
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
		r.cleanupClientContainer(ctx, created.Id)
		pd.ClientProxyContainerID = ""
	}()

	started, startErr := startContainer(ctx, r.config.Orchestrator, clientProxyCtrName, created.Id, containers.StreamCommandOptions{})
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

	dcpproc.RunContainerWatcher(r.config.ProcessExecutor, created.Id, log)

	pd.ClientProxyControlPort = controlEndpointHostPort
	pd.ClientProxyDataPort = dataEndpointHostPort
	return true, reconciliationKindImmediate
}

// Starts the server proxy as an OS process.
// Assumes that the client proxy container has been started and data about it has already been applied
// to the passed containerNetworkTunnelProxyData instance.
// Updates the provided proxy data with process ID, startup timestamp, stdout/stderr capture files, and server control port.
// Returns true if everything went well and the server proxy has been started successfully.
func (r *ContainerNetworkTunnelProxyReconciler) startServerProxy(
	ctx context.Context,
	tunnelProxy *apiv1.ContainerNetworkTunnelProxy,
	pd *containerNetworkTunnelProxyData,
	log logr.Logger,
) bool {
	binDir, binDirErr := dcppaths.GetDcpBinDir()
	if binDirErr != nil {
		log.Error(binDirErr, "Failed to locate DCP bin directory for container tunnel server proxy binary")
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false
	}
	dcptunPath := filepath.Join(binDir, dcptun.ServerBinaryName)

	startFailed := false
	defer func() {
		if !startFailed {
			return
		}
		if pd.serverStdout != nil {
			_ = pd.serverStdout.Close()
			pd.serverStdout = nil
			pd.ServerProxyStdOutFile = ""
		}
		if pd.serverStderr != nil {
			_ = pd.serverStderr.Close()
			pd.serverStderr = nil
			pd.ServerProxyStdErrFile = ""
		}
	}()

	stdoutFile, stdoutErr := usvc_io.OpenTempFile(fmt.Sprintf("%s_out_%s", tunnelProxy.Name, tunnelProxy.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if stdoutErr != nil {
		startFailed = true
		log.Error(stdoutErr, "Failed to create stdout temp file for container tunnel server proxy")
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false
	} else {
		pd.ServerProxyStdOutFile = stdoutFile.Name()
		pd.serverStdout = stdoutFile
	}

	stderrFile, stderrErr := usvc_io.OpenTempFile(fmt.Sprintf("%s_err_%s", tunnelProxy.Name, tunnelProxy.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if stderrErr != nil {
		startFailed = true
		log.Error(stderrErr, "Failed to create stderr temp file for container tunnel server proxy")
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false
	} else {
		pd.ServerProxyStdErrFile = stderrFile.Name()
		pd.serverStderr = stderrFile
	}

	args := []string{
		"server",
		// We rely on the defaults for server control address and port (localhost:0, i.e. auto-allocated port), so not specifying them here.
		networking.IPv4LocalhostDefaultAddress, // Client control address--as exposed by container orchestrator
		strconv.Itoa(int(pd.ClientProxyControlPort)),
		networking.IPv4LocalhostDefaultAddress, // Client data address--as exposed by container orchestrator
		strconv.Itoa(int(pd.ClientProxyDataPort)),
	}

	cmd := exec.Command(dcptunPath, args...)
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile
	cmd.Env = os.Environ()
	logger.WithSessionId(cmd)

	// Start process and wait until the first JSON line is printed to stdout indicating server control address/port
	exitHandler := process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, err error) {
		if err != nil {
			log.Error(err, "Tunnel server proxy process exited with error", "PID", pid, "ExitCode", exitCode)
		} else if exitCode != 0 {
			log.Error(fmt.Errorf("tunnel server proxy process exited with non-zero exit code %d", exitCode), "Tunnel server proxy process exited abnormally", "PID", pid)
		}
		if closeErr := stdoutFile.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			log.Error(closeErr, "Failed to close stdout file for tunnel server proxy process", "PID", pid)
		}
		if closeErr := stderrFile.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			log.Error(closeErr, "Failed to close stderr file for tunnel server proxy process", "PID", pid)
		}
	})
	pid, startTime, startWaitForExit, startErr := r.config.ProcessExecutor.StartProcess(ctx, cmd, exitHandler, process.CreationFlagsNone)
	if startErr != nil {
		log.Error(startErr, "Failed to start server proxy process")
		startFailed = true
		pd.State = apiv1.ContainerNetworkTunnelProxyStateFailed
		return false
	}
	startWaitForExit()

	tc, tcErr := readServerProxyConfig(ctx, stdoutFile.Name())
	if tcErr != nil {
		log.Error(tcErr, "Failed to read connection information from the server proxy")
		stopProcessErr := r.config.ProcessExecutor.StopProcess(pid, startTime)
		if stopProcessErr != nil {
			log.Error(stopProcessErr, "Failed to stop server proxy process after being unable to read its configuration")
		}
		startFailed = true
		return false
	}

	dcpproc.RunProcessWatcher(r.config.ProcessExecutor, pid, startTime, log)

	pointers.SetValue(&pd.ServerProxyProcessID, int64(pid))
	pd.ServerProxyControlPort = tc.ServerControlPort
	pd.ServerProxyStartupTimestamp = metav1.NewMicroTime(startTime)
	pd.ServerProxyStdOutFile = stdoutFile.Name()
	pd.ServerProxyStdErrFile = stderrFile.Name()

	return true
}

func readServerProxyConfig(ctx context.Context, path string) (dcptun.TunnelProxyConfig, error) {
	configCtx, configCtxCancel := context.WithTimeout(ctx, serverProxyConfigReadTimeout)
	defer configCtxCancel()

	config, err := resiliency.RetryGet(configCtx, backoff.NewConstantBackOff(200*time.Millisecond), func() (dcptun.TunnelProxyConfig, error) {
		f, fErr := usvc_io.OpenFile(path, os.O_RDONLY, 0)
		if fErr != nil {
			return dcptun.TunnelProxyConfig{}, fErr
		}
		defer func() { _ = f.Close() }()

		s := bufio.NewScanner(f)
		if !s.Scan() {
			scanErr := s.Err()
			if scanErr != nil {
				return dcptun.TunnelProxyConfig{}, scanErr
			} else {
				return dcptun.TunnelProxyConfig{}, io.EOF
			}
		}
		var config dcptun.TunnelProxyConfig
		umErr := json.Unmarshal(s.Bytes(), &config)
		if umErr != nil {
			return dcptun.TunnelProxyConfig{}, umErr
		}
		return config, nil
	})

	return config, err
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

func (r ContainerNetworkTunnelProxyReconciler) cleanupClientContainer(ctx context.Context, containerID string) {
	removeCtx, removeCtxCancel := context.WithTimeout(ctx, clientProxyContainerCleanupTimeout)
	defer removeCtxCancel()
	_ = removeContainer(removeCtx, r.config.Orchestrator, containerID) // Best effort
}
