package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"golang.org/x/sync/singleflight"

	credis "github.com/duniandewon/madkunyah-be-service/platform/redis"
)

type docStore interface {
	CatalogGenerations(ctx context.Context, outletID string) (global, outlet int64, err error)
	BumpCatalogGeneration(ctx context.Context) error
	BumpOutletCatalogGeneration(ctx context.Context, outletID string) error
	GetCatalogDoc(ctx context.Context, key string) ([]byte, error)
	SetCatalogDoc(ctx context.Context, key string, b []byte, ttl time.Duration) error
}

// Cached document kinds (FR-CACHE-1).
const (
	docMenu   = "menu"
	docBundle = "bundle"
)

// bumpTimeout bounds the post-commit generation bump, which runs even if
// the request was cancelled after its write committed.
const bumpTimeout = time.Second

// buildTimeout bounds a shared document build, which outlives the request
// that started it.
const buildTimeout = 10 * time.Second

// readTimeout bounds each cache call on the read path. Without it a Redis
// outage costs every request the client's dial retries before it falls
// through to Postgres.
const readTimeout = 150 * time.Millisecond

// docCache caches menu and bundle documents under a catalog-wide and a
// per-outlet generation counter (FR-CACHE-1). Unlike auth it never fails
// closed: a Redis failure falls through to Postgres (NFR-AV-1). A nil store
// disables caching. Concurrent misses for the same document share one
// build, so an outage or a bump does not multiply the Postgres load.
//
// The metrics in spec §7 are emitted as log lines with a "metric" field
// until Prometheus is wired up.
type docCache struct {
	store  docStore
	ttl    time.Duration
	builds singleflight.Group
}

// invalidate bumps the catalog-wide generation, dropping every outlet's
// documents. Callers run it after their transaction commits (FR-CACHE-2);
// a failure is logged and the write still succeeds, leaving readers stale
// for at most the TTL.
func (c *docCache) invalidate(ctx context.Context) {
	c.bump(ctx, func(ctx context.Context) error { return c.store.BumpCatalogGeneration(ctx) })
}

// invalidateOutlet bumps one outlet's generation, for writes that only
// change that outlet's documents (channel settings, sold out).
func (c *docCache) invalidateOutlet(ctx context.Context, outletID string) {
	c.bump(ctx, func(ctx context.Context) error { return c.store.BumpOutletCatalogGeneration(ctx, outletID) })
}

func (c *docCache) bump(ctx context.Context, fn func(context.Context) error) {
	if c.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bumpTimeout)
	defer cancel()
	if err := fn(ctx); err != nil {
		slog.ErrorContext(ctx, "catalog cache generation bump failed",
			"metric", "catalog_cache_gen_bump_failures", "error", err)
	}
}

// afterWrite invalidates the cache when a write may have changed the
// catalog: it succeeded, or its commit outcome is unknown.
func (c *docCache) afterWrite(ctx context.Context, err error) {
	if mayHaveCommitted(err) {
		c.invalidate(ctx)
	}
}

// afterOutletWrite invalidates one outlet's documents when an outlet-scoped
// write changed something, or its commit outcome is unknown. A no-op write
// (a repeated PUT, a sold-out toggle that changes nothing) keeps the cache.
func (c *docCache) afterOutletWrite(ctx context.Context, outletID string, changed bool, err error) {
	if changed || errors.Is(err, postgres.ErrCommitUncertain) {
		c.invalidateOutlet(ctx, outletID)
	}
}

func mayHaveCommitted(err error) bool {
	return err == nil || errors.Is(err, postgres.ErrCommitUncertain)
}

// fetch returns the kind document for outletID under the current
// generations, building and storing it on a miss. The generations are read
// before build queries Postgres, so a document built from data older than a
// concurrent write lands under an old generation, which nobody reads again.
func (c *docCache) fetch(ctx context.Context, kind, outletID string, build buildFunc) ([]byte, error) {
	if c.store == nil {
		return build(ctx)
	}
	type gens struct{ global, outlet int64 }
	g, err := withTimeout(ctx, func(ctx context.Context) (gens, error) {
		global, outlet, err := c.store.CatalogGenerations(ctx, outletID)
		return gens{global, outlet}, err
	})
	if err != nil {
		cacheResult(ctx, kind, "bypass", err)
		return c.shared(ctx, "bypass:"+kind+":"+outletID, build)
	}
	key := fmt.Sprintf("catalog:%s:%d.%d:%s", kind, g.global, g.outlet, outletID)
	b, err := withTimeout(ctx, func(ctx context.Context) ([]byte, error) { return c.store.GetCatalogDoc(ctx, key) })
	if err == nil {
		cacheResult(ctx, kind, "hit", nil)
		return b, nil
	}
	if !errors.Is(err, credis.ErrNotFound) {
		cacheResult(ctx, kind, "bypass", err)
		return c.shared(ctx, "bypass:"+kind+":"+outletID, build)
	}
	if b, err = c.shared(ctx, key, build); err != nil {
		return nil, err
	}
	cacheResult(ctx, kind, "miss", nil)
	if _, err := withTimeout(ctx, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, c.store.SetCatalogDoc(ctx, key, b, c.ttl)
	}); err != nil {
		slog.WarnContext(ctx, "catalog cache write failed", "kind", kind, "error", err)
	}
	return b, nil
}

// buildFunc reads a document from Postgres.
type buildFunc func(ctx context.Context) ([]byte, error)

// shared runs build once for all concurrent callers with the same key.
// The build runs detached from any one caller's cancellation, bounded by
// buildTimeout, so a client that hangs up does not fail the others waiting
// on it; each caller still stops waiting when its own context ends.
func (c *docCache) shared(ctx context.Context, key string, build buildFunc) ([]byte, error) {
	ch := c.builds.DoChan(key, func() (any, error) {
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), buildTimeout)
		defer cancel()
		return build(bctx)
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.([]byte), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func withTimeout[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	return fn(ctx)
}

func cacheResult(ctx context.Context, kind, result string, err error) {
	if err != nil {
		slog.WarnContext(ctx, "catalog cache unavailable, reading postgres",
			"metric", "catalog_cache_reads", "kind", kind, "result", result, "error", err)
		return
	}
	slog.DebugContext(ctx, "catalog cache read", "metric", "catalog_cache_reads", "kind", kind, "result", result)
}
