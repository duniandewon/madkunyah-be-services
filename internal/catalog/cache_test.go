package catalog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
)

// Concurrent misses for one document share a single Postgres build, with
// Redis up (miss) and down (bypass).
func TestFetchSharesConcurrentBuilds(t *testing.T) {
	for _, down := range []bool{false, true} {
		t.Run(fmt.Sprintf("redis down=%v", down), func(t *testing.T) {
			store := newMemStore()
			store.down = down
			c := &docCache{store: store, ttl: time.Minute}
			var builds atomic.Int32
			release := make(chan struct{})
			build := func(context.Context) ([]byte, error) {
				builds.Add(1)
				<-release
				return []byte(`{}`), nil
			}
			const callers = 20
			var wg sync.WaitGroup
			errs := make(chan error, callers)
			for range callers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := c.fetch(context.Background(), docMenu, "outlet-1", build)
					errs <- err
				}()
			}
			time.Sleep(50 * time.Millisecond)
			close(release)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			if n := builds.Load(); n != 1 {
				t.Errorf("builds = %d, want 1", n)
			}
		})
	}
}

// A caller that hangs up stops waiting, but the shared build finishes for
// the others.
func TestFetchCallerCancelDoesNotFailOthers(t *testing.T) {
	c := &docCache{store: newMemStore(), ttl: time.Minute}
	release := make(chan struct{})
	build := func(ctx context.Context) ([]byte, error) {
		select {
		case <-release:
			return []byte(`{}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := c.fetch(ctx, docMenu, "outlet-1", build)
		first <- err
	}()
	time.Sleep(20 * time.Millisecond)
	second := make(chan error, 1)
	go func() {
		_, err := c.fetch(context.Background(), docMenu, "outlet-1", build)
		second <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled caller: err = %v", err)
	}
	close(release)
	if err := <-second; err != nil {
		t.Errorf("other caller: err = %v", err)
	}
}

func TestAfterWriteBumpsWhenCommitMayHaveLanded(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int64
	}{
		{"success", nil, 1},
		{"uncertain commit", fmt.Errorf("commit transaction: %w: %w", postgres.ErrCommitUncertain, context.Canceled), 1},
		{"rolled back", errors.New("conflict"), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMemStore()
			c := &docCache{store: store}
			c.afterWrite(context.Background(), tt.err)
			c.afterOutletWrite(context.Background(), "o", false, tt.err)
			g, o := store.bumps("o")
			if g != tt.want {
				t.Errorf("global bumps = %d, want %d", g, tt.want)
			}
			wantOutlet := int64(0)
			if errors.Is(tt.err, postgres.ErrCommitUncertain) {
				wantOutlet = 1
			}
			if o != wantOutlet {
				t.Errorf("outlet bumps = %d, want %d", o, wantOutlet)
			}
		})
	}
}

// A nil store disables caching: invalidation is a no-op, never a panic.
func TestNilStoreInvalidation(t *testing.T) {
	c := &docCache{}
	c.invalidate(context.Background())
	c.afterOutletWrite(context.Background(), "o", true, nil)
}
