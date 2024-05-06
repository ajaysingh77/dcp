package io

import (
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/microsoft/usvc-apiserver/pkg/testutil"
	"github.com/stretchr/testify/require"
)

func TestTimestampAwareReaderIncludesNonTimestampPrefix(t *testing.T) {
	t.Parallel()

	testText := "testinput "

	testReader := testutil.NewTestReader()
	testReader.AddEntry(testutil.AsByteTimelineEntries([]byte(testText)...)...)

	timestampReader := NewTimestampAwareReader(testReader, true)
	readBytes := make([]byte, len(testText))
	n, err := timestampReader.Read(readBytes)
	if err != nil && err != io.EOF {
		require.Fail(t, fmt.Sprintf("unexpected error: %s", err))
	}

	require.Equal(t, string(readBytes[:n]), testText)
}

func TestTimestampAwareReaderDoesNotIncludeTimestampPrefix(t *testing.T) {
	t.Parallel()

	expectedText := "this is the expected output"
	testText := time.Now().UTC().Format(time.RFC3339Nano) + " " + expectedText

	testReader := testutil.NewTestReader()
	testReader.AddEntry(testutil.AsByteTimelineEntries([]byte(testText)...)...)

	timestampReader := NewTimestampAwareReader(testReader, false)
	readBytes := make([]byte, len(testText))
	n, err := timestampReader.Read(readBytes)
	if err != nil && err != io.EOF {
		require.Fail(t, fmt.Sprintf("unexpected error: %s", err))
	}

	require.Equal(t, string(readBytes[:n]), expectedText)
}

func TestTimestampAwareReaderIncludesTimestampPrefixIfRequested(t *testing.T) {
	t.Parallel()

	expectedText := "this is the expected output"
	testText := time.Now().UTC().Format(time.RFC3339Nano) + " " + expectedText

	testReader := testutil.NewTestReader()
	testReader.AddEntry(testutil.AsByteTimelineEntries([]byte(testText)...)...)

	timestampReader := NewTimestampAwareReader(testReader, true)
	readBytes := make([]byte, len(testText))
	n, err := timestampReader.Read(readBytes)
	if err != nil && err != io.EOF {
		require.Fail(t, fmt.Sprintf("unexpected error: %s", err))
	}

	require.Equal(t, string(readBytes[:n]), testText)
}

func TestTimestampAwareReaderIgnoresTimestampWithNoSpace(t *testing.T) {
	t.Parallel()

	expectedText := "this is the expected output"
	testText := time.Now().UTC().Format(time.RFC3339Nano) + expectedText

	testReader := testutil.NewTestReader()
	testReader.AddEntry(testutil.AsByteTimelineEntries([]byte(testText)...)...)

	timestampReader := NewTimestampAwareReader(testReader, false)
	readBytes := make([]byte, len(testText))
	n, err := timestampReader.Read(readBytes)
	if err != nil && err != io.EOF {
		require.Fail(t, fmt.Sprintf("unexpected error: %s", err))
	}

	require.Equal(t, string(readBytes[:n]), testText)
}
