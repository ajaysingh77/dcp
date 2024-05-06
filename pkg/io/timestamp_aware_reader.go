package io

import (
	"bufio"
	"bytes"
	"io"
	"time"
	"unicode"
)

// Reasonable length longer than any RFC3339Nano timestamp
const maxTimestampLength = 40

type timestampAwareReader struct {
	inner             io.Reader
	reader            *bufio.Reader
	includeTimestamps bool
	maybeTimestamp    bool
	timestampBuffer   *bytes.Buffer
}

func NewTimestampAwareReader(inner io.Reader, includeTimestamps bool) *timestampAwareReader {
	return &timestampAwareReader{
		inner:             inner,
		reader:            bufio.NewReader(inner),
		includeTimestamps: includeTimestamps,
		maybeTimestamp:    true,
		timestampBuffer:   new(bytes.Buffer),
	}
}

func (tr *timestampAwareReader) Read(p []byte) (int, error) {
	if tr.includeTimestamps {
		// If we want timestamps, just read from the inner stream as normal
		return tr.inner.Read(p)
	}

	toRead := len(p)

	read := 0
	for {
		if read >= toRead {
			break
		}

		if tr.maybeTimestamp {
			b, readErr := tr.reader.ReadByte()
			// We're buffering and hit a read error (including EOF); we assume the buffer is a timestamp and stop processing input
			if readErr != nil {
				return read, readErr
			}

			if unicode.IsSpace(rune(b)) {
				tr.maybeTimestamp = false
				// Parsing RFC3339 also handles versions of RFC3339 with sub-second precision, so we only do the one check.
				// This code specifically doesn't handle non-RFC3339 timestamps as those can include spaces and would be too difficult to properly parse.
				// Currently all log sources write in RFC3339 format, but if that changes, we would need to special case that scenario.
				if _, timeParseErr := time.Parse(time.RFC3339, string(tr.timestampBuffer.String())); timeParseErr != nil {
					// We honestly shouldn't run into this situation with any of our log sources, but someone could have a very customized container log setup
					writeErr := tr.timestampBuffer.WriteByte(b)
					if writeErr != nil {
						return read, writeErr
					}
				} else if b == '\r' || b == '\n' {
					// We found a timestamp followed by a newline, so we should discard the timestamp and write the newline character
					tr.timestampBuffer.Reset()
					p[read] = b
					read += 1
					continue
				} else {
					// We found a timestamp followed by a space; throw away the space and continue reading
					tr.timestampBuffer.Reset()
				}
			} else {
				writeErr := tr.timestampBuffer.WriteByte(b)
				if writeErr != nil {
					return read, writeErr
				}

				if tr.timestampBuffer.Len() > maxTimestampLength {
					tr.maybeTimestamp = false
				}
			}
		}

		if !tr.maybeTimestamp {
			if tr.timestampBuffer.Len() > 0 {
				b, readErr := tr.timestampBuffer.ReadByte()
				if readErr != nil {
					// If we hit an error reading from the buffer, we're in a bad state and should stop
					return read, readErr
				}

				if tr.timestampBuffer.Len() <= 0 {
					// Reset the buffer once we're done reading the current data
					tr.timestampBuffer.Reset()
				}

				p[read] = b
				read += 1
				if b == '\n' {
					tr.maybeTimestamp = true
				}
			} else {
				b, readErr := tr.reader.ReadByte()
				if readErr != nil {
					return read, readErr
				}

				p[read] = b
				read += 1
				if b == '\n' {
					tr.maybeTimestamp = true
				}
			}
		}
	}

	return read, nil
}

func (tr *timestampAwareReader) Close() error {
	if closer, isCloser := tr.inner.(io.Closer); isCloser {
		return closer.Close()
	}

	return nil
}
