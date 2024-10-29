package logs

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/microsoft/usvc-apiserver/internal/resiliency"
)

const (
	DefaultLogFileRemovalTimeout = 3 * time.Second
)

// RemoveWithRetry removes a file with exponential backoff retry.
//
// This function is handy for files that store logs. When an object exposing logs is being deleted,
// DCP API server closes all associated log streams, but this happens asynchronously,
// so we migt need to wait a bit for the files to be released.
func RemoveWithRetry(ctx context.Context, path string) error {
	return resiliency.RetryExponentialWithTimeout(ctx, DefaultLogFileRemovalTimeout, func() error {
		removeErr := os.Remove(path)
		if removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
			return nil
		} else {
			return removeErr
		}
	})
}
