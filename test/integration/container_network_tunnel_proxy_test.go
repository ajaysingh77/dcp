package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/dcppaths"
	"github.com/microsoft/usvc-apiserver/internal/dcptun"
	"github.com/microsoft/usvc-apiserver/internal/networking"
	internal_testutil "github.com/microsoft/usvc-apiserver/internal/testutil"
	ctrl_testutil "github.com/microsoft/usvc-apiserver/internal/testutil/ctrlutil"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

// Verifies that ContainerNetworkTunnelProxy can be created and deleted (including finalizer handling).
func TestTunnelProxyCreateDelete(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-create-delete"
	log := testutil.NewLogForTesting(t.Name())

	// Use dedicated test environment because otherwise it is difficult to differentiate
	// between different server proxy processes running in parallel tests.
	includedControllers := NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, tpe, _, startupErr := StartTestEnvironment(ctx, includedControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")

	defer func() {
		cancel()

		// Wait for the API server cleanup to complete.
		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-time.After(5 * time.Second):
		}
	}()

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	const serverControlPort int32 = 12393
	simulateServerProxy(t, serverControlPort, tpe)

	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:       "test-tunnel",
					ServerPort: 8080,
				},
			},
		},
	}

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s'...", tunnelProxy.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	t.Log("Waiting for controller to add finalizer to ContainerNetworkTunnelProxy..")
	tunnelProxyFinalizerName := fmt.Sprintf("%s/tunnel-proxy-reconciler", apiv1.GroupVersion.Group)
	_ = waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return slices.Contains(tp.ObjectMeta.Finalizers, tunnelProxyFinalizerName), nil
	})

	t.Logf("Deleting ContainerNetworkTunnelProxy object '%s'", tunnelProxy.ObjectMeta.Name)
	err = retryOnConflictEx(ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(ctx context.Context, tp *apiv1.ContainerNetworkTunnelProxy) error {
		return serverInfo.Client.Delete(ctx, tp)
	})
	require.NoError(t, err, "Could not delete ContainerNetworkTunnelProxy object")

	t.Log("Waiting for controller to remove finalizer and complete deletion...")
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, &tunnelProxy)
}

// Verifies that ContainerNetworkTunnelProxy can be created without an existing ContainerNetwork
// and transitions to Running state once the network is created.
func TestTunnelProxyDelayedNetworkCreation(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-delayed-network-creation"
	log := testutil.NewLogForTesting(t.Name())

	includedControllers := NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, tpe, _, startupErr := StartTestEnvironment(ctx, includedControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")

	defer func() {
		cancel()

		// Wait for the API server cleanup to complete.
		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-time.After(5 * time.Second):
		}
	}()

	// Create the ContainerNetworkTunnelProxy object WITHOUT creating the ContainerNetwork first
	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: testName + "-network",
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:       "test-tunnel",
					ServerPort: 8080,
				},
			},
		},
	}

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s' without referenced ContainerNetwork", tunnelProxy.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	// Wait for the controller to do initial pass over the object
	t.Log("Waiting for controller to add finalizer to ContainerNetworkTunnelProxy...")
	_ = waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStatePending, nil
	})

	const serverControlPort int32 = 13299
	simulateServerProxy(t, serverControlPort, tpe)

	// Now create the ContainerNetwork that the tunnel proxy references
	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tunnelProxy.Spec.ContainerNetworkName,
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	createNetworkErr := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, createNetworkErr, "Could not create a ContainerNetwork object")

	t.Log("Waiting for ContainerNetworkTunnelProxy to transition to Running state...")
	updatedTunnelProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})

	require.NotEmpty(t, updatedTunnelProxy.Status.ClientProxyContainerImage, "Tunnel proxy should publish the image for the client proxy container")
}

// Verifies that running ContainerNetworkTunnelProxy has the status updated with client proxy and server proxy information.
func TestTunnelProxyRunningStatus(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-running-status"
	log := testutil.NewLogForTesting(t.Name())

	includedControllers := NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, tpe, _, startupErr := StartTestEnvironment(ctx, includedControllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")

	defer func() {
		cancel()

		// Wait for the API server cleanup to complete.
		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-time.After(5 * time.Second):
		}
	}()

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	const serverControlPort int32 = 26444
	simulateServerProxy(t, serverControlPort, tpe)

	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:       "test-tunnel",
					ServerPort: 8080,
				},
			},
		},
	}

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s'...", tunnelProxy.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	t.Log("Waiting for ContainerNetworkTunnelProxy to transition to Running state...")
	updatedTunnelProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})

	t.Log("Verifying client proxy status...")
	require.NotEmpty(t, updatedTunnelProxy.Status.ClientProxyContainerImage, "Tunnel proxy should publish the image for the client proxy container")
	require.NotEmpty(t, updatedTunnelProxy.Status.ClientProxyContainerID, "Tunnel proxy should have a client proxy container ID")
	require.True(t, networking.IsValidPort(int(updatedTunnelProxy.Status.ClientProxyControlPort)), "Tunnel proxy should have a valid client proxy control port")
	require.True(t, networking.IsValidPort(int(updatedTunnelProxy.Status.ClientProxyDataPort)), "Tunnel proxy should have a valid client proxy data port")

	t.Log("Verifying client proxy container exists...")
	inspectedContainers, inspectErr := serverInfo.ContainerOrchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{updatedTunnelProxy.Status.ClientProxyContainerID},
	})
	require.NoError(t, inspectErr, "Should be able to inspect client proxy container")
	require.Len(t, inspectedContainers, 1, "Should find exactly one container")
	clientContainer := inspectedContainers[0]
	require.Equal(t, updatedTunnelProxy.Status.ClientProxyContainerImage, clientContainer.Image, "Container should have the expected image")
	require.Equal(t, containers.ContainerStatusRunning, clientContainer.Status, "Container should be running")

	t.Log("Verifying client proxy container has correct labels...")
	require.Contains(t, clientContainer.Labels, controllers.CreatorProcessIdLabel, "Container should have creator process ID label")
	require.Equal(t, fmt.Sprintf("%d", os.Getpid()), clientContainer.Labels[controllers.CreatorProcessIdLabel], "Container should have correct creator process ID label")
	require.Contains(t, clientContainer.Labels, controllers.CreatorProcessStartTimeLabel, "Container should have creator process start time label")

	t.Log("Verifying server proxy status...")
	require.NotNil(t, updatedTunnelProxy.Status.ServerProxyProcessID, "Server proxy should have a process ID")
	require.False(t, updatedTunnelProxy.Status.ServerProxyStartupTimestamp.IsZero(), "Server proxy should have a startup timestamp")
	require.NotEmpty(t, updatedTunnelProxy.Status.ServerProxyStdOutFile, "Server proxy should publish a stdout log file path")
	require.NotEmpty(t, updatedTunnelProxy.Status.ServerProxyStdErrFile, "Server proxy should publish a stderr log file path")
	require.Equal(t, serverControlPort, updatedTunnelProxy.Status.ServerProxyControlPort, "Server proxy should have the expected control port")

	t.Log("Verifying server proxy process has been started...")
	serverPid, pidErr := process.Int64_ToPidT(*updatedTunnelProxy.Status.ServerProxyProcessID)
	require.NoError(t, pidErr, "Should be able to convert process ID")
	pe, found := tpe.FindByPid(serverPid)
	require.True(t, found, "Should find server proxy process in test process executor")

	t.Log("Verifying server proxy process has correct launch arguments...")
	require.True(t, len(pe.Cmd.Args) >= 6, "Server proxy should have at least 6 command line arguments")
	require.Equal(t, "server", pe.Cmd.Args[1], "First argument should be 'server'")
	require.Equal(t, networking.IPv4LocalhostDefaultAddress, pe.Cmd.Args[2], "Second argument should be client control address")
	require.Equal(t, fmt.Sprintf("%d", updatedTunnelProxy.Status.ClientProxyControlPort), pe.Cmd.Args[3], "Third argument should be client control port")
	require.Equal(t, networking.IPv4LocalhostDefaultAddress, pe.Cmd.Args[4], "Fourth argument should be client data address")
	require.Equal(t, fmt.Sprintf("%d", updatedTunnelProxy.Status.ClientProxyDataPort), pe.Cmd.Args[5], "Fifth argument should be client data port")
}

// Verifies that ContainerNetworkTunnelProxy proxy pair cleanup works correctly during object deletion.
func TestTunnelProxyCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()
	dcppaths.EnableTestPathProbing()
	const testName = "test-tunnel-proxy-cleanup"
	log := testutil.NewLogForTesting(t.Name())

	controllers := NetworkController | ContainerNetworkTunnelProxyController
	serverInfo, tpe, _, startupErr := StartTestEnvironment(ctx, controllers, t.Name(), t.TempDir(), log)
	require.NoError(t, startupErr, "Failed to start the API server")

	defer func() {
		cancel()

		// Wait for the API server cleanup to complete.
		select {
		case <-serverInfo.ApiServerDisposalComplete.Wait():
		case <-time.After(5 * time.Second):
		}
	}()

	network := apiv1.ContainerNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName + "-network",
			Namespace: metav1.NamespaceNone,
		},
	}

	t.Logf("Creating ContainerNetwork object '%s'", network.ObjectMeta.Name)
	err := serverInfo.Client.Create(ctx, &network)
	require.NoError(t, err, "Could not create a ContainerNetwork object")

	const serverControlPort int32 = 34567
	simulateServerProxy(t, serverControlPort, tpe)

	tunnelProxy := apiv1.ContainerNetworkTunnelProxy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerNetworkTunnelProxySpec{
			ContainerNetworkName: network.ObjectMeta.Name,
			Tunnels: []apiv1.TunnelConfiguration{
				{
					Name:       "test-tunnel",
					ServerPort: 8080,
				},
			},
		},
	}

	t.Logf("Creating ContainerNetworkTunnelProxy object '%s'...", tunnelProxy.ObjectMeta.Name)
	err = serverInfo.Client.Create(ctx, &tunnelProxy)
	require.NoError(t, err, "Could not create a ContainerNetworkTunnelProxy object")

	t.Log("Waiting for ContainerNetworkTunnelProxy to transition to Running state...")
	updatedTunnelProxy := waitObjectAssumesStateEx(t, ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(tp *apiv1.ContainerNetworkTunnelProxy) (bool, error) {
		return tp.Status.State == apiv1.ContainerNetworkTunnelProxyStateRunning, nil
	})

	// Capture resource information before deletion
	clientContainerID := updatedTunnelProxy.Status.ClientProxyContainerID
	serverProcessID := updatedTunnelProxy.Status.ServerProxyProcessID

	t.Log("Verifying resources exist before deletion...")
	require.NotEmpty(t, clientContainerID, "Client proxy container ID should be set")
	require.NotNil(t, serverProcessID, "Server proxy process ID should be set")
	require.Greater(t, *serverProcessID, int64(0), "Server proxy process ID should be valid")

	// Verify the client container exists
	orchestrator := serverInfo.ContainerOrchestrator
	containerInfoList, inspectErr := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{clientContainerID},
	})
	require.NoError(t, inspectErr, "Should be able to inspect client proxy container")
	require.Len(t, containerInfoList, 1, "Client proxy container should exist")
	require.Equal(t, clientContainerID, containerInfoList[0].Id, "Container ID should match")

	t.Logf("Deleting ContainerNetworkTunnelProxy object '%s'", tunnelProxy.ObjectMeta.Name)
	err = retryOnConflictEx(ctx, serverInfo.Client, tunnelProxy.NamespacedName(), func(ctx context.Context, tp *apiv1.ContainerNetworkTunnelProxy) error {
		return serverInfo.Client.Delete(ctx, tp)
	})
	require.NoError(t, err, "Could not delete ContainerNetworkTunnelProxy object")

	t.Log("Waiting for controller to remove finalizer and complete deletion...")
	ctrl_testutil.WaitObjectDeleted(t, ctx, serverInfo.Client, &tunnelProxy)

	t.Log("Verifying proxy resources are cleaned up...")

	// Verify the client container has been removed
	_, inspectErrAfter := orchestrator.InspectContainers(ctx, containers.InspectContainersOptions{
		Containers: []string{clientContainerID},
	})
	require.ErrorIs(t, inspectErrAfter, containers.ErrNotFound, "Client proxy container should have been removed")

	// For the server process, verify that it was stopped by checking if it has finished in the test process executor
	processExecution, processFound := tpe.FindByPid(process.Pid_t(*serverProcessID))
	require.True(t, processFound, "Server proxy process should be found in test process executor")
	require.True(t, processExecution.Finished(), "Server proxy process should have been stopped")
}

// The tunnel controller will try to create a server-side proxy
// and read the network configuration (control port in particular) off of it,
// so we need to simulate that.
func simulateServerProxy(
	t *testing.T,
	serverControlPort int32,
	tpe *internal_testutil.TestProcessExecutor,
) {
	binDir, binDirErr := dcppaths.GetDcpBinDir()
	require.NoError(t, binDirErr)
	dcptunPath := filepath.Join(binDir, dcptun.ServerBinaryName)

	tpe.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{dcptunPath, "server"},
		},
		RunCommand: func(pe *internal_testutil.ProcessExecution) int32 {
			tc := dcptun.TunnelProxyConfig{
				ServerControlPort: serverControlPort,
			}
			tcBytes, tcErr := json.Marshal(tc)
			require.NoError(t, tcErr, "Could not marshal TunnelProxyConfig to JSON??")
			_, writeErr := pe.Cmd.Stdout.Write(osutil.WithNewline(tcBytes))
			require.NoError(t, writeErr, "Could not write TunnelProxyConfig to stdout")

			<-pe.Signal
			return 0
		},
	})
}
