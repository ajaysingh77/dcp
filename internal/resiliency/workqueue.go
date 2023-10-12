package resiliency

import (
	"context"
	"sync"

	"github.com/smallnest/chanx"
)

type WorkQueueItem = func(ctx context.Context)

// WorkQueue runs work concurrently, but limits the number of concurrent executions.
type WorkQueue struct {
	incoming    *chanx.UnboundedChan[WorkQueueItem]
	limiter     chan struct{}
	lifetimeCtx context.Context
	lock        *sync.Mutex
	processing  bool
}

func NewWorkQueue(lifetimeCtx context.Context, maxConcurrency uint8) *WorkQueue {
	if maxConcurrency == 0 {
		panic("maxConcurrency must be greater than zero")
	}

	return &WorkQueue{
		// The maxConcurrency parameter used here indicates the initial size of the incoming work channel;
		// the channel itself is unbonunded.
		incoming: chanx.NewUnboundedChan[WorkQueueItem](lifetimeCtx, int(maxConcurrency)),

		limiter:     make(chan struct{}, maxConcurrency),
		lifetimeCtx: lifetimeCtx,
		lock:        &sync.Mutex{},
		processing:  false,
	}
}

func (wq *WorkQueue) Enqueue(work WorkQueueItem) error {
	if wq.lifetimeCtx.Err() != nil {
		return wq.lifetimeCtx.Err()
	}

	wq.incoming.In <- work
	wq.lock.Lock()
	defer wq.lock.Unlock()

	if !wq.processing {
		wq.processing = true
		go wq.doWork()
	}

	return nil
}

func (wq *WorkQueue) doWork() {
	resetProcessingFlag := func() {
		wq.lock.Lock()
		wq.processing = false
		wq.lock.Unlock()
	}

	for {
		select {

		case work := <-wq.incoming.Out:
			select {
			// Writing to limiter will block if attempting to start more goroutines than concurrency level (semaphore semantics).
			case wq.limiter <- struct{}{}:
				go func() {
					defer func() { <-wq.limiter }()
					work(wq.lifetimeCtx)
				}()

			// We want to stop the worker goroutine if the lifetime context is done (cancel the wait on writing to limiter).
			case <-wq.lifetimeCtx.Done():
				resetProcessingFlag()
				return
			}

		case <-wq.lifetimeCtx.Done():
			resetProcessingFlag()
			return

		default:
			// No work to do, stop the worker goroutine.
			resetProcessingFlag()
			return

		}

	}
}
