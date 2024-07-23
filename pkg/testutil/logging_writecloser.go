package testutil

import (
	"io"

	"github.com/go-logr/logr"
)

// LoggingWriteCloser is an implementation of io.WriteCloser that also logs results of all writes
// and results of the Close() call to a given log.

type loggingWriteCloser struct {
	log    logr.Logger
	inner  io.Writer
	closer io.Closer
}

func NewLoggingWriteCloser(log logr.Logger, inner io.Writer) io.WriteCloser {
	lwc := loggingWriteCloser{
		log:   log,
		inner: inner,
	}
	if closer, ok := inner.(io.Closer); ok {
		lwc.closer = closer
	}
	return &lwc
}

func (lwc *loggingWriteCloser) Write(p []byte) (int, error) {
	n, err := lwc.inner.Write(p)
	if err != nil {
		lwc.log.Error(err, "error writing to inner writer",
			"bytes", p,
			"written", n,
		)
	} else {
		lwc.log.V(1).Info("write succeeded",
			"bytes", p,
			"written", n,
		)
	}
	return n, err
}

func (lwc *loggingWriteCloser) Close() error {
	if lwc.closer == nil {
		return nil // No-op since the inner writer is not a closer.
	}

	err := lwc.closer.Close()
	if err != nil {
		lwc.log.Error(err, "error closing inner writer")
	} else {
		lwc.log.V(1).Info("inner writer closed")
	}
	return err
}
