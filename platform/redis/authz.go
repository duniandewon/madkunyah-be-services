package redis

import (
	"context"
	"fmt"
	"time"
)

func authzKey(staffID string) string { return "authz:staff:" + staffID }

// GetAccess returns a cached, encoded staff access record, or ErrNotFound.
func (c *Client) GetAccess(ctx context.Context, staffID string) ([]byte, error) {
	b, err := c.rdb.Get(ctx, authzKey(staffID)).Bytes()
	if err != nil {
		if wrapNil(err) == ErrNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get authz cache: %w", err)
	}
	return b, nil
}

func (c *Client) SetAccess(ctx context.Context, staffID string, b []byte, ttl time.Duration) error {
	if err := c.rdb.Set(ctx, authzKey(staffID), b, ttl).Err(); err != nil {
		return fmt.Errorf("set authz cache: %w", err)
	}
	return nil
}

func (c *Client) DeleteAccess(ctx context.Context, staffID string) error {
	if err := c.rdb.Del(ctx, authzKey(staffID)).Err(); err != nil {
		return fmt.Errorf("delete authz cache: %w", err)
	}
	return nil
}
