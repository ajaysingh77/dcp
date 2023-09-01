package commands

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/microsoft/usvc-apiserver/internal/version"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

const (
	defaultVersion = "dev"
)

var (
	Version        = defaultVersion
	CommitHash     = ""
	BuildTimestamp = ""
)

func NewVersionCommand(log logger.Logger) (*cobra.Command, error) {
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Prints version information",
		Long:  `Prints version information.`,
		RunE:  getVersion(log),
		Args:  cobra.NoArgs,
	}

	return versionCmd, nil
}

func getVersion(log logger.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log := log.WithName("version")

		if version, err := json.Marshal(version.Version()); err != nil {
			log.Error(err, "could not serialize version information")
			return err
		} else {
			fmt.Println(string(version))
		}

		return nil
	}
}
