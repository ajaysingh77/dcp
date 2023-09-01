package commands

import (
	"fmt"

	"github.com/spf13/cobra"
	ctrlruntime "sigs.k8s.io/controller-runtime"

	cmds "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

func NewRootCmd(log logger.Logger) (*cobra.Command, error) {
	rootCmd := &cobra.Command{
		Use:   "dcp",
		Short: "Runs and manages multi-service applications and their dependencies",
		Long: `DCP is a developer tool for running multi-service applications.

	It integrates your code, emulators and containers to give you a development environment
	with minimum remote dependencies and maximum ease of use.`,
		SilenceUsage: true,
		PersistentPostRun: func(_ *cobra.Command, _ []string) {
			log.Flush()
		},
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	var err error
	var cmd *cobra.Command

	if cmd, err = cmds.NewVersionCommand(log); cmd != nil {
		rootCmd.AddCommand(cmd)
	} else {
		return nil, fmt.Errorf("could not set up 'version' command: %w", err)
	}

	if cmd, err = NewGenerateFileCommand(log); cmd != nil {
		rootCmd.AddCommand(cmd)
	} else {
		return nil, fmt.Errorf("could not set up 'generate-file' command: %w", err)
	}

	if cmd, err = NewUpCommand(log); cmd != nil {
		rootCmd.AddCommand(cmd)
	} else {
		return nil, fmt.Errorf("could not set up 'up' command: %w", err)
	}

	if cmd, err = NewStartApiSrvCommand(log); cmd != nil {
		rootCmd.AddCommand(cmd)
	} else {
		return nil, fmt.Errorf("could not set up 'start-apiserver' command: %w", err)
	}

	log.AddLevelFlag(rootCmd.PersistentFlags())
	ctrlruntime.SetLogger(log.V(1))

	return rootCmd, nil
}
