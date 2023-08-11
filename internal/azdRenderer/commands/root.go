package commands

import (
	"github.com/spf13/cobra"
	ctrlruntime "sigs.k8s.io/controller-runtime"

	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

func NewRootCommand(logger logger.Logger) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "azdRenderer",
		Short: "Application workload renderer for Azure Developer CLI-enabled applications",
		Long: `DCP is a developer tool for running multi-service applications.

	It integrates your code, emulators and containers to give you an development environment
	with minimum remote dependencies and maximum ease of use.

	AzdRenderer is an extension that enables running Azure Developer CLI-enabled application locally,
	with no need to deploy anything to Azure.`,
		SilenceUsage: true,
		PersistentPostRun: func(_ *cobra.Command, _ []string) {
			logger.Flush()
		},
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	rootCmd.AddCommand(NewGetCapabilitiesCommand(logger))
	rootCmd.AddCommand(NewCanRenderCommand(logger))
	rootCmd.AddCommand(NewRenderWorkloadCommand(logger))

	logger.AddLevelFlag(rootCmd.PersistentFlags())
	ctrlruntime.SetLogger(logger.V(1))

	return rootCmd
}
