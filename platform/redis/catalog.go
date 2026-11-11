package redis

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// catalogGenKey counts catalog-wide writes, and outletGenKey the writes
// that touch one outlet only. Cached documents are keyed by both
// generations they were built under, so a catalog-wide bump invalidates
// every outlet and an outlet bump only that outlet.
const catalogGenKey = "catalog:gen"

func outletGenKey(outletID string) string { return "catalog:gen:" + outletID }

// CatalogGenerations returns the catalog-wide and the outlet's generation
// in one round trip, each 0 before its first write.
func (c *Client) CatalogGenerations(ctx context.Context, outletID string) (global, outlet int64, err error) {
	vals, err := c.rdb.MGet(ctx, catalogGenKey, outletGenKey(outletID)).Result()
	if err != nil {
		return 0, 0, fmt.Errorf("get catalog generations: %w", err)
	}
	var gens [2]int64
	for i, v := range vals {
		if v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return 0, 0, fmt.Errorf("get catalog generations: unexpected %T", v)
		}
		if gens[i], err = strconv.ParseInt(s, 10, 64); err != nil {
			return 0, 0, fmt.Errorf("get catalog generations: %w", err)
		}
	}
	return gens[0], gens[1], nil
}

func (c *Client) BumpCatalogGeneration(ctx context.Context) error {
	if err := c.rdb.Incr(ctx, catalogGenKey).Err(); err != nil {
		return fmt.Errorf("bump catalog generation: %w", err)
	}
	return nil
}

func (c *Client) BumpOutletCatalogGeneration(ctx context.Context, outletID string) error {
	if err := c.rdb.Incr(ctx, outletGenKey(outletID)).Err(); err != nil {
		return fmt.Errorf("bump outlet catalog generation: %w", err)
	}
	return nil
}

// GetCatalogDoc returns a cached catalog document, or ErrNotFound.
func (c *Client) GetCatalogDoc(ctx context.Context, key string) ([]byte, error) {
	b, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		if wrapNil(err) == ErrNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get catalog cache: %w", err)
	}
	return b, nil
}

func (c *Client) SetCatalogDoc(ctx context.Context, key string, b []byte, ttl time.Duration) error {
	if err := c.rdb.Set(ctx, key, b, ttl).Err(); err != nil {
		return fmt.Errorf("set catalog cache: %w", err)
	}
	return nil
}
