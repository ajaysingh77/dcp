package main

import (
	"fmt"
	"os"

	kubeapiserver "k8s.io/apiserver/pkg/server"

	"github.com/microsoft/usvc-apiserver/internal/azdRenderer/commands"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

const (
	errCommandError = 1
)

func main() {
	ctx := kubeapiserver.SetupSignalContext()

	logger := logger.New("azdRenderer")

	root := commands.NewRootCommand(logger)
	err := root.ExecuteContext(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(errCommandError)
	}
}
