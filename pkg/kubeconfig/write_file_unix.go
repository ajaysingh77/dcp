//go:build !windows

package kubeconfig

import (
	"os"
	"path/filepath"
)

func writeFile(path string, content []byte) error {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err = os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	if err := os.WriteFile(path, content, 0600); err != nil {
		return err
	}
	return nil
}
