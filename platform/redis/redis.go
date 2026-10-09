package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

type Client struct {
	rdb *goredis.Client
}

type Config struct {
	Addr     string
	Password string
	DB       int
}

func Connect(ctx context.Context, cfg Config) (*Client, error) {
	rdb := goredis.NewClient(&goredis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &Client{rdb: rdb}, nil
}

func (c *Client) Raw() *goredis.Client { return c.rdb }
func (c *Client) Close() error         { return c.rdb.Close() }

func (c *Client) Health(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }

// ErrNotFound is returned when a key is absent, so callers do not import the
// driver just to compare against redis.Nil.
var ErrNotFound = fmt.Errorf("redis: key not found")

func wrapNil(err error) error {
	if err == goredis.Nil {
		return ErrNotFound
	}
	return err
}
