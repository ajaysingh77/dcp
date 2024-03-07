// Copyright (c) Microsoft Corporation. All rights reserved.

package logs

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/usvc-apiserver/pkg/randdata"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

func TestLogFollowingDelayWithinBounds(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	lds := NewLogDescriptorSet(ctx, testutil.TestTempRoot())
	uid, err := randdata.MakeRandomString(8)
	require.NoError(t, err)

	ld, stdoutWriter, _, created, err := lds.AcquireForResource(ctx, cancel,
		types.NamespacedName{
			Namespace: metav1.NamespaceNone,
			Name:      "test-log-following-delay-within-bounds",
		},
		types.UID(uid),
	)
	require.NoError(t, err)
	require.True(t, created)
	defer lds.ReleaseForResource(types.UID(uid))

	stdOutPath, _, err := ld.LogConsumerStarting()
	require.NoError(t, err)
	defer ld.LogConsumerStopped()

	// Make 8 writes at the span of 4 * logReadRetryInterval.
	buf := testutil.NewBufferWriter(4096) // big enough to hold all writes
	const numWrites = 8
	const writeDelay = 4 * logReadRetryInterval / numWrites
	watcherDone := make(chan struct{})

	go func() {
		err := WatchLogs(ctx, stdOutPath, buf, WatchLogOptions{Follow: true})
		require.NoError(t, err)
		close(watcherDone)
	}()

	var logWriteTimes []time.Time
	for i := 0; i < numWrites; i++ {
		logWriteTimes = append(logWriteTimes, time.Now())
		_, err := stdoutWriter.Write([]byte(fmt.Sprintf("%d\n", i)))
		require.NoError(t, err)
		time.Sleep(writeDelay)
	}

	// Wait for all writes to be captured.
	err = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(_ context.Context) (bool, error) {
		lines := bytes.Count(buf.Bytes(), []byte("\n"))
		return lines == numWrites, nil
	})
	require.NoError(t, err)

	stdoutWriter.Close()
	<-watcherDone

	// Each write should be captured after no more that logReadRetryInterval + 200 ms.
	const maxDiscrepancy = logReadRetryInterval + 200*time.Millisecond
	for i := 0; i < numWrites; i++ {
		require.WithinDuration(t, logWriteTimes[i], buf.Chunks()[i].Timestamp, maxDiscrepancy,
			"Write %d was not captured within the expected time frame. Written at %s, captured at %s",
			i, logWriteTimes[i].Format(time.TimeOnly), buf.Chunks()[i].Timestamp.Format(time.TimeOnly))
	}

}
