package main

import (
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"

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
	h := handlers.NewPhotoHandler(ps, cfg, ingest, nil)
	if store.Enabled() {
		h.SetPresetStore(store) // MM103
	}
	return h
}

// newPhotoPipeline wires the MM96 render and AI collaborators: the NumPy
// renderer (photo_render.py, under cmdaudit) and rembg/LaMa through MM94's GPU
// scheduler.
func newPhotoPipeline(cfg *config.Config, jm *services.JobManager, runner *cmdaudit.Runner) *handlers.PhotoPipeline {
	return &handlers.PhotoPipeline{
		Jobs:   jm,
		Render: services.NewPhotoRenderer(cfg, jm, runner),
		AI:     services.NewPhotoAI(cfg, jm),
	}
}
