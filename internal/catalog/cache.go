package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	credis "github.com/duniandewon/madkunyah-be-service/platform/redis"
)

type docStore interface {
	CatalogGeneration(ctx context.Context) (int64, error)
	BumpCatalogGeneration(ctx context.Context) error
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

// readTimeout bounds each cache call on the read path. Without it a Redis
// outage costs every request the client's dial retries before it falls
// through to Postgres.
const readTimeout = 150 * time.Millisecond

// docCache caches menu and bundle documents under a global generation
// counter (FR-CACHE-1). Unlike auth it never fails closed: a Redis failure
// falls through to Postgres (NFR-AV-1). A nil store disables caching.
//
// The metrics in spec §7 are emitted as log lines with a "metric" field
// until Prometheus is wired up.
type docCache struct {
	store docStore
	ttl   time.Duration
}

// invalidate bumps the generation. Callers run it after their transaction
// commits (FR-CACHE-2); a failure is logged and the write still succeeds,
// leaving readers stale for at most the TTL.
func (c *docCache) invalidate(ctx context.Context) {
	if c.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bumpTimeout)
	defer cancel()
	if err := c.store.BumpCatalogGeneration(ctx); err != nil {
		slog.ErrorContext(ctx, "catalog cache generation bump failed",
			"metric", "catalog_cache_gen_bump_failures", "error", err)
	}
}

// afterWrite invalidates the cache when a write succeeded.
func (c *docCache) afterWrite(ctx context.Context, err error) {
	if err == nil {
		c.invalidate(ctx)
	}
}

// fetch returns the kind document for outletID under the current
// generation, building and storing it on a miss. The generation is read
// before build queries Postgres, so a document built from data older than a
// concurrent write lands under the old generation, which nobody reads again.
func (c *docCache) fetch(ctx context.Context, kind, outletID string, build func() ([]byte, error)) ([]byte, error) {
	if c.store == nil {
		return build()
	}
	gen, err := withTimeout(ctx, func(ctx context.Context) (int64, error) { return c.store.CatalogGeneration(ctx) })
	if err != nil {
		cacheResult(ctx, kind, "bypass", err)
		return build()
	}
	key := fmt.Sprintf("catalog:%s:%d:%s", kind, gen, outletID)
	b, err := withTimeout(ctx, func(ctx context.Context) ([]byte, error) { return c.store.GetCatalogDoc(ctx, key) })
	if err == nil {
		cacheResult(ctx, kind, "hit", nil)
		return b, nil
	}
	if !errors.Is(err, credis.ErrNotFound) {
		cacheResult(ctx, kind, "bypass", err)
		return build()
	}
	if b, err = build(); err != nil {
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
