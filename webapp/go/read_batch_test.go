package isureal

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestReadBatchBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var batch readBatch[int, int]
		var calls atomic.Int64
		var callers sync.WaitGroup
		load := func(_ context.Context, keys []int) ([]int, error) {
			if len(keys) > 128 {
				t.Errorf("oversized SQL array: %d", len(keys))
			}
			calls.Add(1)
			return slices.Clone(keys), nil
		}
		for i := range 314 {
			callers.Go(func() {
				got, err := batch.lookup(t.Context(), i, time.Millisecond, load)
				if err != nil || got != i {
					t.Errorf("request %d got %d, %v", i, got, err)
				}
			})
		}
		synctest.Wait()
		callers.Wait()
		if calls.Load() != 3 {
			t.Fatalf("314 simultaneous reads used %d queries", calls.Load())
		}
	})
}

func TestReadBatchCancellationAndSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var batch readBatch[string, int]
		var calls atomic.Int64
		release := make(chan struct{})
		started := make(chan struct{})
		load := func(ctx context.Context, keys []string) ([]int, error) {
			epoch := int(calls.Add(1))
			if epoch == 1 {
				close(started)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-release:
				}
			}
			out := make([]int, len(keys))
			for i := range out {
				out[i] = epoch
			}
			return out, nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		errs := make(chan error, 1)
		go func() { _, err := batch.lookup(ctx, "same", time.Millisecond, load); errs <- err }()
		first := make(chan int, 1)
		go func() { n, _ := batch.lookup(t.Context(), "same", time.Millisecond, load); first <- n }()
		<-started
		cancel()
		if !errors.Is(<-errs, context.Canceled) {
			t.Fatal("cancelled caller did not return")
		}
		// A request starting after a commit (while an older read is still in
		// flight) must get a new primary snapshot, even for an identical key.
		if n, err := batch.lookup(t.Context(), "same", time.Millisecond, load); n != 2 || err != nil {
			t.Fatalf("new request saw prior snapshot: %d %v", n, err)
		}
		close(release)
		if <-first != 1 {
			t.Fatal("cancelling one caller interrupted its live sibling")
		}
	})
}
