package main

import (
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// vectorThumbnailS3Prefix is the first key segment of every Motion thumbnail
// (vector/<ownerUid>/<projectId>/thumb.png). The sweeper may delete under it.
const vectorThumbnailS3Prefix = "vector"

// retentionS3Prefixes is the S3 allowlist every sweeper is built with: the job
// uploads, results, Darkroom media and (MM43) Motion thumbnails.
func retentionS3Prefixes(cfg *config.Config) []string {
	return []string{"videos", cfg.S3ResultPrefix, cfg.ContentStudioS3Prefix, vectorThumbnailS3Prefix}
}

// newVectorHandler builds the Motion handler (MM43). With no database the
// handler still mounts and every route answers 503, like Darkroom.
func newVectorHandler(cfg *config.Config, store *telemetry.Store, registry *tiers.Registry, s3Client *s3.Client,
	conversion *handlers.ConversionHandler, log *slog.Logger) *handlers.VectorHandler {
	var vs handlers.VectorStore
	if store.Enabled() {
		vs = store
	}
	var presign handlers.ThumbnailPresigner
	if s3Client != nil && cfg.S3Bucket != "" {
		presign = handlers.S3ThumbnailPresigner{Client: s3.NewPresignClient(s3Client), Bucket: cfg.S3Bucket}
	}
	h := handlers.NewVectorHandler(vs, cfg, registry, presign, conversion, log)
	// Same fixed salt as the quota middleware, so an anonymous visitor without
	// a session id is keyed on the same hashed IP everywhere.
	h.SetIPHashSecret(quotaIPSalt[:])
	return h
}
