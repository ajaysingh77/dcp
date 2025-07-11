package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	cmds "github.com/microsoft/usvc-apiserver/internal/commands"
	container_flags "github.com/microsoft/usvc-apiserver/internal/containers/flags"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

func NewRootCommand(log logger.Logger) (*cobra.Command, error) {
	rootCmd := &cobra.Command{
		SilenceErrors: true,
		Use:           "dcptun",
		Short:         "Runs reverse tunnels for DCP applications",
		Long: `Runs reverse tunnels for DCP applications.

	A reverse tunnel allows clients to connect to servers running on a different network that is not diredctly accessible for clients`,
		SilenceUsage:     true,
		PersistentPreRun: cmds.LogVersion(log, "Starting DCPTUN..."),
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	var err error
	var cmd *cobra.Command

	if cmd, err = cmds.NewVersionCommand(log); err != nil {
		return nil, fmt.Errorf("could not set up 'version' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	rootCmd.AddCommand(NewRunServerCommad(log))
	rootCmd.AddCommand(NewRunClientCommand(log))

	container_flags.EnsureRuntimeFlag(rootCmd.PersistentFlags())

	log.AddLevelFlag(rootCmd.PersistentFlags())

	return rootCmd, nil
}
