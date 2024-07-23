package io

import (
	"io"
	"net/http"
)

type flushWriter struct {
	inner   io.Writer
	flusher http.Flusher
}

// FlushWriter is a writer that flushes after every write if the writer implements the Flusher interface.
func NewFlushWriter(w io.Writer) io.Writer {
	fw := &flushWriter{
		inner: w,
	}
	if flusher, ok := w.(http.Flusher); ok {
		fw.flusher = flusher
	}
	return fw
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.inner.Write(p)
	if err != nil {
		return n, err
	}
	if fw.flusher != nil {
		fw.flusher.Flush()
	}
	return n, nil
}

var _ io.Writer = (*flushWriter)(nil)
