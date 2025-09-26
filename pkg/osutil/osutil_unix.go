//go:build !windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package osutil

import (
	"os"
)

func IsAdmin() (bool, error) {
	if os.Getuid() == 0 {
		return true, nil
	} else {
		return false, nil
	}
}
