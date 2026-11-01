package redis

import (
	"context"
	"fmt"
	"time"
)

// catalogGenKey counts catalog writes. Cached documents are keyed by the
// generation they were built under, so bumping it invalidates them all.
const catalogGenKey = "catalog:gen"

// CatalogGeneration returns the current catalog generation, 0 before the
// first write.
func (c *Client) CatalogGeneration(ctx context.Context) (int64, error) {
	n, err := c.rdb.Get(ctx, catalogGenKey).Int64()
	if wrapNil(err) == ErrNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get catalog generation: %w", err)
	}
	return n, nil
}

func (c *Client) BumpCatalogGeneration(ctx context.Context) error {
	if err := c.rdb.Incr(ctx, catalogGenKey).Err(); err != nil {
		return fmt.Errorf("bump catalog generation: %w", err)
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
