package redis

import (
	"context"
	"fmt"
	"time"
)

// AllowFixedWindow counts one request against key in a fixed window. It
// returns 0 when allowed, or the time until the window resets.
func (c *Client) AllowFixedWindow(ctx context.Context, key string, limit int, window time.Duration) (time.Duration, error) {
	ms, err := fixedWindowScript.Run(ctx, c.rdb, []string{"ratelimit:" + key},
		window.Milliseconds(), limit).Int64()
	if err != nil {
		return 0, fmt.Errorf("check rate limit: %w", err)
	}
	return time.Duration(ms) * time.Millisecond, nil
}
