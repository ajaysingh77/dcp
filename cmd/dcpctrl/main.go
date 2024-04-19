package main

//go:generate goversioninfo

import (
	"fmt"
	"os"

	kubeapiserver "k8s.io/apiserver/pkg/server"

	cmdutil "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/internal/dcpctrl/commands"
	"github.com/microsoft/usvc-apiserver/internal/telemetry"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

const (
	errCommandError = 1
	errSetup        = 2
	errPanic        = 3
)

func main() {
	log := logger.New("dcpctrl")
	defer log.BeforeExit(func(value interface{}) {
		// Attempt to log the panic before exiting (we're already in a panic state, so the worst that can happen is that we panic again)
		log.Error(fmt.Errorf("panic: %v", value), "exiting due to panic")
		os.Exit(errPanic)
	})

	ctx := kubeapiserver.SetupSignalContext()

	telemetrySystem := telemetry.GetTelemetrySystem()

	root, err := commands.NewRootCommand(log)
	if err != nil {
		cmdutil.ErrorExit(log, err, errSetup)
	}

	err = root.ExecuteContext(ctx)
	_ = telemetrySystem.Shutdown(ctx)
	if err != nil {
		cmdutil.ErrorExit(log, err, errCommandError)
	} else {
		log.Flush()
	}
}
