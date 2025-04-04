package concurrency

import "context"

type SyncChannel struct {
	ch chan struct{}
}

func NewSyncChannel() *SyncChannel {
	return &SyncChannel{
		ch: make(chan struct{}, 1),
	}
}

func (sc *SyncChannel) Lock(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case sc.ch <- struct{}{}:
	}

	// guard against possible race condition where the context expires and mutex locks at the same time
	if ctx.Err() != nil {
		sc.Unlock()
		return ctx.Err()
	}

	return nil
}

func (sc *SyncChannel) TryLock() bool {
	select {
	case sc.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (sc *SyncChannel) Unlock() {
	// Non-blocking for caller
	select {
	case <-sc.ch:
	default:
	}
}
