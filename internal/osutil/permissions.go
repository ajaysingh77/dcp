package osutil

import "os"

const (
	PermissionFile          os.FileMode = 0644
	PermissionFileOwnerOnly os.FileMode = 0600
	PermissionFileOwnerAll  os.FileMode = 0700
)
