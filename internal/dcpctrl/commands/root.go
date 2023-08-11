package commands

import (
	"github.com/spf13/cobra"
	ctrlruntime "sigs.k8s.io/controller-runtime"

	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

func NewRootCommand(logger logger.Logger) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "dcpctrl",
		Short: "Runs standard DCP controllers (for Executable, Container, and ContainerVolume objects)",
		Long: `DCP is a developer tool for running multi-service applications.

	It integrates your code, emulators and containers to give you an development environment
	with minimum remote dependencies and maximum ease of use.

	dcpctrl is the host process that runs the controllers for the DCP API objects.`,
		SilenceUsage: true,
		PersistentPostRun: func(_ *cobra.Command, _ []string) {
			logger.Flush()
		},
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	rootCmd.AddCommand(NewGetCapabilitiesCommand(logger))
	rootCmd.AddCommand(NewRunControllersCommand(logger))

	logger.AddLevelFlag(rootCmd.PersistentFlags())
	ctrlruntime.SetLogger(logger.V(1))

	return rootCmd
}
