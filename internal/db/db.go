// Package db owns the pgxpool used by the rest of the service.
//
// We separate this from the telemetry package so unit tests can construct a
// fake pool without dragging in telemetry behavior.
package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// New opens a pgxpool against cfg.DatabaseURL.
//
// When cfg.TelemetryDBEnabled is false and the URL is empty we treat the DB
// as opt-in and return (nil, nil) so callers degrade gracefully.
func New(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	url := strings.TrimSpace(cfg.DatabaseURL)
	if !cfg.TelemetryDBEnabled && url == "" {
		return nil, nil
	}
	if url == "" {
		return nil, errors.New("DATABASE_URL is required when TELEMETRY_DB_ENABLED=true")
	}

	pcfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if pcfg.ConnConfig.RuntimeParams == nil {
		pcfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	pcfg.ConnConfig.RuntimeParams["timezone"] = "UTC"

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}
