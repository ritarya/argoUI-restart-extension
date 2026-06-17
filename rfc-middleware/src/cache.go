package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

type Cache struct {
	client *redis.Client
	ttl    time.Duration
}

func newCache(addr string, ttlSecs int) (*Cache, error) {
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &Cache{client: client, ttl: time.Duration(ttlSecs) * time.Second}, nil
}

func (c *Cache) Ping(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}

func (c *Cache) Get(ctx context.Context, key string) (*ValidationResult, bool) {
	data, err := c.client.Get(ctx, key).Bytes()
	if err != nil {
		return nil, false
	}
	var result ValidationResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, false
	}
	return &result, true
}

func (c *Cache) Set(ctx context.Context, key string, result *ValidationResult) {
	data, err := json.Marshal(result)
	if err != nil {
		return
	}
	c.client.Set(ctx, key, data, c.ttl)
}

func ttlSeconds() int {
	if s := os.Getenv("REDIS_TTL_SECONDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return 60
}
