package redisstore

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
	"ha/envutil"
)

// Options collects Redis connection settings.
type Options struct {
	Addr     string
	Password string
	DB       int
}

// FromEnv builds Options using REDIS_ADDR, REDIS_PASSWORD, REDIS_DB.
func FromEnv() Options {
	db := 0
	if v := os.Getenv("REDIS_DB"); v != "" {
		fmt.Sscanf(v, "%d", &db)
	}
	return Options{
		Addr:     envutil.GetDefault("REDIS_ADDR", "127.0.0.1:6379"),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       db,
	}
}

// NewClient returns a configured redis client.
func NewClient(opts Options) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:         opts.Addr,
		Password:     opts.Password,
		DB:           opts.DB,
		DialTimeout:  1 * time.Second,
		ReadTimeout:  1 * time.Second,
		WriteTimeout: 500 * time.Millisecond,
		PoolSize:     100,
		MinIdleConns: 5,
		MaxRetries:   1,
	})
}

// Ping verifies connectivity with a short timeout.
func Ping(ctx context.Context, c *redis.Client) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := c.Ping(ctx).Result()
	return err
}
