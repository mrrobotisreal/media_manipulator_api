// Package redisx wraps redis client construction so other packages get a
// consistent connection setup (timeouts, password, URL parsing).
package redisx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// New returns a redis client and pings it. When Redis is disabled and not
// strictly required, returns (nil, nil).
func New(ctx context.Context, cfg *config.Config, required bool) (*redis.Client, error) {
	if !cfg.RedisEnabled && !required {
		return nil, nil
	}
	rawURL := strings.TrimSpace(cfg.RedisURL)
	var client *redis.Client
	if rawURL != "" {
		opt, err := redis.ParseURL(rawURL)
		if err != nil {
			return nil, fmt.Errorf("parse REDIS_URL: %w", err)
		}
		client = redis.NewClient(opt)
	} else {
		client = redis.NewClient(&redis.Options{
			Addr:         cfg.RedisAddr,
			Password:     cfg.RedisPassword,
			DB:           cfg.RedisDB,
			DialTimeout:  2 * time.Second,
			ReadTimeout:  2 * time.Second,
			WriteTimeout: 2 * time.Second,
		})
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		if required {
			return nil, fmt.Errorf("redis ping: %w", err)
		}
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	if client == nil {
		return nil, errors.New("redis client construction returned nil")
	}
	return client, nil
}
