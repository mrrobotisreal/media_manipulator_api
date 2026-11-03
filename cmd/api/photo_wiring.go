package main

import (
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// photoHandler is the /api/photo handler main builds; setupRouter mounts it. A
// package variable (like voicesHandler) so setupRouter's signature stays as
// every test spells it. nil mounts nothing.
var photoHandler *handlers.PhotoHandler

// newPhotoHandler wires Aperture (MM95): the project store (nil without a
// database: routes then answer 503), the ImageMagick-backed ingest with its
// PHOTO_INGEST_TIMEOUT_SECONDS budget, and the user-delete sweeper.
func newPhotoHandler(cfg *config.Config, store *telemetry.Store) *handlers.PhotoHandler {
	var ps handlers.PhotoStore
	if store.Enabled() {
		ps = store
	}
	ingest := services.NewPhotoIngest(time.Duration(cfg.PhotoIngestTimeoutSeconds) * time.Second)
	return handlers.NewPhotoHandler(ps, cfg, ingest, nil)
}
