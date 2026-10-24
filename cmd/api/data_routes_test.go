package main

import (
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

// TestMM24RoutesAreRegistered pins the route table the UI (MM25) consumes.
func TestMM24RoutesAreRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")

	jm := services.NewJobManager()
	conv := &handlers.ConversionHandler{}
	conv.SetDataHandler(handlers.NewDataHandler(nil, nil, jm, nil))
	conv.SetShareHandler(handlers.NewShareHandler(conv, nil, jm, &config.Config{}, nil))
	handlers.RegisterConversionRoutes(api, conv)

	studio := &handlers.StudioHandler{}
	handlers.RegisterStudioRoutes(api, studio)

	acct := handlers.NewAccountHandler(nil, nil, &config.Config{}, nil, nil, nil)
	acct.SetAccountData(handlers.NewAccountDataHandler(nil, nil, jm, nil))
	handlers.RegisterAccountRoutes(api, acct, &config.Config{}, nil)

	have := map[string]bool{}
	for _, ri := range r.Routes() {
		have[ri.Method+" "+ri.Path] = true
	}
	for _, want := range []string{
		"DELETE /api/jobs/:jobId",
		"PATCH /api/jobs/:jobId/retention",
		"DELETE /api/studio/projects/:id",
		"POST /api/account/data/delete-all",
		"GET /api/account/data/export",
		"POST /api/account/sessions/revoke",
		"DELETE /api/account",
		"GET /api/download/:jobId",
		"GET /api/result/:jobId",
		// MM32
		"POST /api/jobs/:jobId/share",
		"DELETE /api/jobs/:jobId/share",
		"GET /api/share/:token",
		"GET /api/share/:token/file",
	} {
		if !have[want] {
			t.Errorf("route not registered: %s", want)
		}
	}
}
