package main

import (
	"fmt"
	"os"

	"github.com/microsoft/usvc-apiserver/internal/dcp/commands"
)

const (
	errCommand = 1
	errSetup   = 2
)

func main() {
	root, err := commands.NewRootCmd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(errSetup)
	}

	err = root.Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(errCommand)
	}
}
