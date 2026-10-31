package isureal

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// readBatch groups independent reads before their DB snapshot starts. No result
// is available to a request arriving after that snapshot (including retries).
type readBatch[K, V any] struct {
	mu      sync.Mutex
	pending []*readRequest[K, V]
}

type readRequest[K, V any] struct {
	ctx   context.Context
	key   K
	done  chan struct{}
	value V
	err   error
}

type readMany[K, V any] func(context.Context, []K) ([]V, error)

func (b *readBatch[K, V]) lookup(ctx context.Context, key K, delay time.Duration, load readMany[K, V]) (V, error) {
	req := &readRequest[K, V]{ctx: ctx, key: key, done: make(chan struct{})}
	b.mu.Lock()
	b.pending = append(b.pending, req)
	if len(b.pending) == 1 {
		time.AfterFunc(delay, func() { b.flush(load) })
	}
	b.mu.Unlock()
	select {
	case <-req.done:
		return req.value, req.err
	case <-ctx.Done():
		var zero V
		return zero, ctx.Err()
	}
}

func (b *readBatch[K, V]) flush(load readMany[K, V]) {
	b.mu.Lock()
	all := b.pending
	b.pending = nil
	b.mu.Unlock()
	live := all[:0]
	for _, req := range all {
		if req.ctx.Err() == nil {
			live = append(live, req)
		}
	}
	// Limit array sizes and let separate groups make progress on the DB pool.
	const limit = 128
	for len(live) > limit {
		go queryReads(live[:limit:limit], load)
		live = live[limit:]
	}
	if len(live) != 0 {
		queryReads(live, load)
	}
}

func queryReads[K, V any](group []*readRequest[K, V], load readMany[K, V]) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(group[0].ctx))
	defer cancel()
	var remaining atomic.Int64
	remaining.Store(int64(len(group)))
	drop := func() {
		if remaining.Add(-1) == 0 {
			cancel()
		}
	}
	keys := make([]K, len(group))
	stops := make([]func() bool, len(group))
	for i, req := range group {
		keys[i] = req.key
		stops[i] = context.AfterFunc(req.ctx, drop)
	}
	values, err := load(ctx, keys)
	for i, req := range group {
		stops[i]()
		req.err = err
		if err == nil {
			req.value = values[i]
		}
		close(req.done)
	}
}
