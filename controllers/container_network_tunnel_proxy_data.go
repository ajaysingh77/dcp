package controllers

import (
	"os"
	std_slices "slices"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

// Data we keep in memory for ContainerNetworkTunnelProxy instances.
type containerNetworkTunnelProxyData struct {
	apiv1.ContainerNetworkTunnelProxyStatus

	// Whether the startup of the proxy pair has been scheduled.
	// This is checked and updated when we enter the starting state.
	startupScheduled bool

	// Whether the cleanup of the proxy pair has been scheduled.
	// Graceful shutdown of the client proxy container and the server proxy process
	// can take a while, so we do it asynchronously.
	cleanupScheduled bool

	// Standard output file for the server proxy process.
	// Note: this is a file descriptor, and is not "cloned" when Clone() is called.
	serverStdout *os.File

	// Standard error file for the server proxy process.
	// Note: this is a file descriptor, and is not "cloned" when Clone() is called.
	serverStderr *os.File
}

func (tpd *containerNetworkTunnelProxyData) Clone() *containerNetworkTunnelProxyData {
	clone := containerNetworkTunnelProxyData{
		ContainerNetworkTunnelProxyStatus: *tpd.ContainerNetworkTunnelProxyStatus.DeepCopy(),
		startupScheduled:                  tpd.startupScheduled,
		cleanupScheduled:                  tpd.cleanupScheduled,
		serverStdout:                      tpd.serverStdout,
		serverStderr:                      tpd.serverStderr,
	}
	return &clone
}

func (tpd *containerNetworkTunnelProxyData) UpdateFrom(other *containerNetworkTunnelProxyData) bool {
	if other == nil {
		return false
	}

	updated := false

	if tpd.State != other.State {
		tpd.State = other.State
		updated = true
	}

	if !std_slices.EqualFunc(tpd.TunnelStatuses, other.TunnelStatuses, apiv1.TunnelStatus.Equal) {
		tpd.TunnelStatuses = slices.Map[apiv1.TunnelStatus, apiv1.TunnelStatus](other.TunnelStatuses, apiv1.TunnelStatus.Clone)
		updated = true
	}

	if tpd.TunnelConfigurationVersion != other.TunnelConfigurationVersion {
		tpd.TunnelConfigurationVersion = other.TunnelConfigurationVersion
		updated = true
	}

	if tpd.ClientProxyContainerImage != other.ClientProxyContainerImage {
		tpd.ClientProxyContainerImage = other.ClientProxyContainerImage
		updated = true
	}

	if tpd.ClientProxyContainerID != other.ClientProxyContainerID {
		tpd.ClientProxyContainerID = other.ClientProxyContainerID
		updated = true
	}

	if !pointers.EqualValue(tpd.ServerProxyProcessID, other.ServerProxyProcessID) {
		pointers.SetValueFrom(&tpd.ServerProxyProcessID, other.ServerProxyProcessID)
		updated = true
	}

	if tpd.ServerProxyStartupTimestamp != other.ServerProxyStartupTimestamp {
		tpd.ServerProxyStartupTimestamp = other.ServerProxyStartupTimestamp
		updated = true
	}

	if tpd.ServerProxyStdOutFile != other.ServerProxyStdOutFile {
		tpd.ServerProxyStdOutFile = other.ServerProxyStdOutFile
		updated = true
	}

	if tpd.ServerProxyStdErrFile != other.ServerProxyStdErrFile {
		tpd.ServerProxyStdErrFile = other.ServerProxyStdErrFile
		updated = true
	}

	if tpd.ClientProxyControlPort != other.ClientProxyControlPort {
		tpd.ClientProxyControlPort = other.ClientProxyControlPort
		updated = true
	}

	if tpd.ClientProxyDataPort != other.ClientProxyDataPort {
		tpd.ClientProxyDataPort = other.ClientProxyDataPort
		updated = true
	}

	if tpd.ServerProxyControlPort != other.ServerProxyControlPort {
		tpd.ServerProxyControlPort = other.ServerProxyControlPort
		updated = true
	}

	if tpd.startupScheduled != other.startupScheduled {
		tpd.startupScheduled = other.startupScheduled
		updated = true
	}

	if tpd.cleanupScheduled != other.cleanupScheduled {
		tpd.cleanupScheduled = other.cleanupScheduled
		updated = true
	}

	if tpd.serverStdout != other.serverStdout {
		tpd.serverStdout = other.serverStdout
		updated = true
	}

	if tpd.serverStderr != other.serverStderr {
		tpd.serverStderr = other.serverStderr
		updated = true
	}

	return updated
}

func (tpd *containerNetworkTunnelProxyData) applyTo(tunnelProxy *apiv1.ContainerNetworkTunnelProxy) objectChange {
	change := noChange

	if tpd.State != apiv1.ContainerNetworkTunnelProxyStateEmpty && tpd.State != tunnelProxy.Status.State {
		tunnelProxy.Status.State = tpd.State
		change |= statusChanged
	}

	if len(tpd.TunnelStatuses) > 0 && !std_slices.EqualFunc(tpd.TunnelStatuses, tunnelProxy.Status.TunnelStatuses, apiv1.TunnelStatus.Equal) {
		tunnelProxy.Status.TunnelStatuses = slices.Map[apiv1.TunnelStatus, apiv1.TunnelStatus](tpd.TunnelStatuses, apiv1.TunnelStatus.Clone)
		change |= statusChanged
	}

	if tpd.TunnelConfigurationVersion != tunnelProxy.Status.TunnelConfigurationVersion {
		tunnelProxy.Status.TunnelConfigurationVersion = tpd.TunnelConfigurationVersion
		change |= statusChanged
	}

	if tpd.ClientProxyContainerImage != tunnelProxy.Status.ClientProxyContainerImage {
		tunnelProxy.Status.ClientProxyContainerImage = tpd.ClientProxyContainerImage
		change |= statusChanged
	}

	if tpd.ClientProxyContainerID != tunnelProxy.Status.ClientProxyContainerID {
		tunnelProxy.Status.ClientProxyContainerID = tpd.ClientProxyContainerID
		change |= statusChanged
	}

	if !pointers.EqualValue(tunnelProxy.Status.ServerProxyProcessID, tpd.ServerProxyProcessID) {
		pointers.SetValueFrom(&tunnelProxy.Status.ServerProxyProcessID, tpd.ServerProxyProcessID)
		change |= statusChanged
	}

	if tunnelProxy.Status.ServerProxyStartupTimestamp.IsZero() && !tpd.ServerProxyStartupTimestamp.IsZero() {
		tunnelProxy.Status.ServerProxyStartupTimestamp = tpd.ServerProxyStartupTimestamp
		change |= statusChanged
	}

	if tpd.ServerProxyStdOutFile != tunnelProxy.Status.ServerProxyStdOutFile {
		tunnelProxy.Status.ServerProxyStdOutFile = tpd.ServerProxyStdOutFile
		change |= statusChanged
	}

	if tpd.ServerProxyStdErrFile != tunnelProxy.Status.ServerProxyStdErrFile {
		tunnelProxy.Status.ServerProxyStdErrFile = tpd.ServerProxyStdErrFile
		change |= statusChanged
	}

	if tpd.ClientProxyControlPort != tunnelProxy.Status.ClientProxyControlPort {
		tunnelProxy.Status.ClientProxyControlPort = tpd.ClientProxyControlPort
		change |= statusChanged
	}

	if tpd.ClientProxyDataPort != tunnelProxy.Status.ClientProxyDataPort {
		tunnelProxy.Status.ClientProxyDataPort = tpd.ClientProxyDataPort
		change |= statusChanged
	}

	if tpd.ServerProxyControlPort != tunnelProxy.Status.ServerProxyControlPort {
		tunnelProxy.Status.ServerProxyControlPort = tpd.ServerProxyControlPort
		change |= statusChanged
	}

	return change
}

var _ Cloner[*containerNetworkTunnelProxyData] = (*containerNetworkTunnelProxyData)(nil)
var _ UpdateableFrom[*containerNetworkTunnelProxyData] = (*containerNetworkTunnelProxyData)(nil)
