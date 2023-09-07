// Copyright (c) Microsoft Corporation. All rights reserved.

package testutil

import (
	"context"
	"time"

	"github.com/microsoft/usvc-apiserver/internal/containers"
)

type testContainerOrchestrator struct {
	inner ContainerAndVolumeOrchestrator
}

type ContainerAndVolumeOrchestrator interface {
	containers.ContainerOrchestrator
	containers.VolumeOrchestrator
}

func NewTestContainerOrchestrator(inner ContainerAndVolumeOrchestrator) ContainerAndVolumeOrchestrator {
	return &testContainerOrchestrator{
		inner: inner,
	}
}

func withTestTimeout[ReturnT any](outerCtx context.Context, doWork func(context.Context) (ReturnT, error)) (ReturnT, error) {
	// In rare cases, when running tests on slow machines, the reconciliation queue might have
	// mmore than one call scheduled, and they can happen in short order after each other.
	// The reconciliation function can then load from its cache the same object in the same state twice,
	// and try to invoke Docker commands more often than the test expects.
	// For example, the controller might see Container in "pending deletion" state twice and invoke "docker rm" twice.
	//
	// Since the test code handles each call (and provides each response individually, the extra call "hangs"
	// until the container orchestrator times out, which is set to be fairly long,
	// because real-world Docker CLI invocatiions can take a long time.
	//
	// For this reason, during testing we use a much shorter timeout Docker calls and let the controller
	// experience the error if the call times out. This will cause a reconciliation retry for a given container,
	// but it will allow the controller to make progess on other work,
	// and by the time the reconciliation retry happens, the state of the Container object will have changed.

	var effectiveCtx context.Context
	var cancelFn context.CancelFunc
	if outerCtx == nil {
		effectiveCtx, cancelFn = context.WithTimeout(context.Background(), 3*time.Second)
	} else {
		effectiveCtx, cancelFn = context.WithTimeout(outerCtx, 3*time.Second)
	}
	defer cancelFn()

	return doWork(effectiveCtx)
}

func (tco *testContainerOrchestrator) RunContainer(ctx context.Context, opts containers.RunContainerOptions) (string, error) {
	return withTestTimeout(ctx, func(effectiveCtx context.Context) (string, error) {
		return tco.inner.RunContainer(effectiveCtx, opts)
	})
}

func (tco *testContainerOrchestrator) InspectContainers(ctx context.Context, containerIDs []string) ([]containers.InspectedContainer, error) {
	return withTestTimeout(ctx, func(effectiveCtx context.Context) ([]containers.InspectedContainer, error) {
		return tco.inner.InspectContainers(effectiveCtx, containerIDs)
	})
}

func (tco *testContainerOrchestrator) StopContainers(ctx context.Context, containerIDs []string, secondsToKill uint) ([]string, error) {
	return withTestTimeout(ctx, func(effectiveCtx context.Context) ([]string, error) {
		return tco.inner.StopContainers(effectiveCtx, containerIDs, secondsToKill)
	})
}

func (tco *testContainerOrchestrator) RemoveContainers(ctx context.Context, containerIDs []string, force bool) ([]string, error) {
	return withTestTimeout(ctx, func(effectiveCtx context.Context) ([]string, error) {
		return tco.inner.RemoveContainers(effectiveCtx, containerIDs, force)
	})
}

func (tco *testContainerOrchestrator) WatchContainers(sink chan<- containers.EventMessage) (containers.EventSubscription, error) {
	// Container watcher is started in the background, so the inner call will always return quickly.
	return tco.inner.WatchContainers(sink)
}

func (tco *testContainerOrchestrator) CreateVolume(ctx context.Context, name string) error {
	_, err := withTestTimeout(ctx, func(effectiveCtx context.Context) (struct{}, error) {
		err := tco.inner.CreateVolume(effectiveCtx, name)
		return struct{}{}, err
	})
	return err
}

func (tco *testContainerOrchestrator) InspectVolumes(ctx context.Context, volumes []string) ([]containers.InspectedVolume, error) {
	return withTestTimeout(ctx, func(effectiveCtx context.Context) ([]containers.InspectedVolume, error) {
		return tco.inner.InspectVolumes(effectiveCtx, volumes)
	})
}

func (tco *testContainerOrchestrator) RemoveVolumes(ctx context.Context, volumes []string, force bool) ([]string, error) {
	return withTestTimeout(ctx, func(effectiveCtx context.Context) ([]string, error) {
		return tco.inner.RemoveVolumes(effectiveCtx, volumes, force)
	})
}

var _ containers.ContainerOrchestrator = (*testContainerOrchestrator)(nil)
var _ containers.VolumeOrchestrator = (*testContainerOrchestrator)(nil)
