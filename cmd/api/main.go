package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/mrrobotisreal/media_manipulator_api/internal/cleanup"
	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/db"
	"github.com/mrrobotisreal/media_manipulator_api/internal/geo"
	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/limits"
	"github.com/mrrobotisreal/media_manipulator_api/internal/logger"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metrics"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/redisx"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

func loadDotEnv() {
	_ = godotenv.Load(".env")
	_ = godotenv.Load(".env.local")
}

func main() {
	loadDotEnv()
	cfg := config.Load()
	logging := logger.New(cfg)
	slog.SetDefault(logging)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	createDirs(cfg)

	// Run migrations on boot if configured. We don't fail the API on a
	// migration error so local dev still starts when the DB is offline —
	// but we log loudly.
	if cfg.APIAutoMigrate {
		if err := runMigrationsAtBoot(cfg, logging); err != nil {
			logging.Error("api auto-migrate failed", "error", err.Error())
		}
	}

	// Postgres
	pool, err := db.New(ctx, cfg)
	if err != nil {
		if cfg.TelemetryDBEnabled {
			logging.Error("database unavailable; continuing with telemetry disabled", "error", err.Error())
		}
	}
	if pool != nil {
		defer pool.Close()
		// One-shot clock-skew sentinel: DR timestamps come from Postgres now(),
		// which inherits the host system clock. If the DB and app clocks disagree,
		// created_at values (and the UI's relative times) will be wrong.
		checkDBClockSkew(ctx, pool, logging)
	}

	// Redis
	redisClient, err := redisx.New(ctx, cfg, cfg.RateLimitEnabled)
	if err != nil {
		if cfg.RateLimitEnabled {
			logging.Error("redis unavailable; rate limiting will fail-open", "error", err.Error())
		} else {
			logging.Warn("redis unavailable; continuing without it", "error", err.Error())
		}
	}
	if redisClient != nil {
		defer redisClient.Close()
	}

	// MaxMind / geo
	enricher, err := geo.Open(cfg, redisClient)
	if err != nil {
		logging.Error("failed to open MaxMind readers", "error", err.Error())
	}
	if enricher != nil {
		defer enricher.Close()
	}

	store := telemetry.NewStore(pool, cfg, logging)

	// Command audit runner. Even when the telemetry DB is offline we still
	// build a runner so callers can switch over without conditional code.
	sanitizer := cmdaudit.NewPathSanitizer(cfg.UploadDir, cfg.OutputDir, cfg.TempDir)
	var auditSink cmdaudit.AuditSink = cmdaudit.NopSink{}
	if store.Enabled() && cfg.TelemetryAuditCommands {
		auditSink = store
	}
	cmdRunner := cmdaudit.NewRunner(sanitizer, auditSink)

	// Metrics + GPU + rate limiter
	metricsReg := metrics.New()
	limiter := limits.New(redisClient, cfg, store, metricsReg)
	gpuMgr := gpu.NewManager(cfg, store, metricsReg, logging)

	// Existing services
	jobManager := services.NewJobManager()
	converter := services.NewConverter(cfg)
	inspector := services.NewMediaInspector(cfg.CommandTimeout)
	analysisQueue := services.NewAnalysisQueue(cfg, inspector)
	if hookable, ok := any(analysisQueue).(interface {
		SetTelemetry(store *telemetry.Store, sanitizer *cmdaudit.PathSanitizer, enricher *geo.Enricher, cfg *config.Config)
	}); ok {
		hookable.SetTelemetry(store, sanitizer, enricher, cfg)
	}
	analysisQueue.Start()

	transcription := services.NewTranscriptionService(cfg, inspector, jobManager, analysisQueue)
	s3Client := newS3Client(cfg)
	faceDetectionStore := services.NewFaceDetectionStore(30 * time.Minute)
	conversionHandler := handlers.NewConversionHandler(jobManager, converter, cfg, inspector, analysisQueue, transcription, s3Client, faceDetectionStore)
	// Content Studio gets its own handler because it persists projects/assets in
	// Postgres (the conversion handler is stateless). It shares the jobManager so
	// ingest/export progress flows through the same /api/job/:jobId machinery.
	studioHandler := handlers.NewStudioHandler(jobManager, cfg, inspector, s3Client, pool, transcription)
	// AI Video Restoration gets a dedicated handler too: its pipeline is the
	// first consumer of the GPU lease manager and the command-audit runner.
	videoRestoreHandler := handlers.NewVideoRestoreHandler(jobManager, cfg, s3Client, gpuMgr, store, cmdRunner)
	// AI Image Restoration & Upscaling — the still-image sibling of video
	// restoration. No S3 dependency (images are small, uploaded directly); it
	// shares the GPU lease manager + command-audit runner.
	imageRestoreHandler := handlers.NewImageRestoreHandler(jobManager, cfg, inspector, gpuMgr, store, cmdRunner)
	// AI Document Scan — scanned printed documents AND handwritten notes →
	// searchable PDF + optional structured DOCX. PUBLIC (no Firebase), mounted on
	// the plain /api group like the conversion routes; shares the GPU lease
	// manager + command-audit runner.
	documentScanHandler := handlers.NewDocumentScanHandler(jobManager, cfg, inspector, gpuMgr, store, cmdRunner)
	// Double Raven partner portal — Postgres-backed document API (GET /api/dr/docs
	// [, /:slug]) plus the in-portal "Create Doc" editor (draft create/update/
	// publish + media asset presign/complete/delete). Shares the same pgx pool as
	// Content Studio and the same S3 client + config as the DR comments handler
	// for the presign→PUT→complete asset handshake; always Firebase-gated at the
	// /dr group (see the DR verifier + group below).
	drDocsHandler := handlers.NewDrDocsHandler(pool, cfg, s3Client)
	// DR document comments (v2): comment/reply/attachment endpoints on the same
	// /api/dr group. Reuses the shared S3 client + pool for the presign→PUT→
	// complete attachment handshake and the draft reaper.
	drCommentsHandler := handlers.NewDrCommentsHandler(pool, cfg, s3Client)
	// DR Communication/Feedback (Slack-style messaging at /dr/feedback):
	// conversations/messages/threads/attachments + an in-memory SSE nudge stream.
	// Same /api/dr group + shared pool/S3/config. The root ctx is passed so the
	// SSE stream can exit promptly on graceful shutdown; the in-memory
	// broadcaster is single-process by design (the API runs as one process).
	drFeedbackHandler := handlers.NewDrFeedbackHandler(ctx, pool, cfg, s3Client)
	// DR AI Chat Test Lab (/dr/demos/chat-lab): OpenRouter-backed chat with
	// per-message model + reasoning-effort selection, streamed over SSE from
	// this API (the key never leaves the home server). Same /api/dr group +
	// shared pool/S3/config; the root ctx lets the streaming send exit promptly
	// on graceful shutdown. With OPENROUTER_API_KEY unset the chat endpoints
	// fail closed (503).
	drChatLabHandler := handlers.NewDrChatLabHandler(ctx, pool, cfg, s3Client)

	// Future auth seam (default OFF): when RESTORE_REQUIRE_FIREBASE_AUTH is
	// set, /api/video-restore/* verifies Firebase ID tokens. Init failure
	// leaves the verifier nil and the middleware fails CLOSED on that group.
	var restoreAuthVerifier middleware.TokenVerifier
	if cfg.RestoreRequireFirebaseAuth {
		verifier, err := middleware.NewFirebaseVerifier(ctx, cfg.FirebaseProjectID)
		if err != nil {
			logging.Error("firebase auth init failed; /api/video-restore/* will reject all requests", "error", err.Error())
		} else {
			restoreAuthVerifier = verifier
		}
	}

	// Double Raven portal auth (ALWAYS on, independent of
	// RESTORE_REQUIRE_FIREBASE_AUTH): initialize a Firebase claims verifier for
	// /api/dr/*. On init failure we log and continue with a nil verifier — the
	// middleware fails CLOSED (503) rather than exposing the document store.
	var drVerifier middleware.ClaimsVerifier
	if verifier, err := middleware.NewFirebaseClaimsVerifier(ctx, cfg.FirebaseProjectID); err != nil {
		logging.Error("double raven auth init failed; /api/dr/* will reject all requests (503)", "error", err.Error())
	} else {
		drVerifier = verifier
	}
	if len(cfg.DRAllowedEmails) == 0 {
		logging.Warn("DR_ALLOWED_EMAILS is empty; any verified Firebase user of the project can access /api/dr/* — set DR_ALLOWED_EMAILS to restrict the Double Raven portal")
	}

	// Cleanup worker
	if cfg.CleanupEnabled {
		worker := cleanup.NewWorker(cfg, store, metricsReg, logging, jobManager)
		go worker.Run(ctx)
	}

	// Daily reaper for abandoned DR comment drafts + orphaned pending
	// attachments (see DrCommentsHandler.ReapStaleDrafts). DB-gated.
	if pool != nil {
		go runDrCommentReaper(ctx, drCommentsHandler)
		// Companion reaper for orphaned pending document assets (uploads never
		// completed). Draft documents themselves are never reaped.
		go runDrDocAssetReaper(ctx, drDocsHandler)
		// Companion reaper for unbound feedback message attachments (uploaded
		// while composing but never bound to a sent message). Bound attachments
		// are never reaped.
		go runDrFeedbackAttachmentReaper(ctx, drFeedbackHandler)
		// Companion reaper for unbound chat-lab attachments (uploaded while
		// composing but never bound to a sent chat message).
		go runDrChatLabAttachmentReaper(ctx, drChatLabHandler)
		// Nightly hash-gated project-memory scheduler (default 4 AM
		// America/Denver): regenerates memory only for projects whose content
		// fingerprint changed since the last successful generation.
		go runDrChatLabMemoryScheduler(ctx, drChatLabHandler, cfg)
	}

	// Periodic active-jobs gauge update.
	go pollActiveJobs(ctx, jobManager, metricsReg)

	router := setupRouter(cfg, conversionHandler, studioHandler, videoRestoreHandler, imageRestoreHandler, documentScanHandler, restoreAuthVerifier, drVerifier, drDocsHandler, drCommentsHandler, drFeedbackHandler, drChatLabHandler, store, enricher, limiter, metricsReg)

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 15 * time.Second,
	}

	// pprof on a separate admin bind if requested.
	var adminServer *http.Server
	if cfg.PProfEnabled && strings.TrimSpace(cfg.AdminDebugBindAddr) != "" {
		adminServer = startAdminServer(cfg, logging)
	} else if cfg.PProfEnabled {
		mountPProf(router)
		logging.Warn("pprof endpoints mounted on the main router — do NOT expose this in production")
	}

	go func() {
		logging.Info("media-manipulator-api listening", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	logging.Info("shutting down http server")
	if err := server.Shutdown(shutdownCtx); err != nil {
		logging.Error("graceful shutdown failed", "error", err.Error())
	}
	if adminServer != nil {
		_ = adminServer.Shutdown(shutdownCtx)
	}
}

func newS3Client(cfg *config.Config) *s3.Client {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}
	return s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		if cfg.S3Endpoint != "" {
			options.BaseEndpoint = aws.String(cfg.S3Endpoint)
		}
	})
}

func setupRouter(cfg *config.Config, conversionHandler *handlers.ConversionHandler, studioHandler *handlers.StudioHandler, videoRestoreHandler *handlers.VideoRestoreHandler, imageRestoreHandler *handlers.ImageRestoreHandler, documentScanHandler *handlers.DocumentScanHandler, restoreAuthVerifier middleware.TokenVerifier, drVerifier middleware.ClaimsVerifier, drDocsHandler *handlers.DrDocsHandler, drCommentsHandler *handlers.DrCommentsHandler, drFeedbackHandler *handlers.DrFeedbackHandler, drChatLabHandler *handlers.DrChatLabHandler, store *telemetry.Store, enricher *geo.Enricher, limiter *limits.Limiter, m *metrics.Registry) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	router := gin.Default()
	_ = router.SetTrustedProxies([]string{"127.0.0.1", "::1"})

	corsConfig := cors.DefaultConfig()
	corsConfig.AllowOrigins = []string{
		"https://media-manipulator.com",
		"https://www.media-manipulator.com",
		// Restricted AI Video Restoration deployment (Firebase-gated when
		// RESTORE_REQUIRE_FIREBASE_AUTH is enabled).
		"https://dr.media-manipulator.com",
		"http://localhost:3000",
		// Standalone Double Raven portal — Electron desktop app (fixed local port).
		"http://localhost:41999",
		// Standalone Double Raven portal — web deployment on Vercel.
		"https://drportal.wintrow.dev",
	}
	// PUT is required by the Content Studio project save (PUT /api/studio/projects/:id);
	// PATCH/DELETE are allowed too so the editor's CRUD surface doesn't trip CORS.
	corsConfig.AllowMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
	corsConfig.AllowHeaders = []string{
		"Origin",
		"Content-Type",
		"Accept",
		"Authorization",
		"X-Requested-With",
		"X-MM-Visitor-ID",
		"X-MM-Session-ID",
		// Range lets the Content Studio preview proxy be scrubbed cross-origin
		// from a <video crossorigin="anonymous"> element (needed for Web Audio).
		"Range",
	}
	corsConfig.AllowCredentials = false
	// Expose the byte-range response headers so cross-origin <video> seeking and
	// the Content Studio proxy passthrough work.
	corsConfig.ExposeHeaders = []string{"Content-Length", "Content-Disposition", "Content-Range", "Accept-Ranges", "X-MM-Request-ID"}
	router.Use(cors.New(corsConfig))
	router.Use(middleware.RequestContext())
	router.Use(middleware.AccessLog(store, enricher))
	router.Use(m.Middleware())
	// Global per-IP rate limit guard.
	router.Use(limiter.GlobalIPRPS())

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "healthy", "service": "media_manipulator_api"})
	})

	if cfg.MetricsEnabled {
		router.GET("/metrics", gin.WrapH(promhttp.HandlerFor(m.Reg, promhttp.HandlerOpts{})))
	}

	api := router.Group("/api")
	{
		api.GET("/health", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "healthy", "service": "media_manipulator_api"})
		})
		// Conversion routes — register first so per-route rate limits can
		// be layered onto specific groups.
		handlers.RegisterConversionRoutes(api, conversionHandler)
		// Specialized tool endpoints that don't fit cleanly into the
		// single-file /upload contract (caption translator takes .srt/.vtt
		// text files; stitch-audio-to-video takes multi-file multipart).
		handlers.RegisterToolRoutes(api, conversionHandler)
		// Content Studio (browser NLE) endpoints — projects/assets/export.
		handlers.RegisterStudioRoutes(api, studioHandler)
		// AI Video Restoration (multi-model comparison pipeline). The group
		// carries the Firebase auth seam — a pass-through no-op while
		// RESTORE_REQUIRE_FIREBASE_AUTH is off (the default).
		restoreGroup := api.Group("")
		restoreGroup.Use(middleware.RequireFirebaseAuth(cfg.RestoreRequireFirebaseAuth, restoreAuthVerifier))
		handlers.RegisterVideoRestoreRoutes(restoreGroup, videoRestoreHandler)
		// AI Image Restoration shares the same Firebase-gated group so
		// RESTORE_REQUIRE_FIREBASE_AUTH gates it identically.
		handlers.RegisterImageRestoreRoutes(restoreGroup, imageRestoreHandler)
		// AI Document Scan is PUBLIC (like the conversion/tool routes) — mounted
		// on the plain /api group, NOT behind the Firebase-gated restoreGroup.
		handlers.RegisterDocumentScanRoutes(api, documentScanHandler)

		// Double Raven partner portal document API. ALWAYS Firebase-gated and
		// fail-closed via RequireDoubleRavenAuth (nil verifier -> 503; bad/missing
		// token -> 401; not on DR_ALLOWED_EMAILS -> 403). Deliberately NOT the
		// toggleable restoreGroup seam — the DR group has no pass-through mode.
		// GET-only for two users, so no dedicated rate-limit bucket (the global
		// per-IP RPS limiter still applies).
		drGroup := api.Group("/dr")
		drGroup.Use(middleware.RequireDoubleRavenAuth(drVerifier, cfg.DRAllowedEmails))
		handlers.RegisterDrDocsRoutes(drGroup, drDocsHandler)
		handlers.RegisterDrCommentsRoutes(drGroup, drCommentsHandler)
		// DR Communication/Feedback (Slack-style messaging) — conversations,
		// messages, threads, attachments, and the SSE nudge stream, all on the
		// same always-Firebase-gated /api/dr group.
		handlers.RegisterDrFeedbackRoutes(drGroup, drFeedbackHandler)
		// DR AI Chat Test Lab — model catalog, chat sessions/messages (the
		// send streams SSE over POST), and attachments, on the same group.
		handlers.RegisterDrChatLabRoutes(drGroup, drChatLabHandler)

		// Tighter limits for upload/transcode/analysis paths.
		api.Use() // marker; per-route limiters below
		registerLimitedRoute := func(path string, h gin.HandlerFunc, sessionLimit, ipLimit int, tool string) {
			api.POST(path, limiter.Route(strings.ReplaceAll(strings.TrimPrefix(path, "/"), "/", "_"), tool, sessionLimit, ipLimit), h)
		}
		_ = registerLimitedRoute // wired below for limiter middlewares applied on existing routes

		telemetryHandler := handlers.NewTelemetryHandler(store, enricher)
		telemetryHandler.Register(api)
	}

	// Per-route limiters: we attach extra-strict limits via a second
	// router group so the original handler logic is untouched. We re-mount
	// the limited endpoints under the same paths; gin allows multiple
	// handlers per (method, path), so the order matters: this group sits
	// behind the conversion routes already registered above. To preserve
	// behavior we set the limiter as middleware on a fresh "guarded" view
	// that does nothing on its own — limiter aborts on 429.
	guard := func(routeKey, tool string, sessionLimit, ipLimit int) gin.HandlerFunc {
		return limiter.Route(routeKey, tool, sessionLimit, ipLimit)
	}
	// Wrap by replacing handler chains is complex; instead we add limiter
	// middleware via Use on a child group bound to specific paths via
	// path matching middleware.
	router.Use(routeLimitDispatcher(cfg, limiter, guard))

	return router
}

// routeLimitDispatcher applies per-route limits using the path on the
// incoming request, before the route handler runs. This avoids re-registering
// existing conversion routes.
func routeLimitDispatcher(cfg *config.Config, limiter *limits.Limiter, guard func(routeKey, tool string, sessionLimit, ipLimit int) gin.HandlerFunc) gin.HandlerFunc {
	type rule struct {
		path         string
		routeKey     string
		tool         string
		sessionLimit int
		ipLimit      int
		// matches optionally overrides the default exact-path comparison. Used
		// for parameterized routes (e.g. /studio/projects/:id/export) where the
		// concrete path varies per request.
		matches func(path string) bool
	}
	rules := []rule{
		{path: "/api/upload", routeKey: "upload", tool: "upload", sessionLimit: cfg.RateLimitUploadsPerSessionPerHour, ipLimit: cfg.RateLimitUploadsPerIPPerHour},
		{path: "/api/video-upload/presign", routeKey: "video_upload_presign", tool: "video_upload", sessionLimit: cfg.RateLimitUploadsPerSessionPerHour, ipLimit: cfg.RateLimitUploadsPerIPPerHour},
		{path: "/api/video-upload/complete", routeKey: "video_upload_complete", tool: "video_upload", sessionLimit: cfg.RateLimitUploadsPerSessionPerHour, ipLimit: cfg.RateLimitUploadsPerIPPerHour},
		{path: "/api/video-transcode/start", routeKey: "video_transcode_start", tool: "video_transcode", sessionLimit: cfg.RateLimitTranscodesPerSessionPerHour, ipLimit: cfg.RateLimitTranscodesPerIPPerHour},
		// AI Video Restoration is GPU- and disk-hungry (up to six models per
		// job) — it gets its own, much tighter bucket.
		{path: "/api/video-restore/start", routeKey: "video_restore_start", tool: "video_restore", sessionLimit: cfg.RestoreRateLimitPerSessionPerHour, ipLimit: cfg.RestoreRateLimitPerIPPerHour},
		{path: "/api/image-restore/start", routeKey: "image_restore_start", tool: "image_restore", sessionLimit: cfg.ImageRestoreRateLimitPerSessionPerHour, ipLimit: cfg.ImageRestoreRateLimitPerIPPerHour},
		{path: "/api/document-scan/start", routeKey: "document_scan_start", tool: "document_scan", sessionLimit: cfg.DocumentScanRateLimitPerSessionPerHour, ipLimit: cfg.DocumentScanRateLimitPerIPPerHour},
		{path: "/api/video-transcode/probe", routeKey: "video_transcode_probe", tool: "video_transcode", sessionLimit: cfg.RateLimitTranscodesPerSessionPerHour, ipLimit: cfg.RateLimitTranscodesPerIPPerHour},
		{path: "/api/ai/faces/detect", routeKey: "ai_faces_detect", tool: "ai_faces", sessionLimit: cfg.RateLimitAnalysisPerSessionPerHour, ipLimit: cfg.RateLimitAnalysisPerIPPerHour},
		// Caption translator runs the local Ollama LLM — treat it like analysis
		// usage (the model competes for GPU time with whisper).
		{path: "/api/tools/caption-translator", routeKey: "tools_caption_translator", tool: "caption_translator", sessionLimit: cfg.RateLimitAnalysisPerSessionPerHour, ipLimit: cfg.RateLimitAnalysisPerIPPerHour},
		// Stitch-audio-to-video uploads + transcodes — share the upload bucket.
		{path: "/api/tools/stitch-audio-to-video", routeKey: "tools_stitch_audio_to_video", tool: "stitch_audio_to_video", sessionLimit: cfg.RateLimitUploadsPerSessionPerHour, ipLimit: cfg.RateLimitUploadsPerIPPerHour},
		// Content Studio: source ingest shares the upload bucket; the EDL export
		// (NVENC transcode) shares the transcode bucket.
		{path: "/api/studio/assets/presign", routeKey: "studio_assets_presign", tool: "studio_upload", sessionLimit: cfg.RateLimitUploadsPerSessionPerHour, ipLimit: cfg.RateLimitUploadsPerIPPerHour},
		{path: "/api/studio/assets/complete", routeKey: "studio_assets_complete", tool: "studio_upload", sessionLimit: cfg.RateLimitUploadsPerSessionPerHour, ipLimit: cfg.RateLimitUploadsPerIPPerHour},
		{
			routeKey:     "studio_project_export",
			tool:         "studio_export",
			sessionLimit: cfg.RateLimitTranscodesPerSessionPerHour,
			ipLimit:      cfg.RateLimitTranscodesPerIPPerHour,
			matches: func(path string) bool {
				return strings.HasPrefix(path, "/api/studio/projects/") && strings.HasSuffix(path, "/export")
			},
		},
		// AI-derived audio (DeepFilter/Demucs) competes with whisper for GPU —
		// rate-limit it like the analysis routes.
		{
			routeKey:     "studio_asset_derive",
			tool:         "studio_derive",
			sessionLimit: cfg.RateLimitAnalysisPerSessionPerHour,
			ipLimit:      cfg.RateLimitAnalysisPerIPPerHour,
			matches: func(path string) bool {
				return strings.HasPrefix(path, "/api/studio/assets/") && strings.HasSuffix(path, "/derive")
			},
		},
		// Caption generation runs whisper — share the analysis bucket.
		{
			routeKey:     "studio_captions_generate",
			tool:         "studio_captions",
			sessionLimit: cfg.RateLimitAnalysisPerSessionPerHour,
			ipLimit:      cfg.RateLimitAnalysisPerIPPerHour,
			matches: func(path string) bool {
				return strings.HasPrefix(path, "/api/studio/projects/") && strings.HasSuffix(path, "/captions/generate")
			},
		},
	}
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}
		path := c.Request.URL.Path
		for _, r := range rules {
			matched := r.path != "" && path == r.path
			if r.matches != nil {
				matched = r.matches(path)
			}
			if matched {
				guard(r.routeKey, r.tool, r.sessionLimit, r.ipLimit)(c)
				return
			}
		}
		c.Next()
	}
}

func mountPProf(r *gin.Engine) {
	debug := r.Group("/debug/pprof")
	debug.GET("/", gin.WrapF(pprof.Index))
	debug.GET("/cmdline", gin.WrapF(pprof.Cmdline))
	debug.GET("/profile", gin.WrapF(pprof.Profile))
	debug.POST("/symbol", gin.WrapF(pprof.Symbol))
	debug.GET("/symbol", gin.WrapF(pprof.Symbol))
	debug.GET("/trace", gin.WrapF(pprof.Trace))
	debug.GET("/allocs", gin.WrapF(pprof.Handler("allocs").ServeHTTP))
	debug.GET("/block", gin.WrapF(pprof.Handler("block").ServeHTTP))
	debug.GET("/goroutine", gin.WrapF(pprof.Handler("goroutine").ServeHTTP))
	debug.GET("/heap", gin.WrapF(pprof.Handler("heap").ServeHTTP))
	debug.GET("/mutex", gin.WrapF(pprof.Handler("mutex").ServeHTTP))
	debug.GET("/threadcreate", gin.WrapF(pprof.Handler("threadcreate").ServeHTTP))
}

// startAdminServer starts a separate http.Server on cfg.AdminDebugBindAddr
// hosting /debug/pprof. We isolate pprof here so it never accidentally lands
// on the public port.
func startAdminServer(cfg *config.Config, logging *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	admin := &http.Server{
		Addr:              cfg.AdminDebugBindAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	logging.Warn("admin pprof server listening — do NOT expose publicly", "addr", admin.Addr)
	go func() {
		if err := admin.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Error("admin server: " + err.Error())
		}
	}()
	return admin
}

func createDirs(cfg *config.Config) {
	for _, dir := range []string{cfg.UploadDir, cfg.OutputDir, cfg.TempDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("failed to create directory %s: %v", dir, err)
		}
	}
}

// pollActiveJobs updates the mm_active_jobs gauge once per second.
func pollActiveJobs(ctx context.Context, jm *services.JobManager, m *metrics.Registry) {
	if m == nil || jm == nil {
		return
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.SetActiveJobs(jm.ActiveCount())
		}
	}
}

// checkDBClockSkew compares the database clock (Postgres now()) against the
// application clock once at startup. On a same-box deployment these are within
// milliseconds; a delta over 5s means the host system clock is wrong, which
// would make every created_at/updated_at (and thus the UI's relative
// timestamps) incorrect. We log a loud WARN rather than failing — it's an
// operational fix (NTP/RTC), not a code bug — so it's immediately visible in
// the logs. One query at startup, never per-request.
func checkDBClockSkew(ctx context.Context, pool *pgxpool.Pool, logging *slog.Logger) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var dbNow time.Time
	if err := pool.QueryRow(cctx, "SELECT now()").Scan(&dbNow); err != nil {
		logging.Warn("clock-skew check failed; could not read database time", "error", err.Error())
		return
	}
	delta := time.Since(dbNow)
	if delta < 0 {
		delta = -delta
	}
	if delta > 5*time.Second {
		logging.Warn("database clock and application clock disagree; timestamps (created_at etc.) will be wrong until the system clock is fixed",
			"db_now", dbNow.UTC().Format(time.RFC3339),
			"app_now", time.Now().UTC().Format(time.RFC3339),
			"delta", delta.String())
	}
}

// runDrCommentReaper runs the DR comment draft reaper shortly after boot and
// then daily, until ctx is cancelled. Mirrors the ticker pattern of the cleanup
// worker but is DR-specific (needs the pgx pool + S3 client the handler holds).
func runDrCommentReaper(ctx context.Context, h *handlers.DrCommentsHandler) {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			h.ReapStaleDrafts(ctx)
			timer.Reset(24 * time.Hour)
		}
	}
}

// runDrDocAssetReaper runs the DR document-asset reaper shortly after boot and
// then daily, until ctx is cancelled. Mirrors runDrCommentReaper; it removes
// only orphaned 'pending' assets (uploads that never completed) — draft
// documents are intentionally left alone (see ReapStalePendingAssets).
func runDrDocAssetReaper(ctx context.Context, h *handlers.DrDocsHandler) {
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			h.ReapStalePendingAssets(ctx)
			timer.Reset(24 * time.Hour)
		}
	}
}

// runDrFeedbackAttachmentReaper runs the DR feedback unbound-attachment reaper
// shortly after boot and then daily, until ctx is cancelled. Mirrors the two
// reapers above; it removes only unbound message attachments (uploaded while
// composing but never bound to a sent message) older than 24h. Bound
// attachments are never reaped (see ReapUnboundAttachments).
func runDrFeedbackAttachmentReaper(ctx context.Context, h *handlers.DrFeedbackHandler) {
	timer := time.NewTimer(3 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			h.ReapUnboundAttachments(ctx)
			timer.Reset(24 * time.Hour)
		}
	}
}

// runDrChatLabAttachmentReaper runs the DR chat-lab reapers shortly after boot
// and then daily, until ctx is cancelled. Mirrors
// runDrFeedbackAttachmentReaper; it removes unbound chat attachments (uploaded
// while composing but never bound to a sent message) AND pending project
// assets (uploads never completed), both older than 24h.
func runDrChatLabAttachmentReaper(ctx context.Context, h *handlers.DrChatLabHandler) {
	timer := time.NewTimer(4 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			h.ReapUnboundAttachments(ctx)
			h.ReapStaleProjectAssets(ctx)
			timer.Reset(24 * time.Hour)
		}
	}
}

// runDrChatLabMemoryScheduler runs the nightly hash-gated project-memory job
// at DR_CHATLAB_MEMORY_JOB_HOUR o'clock in DR_CHATLAB_MEMORY_JOB_TZ (default
// 4 AM America/Denver — deliberately a WALL-CLOCK schedule, the one exception
// to UTC-thinking, so it stays 4 AM local across DST). One minute after boot
// (mirroring the reaper warm-up) it first checks dr_chatlab_job_state and
// catches up on a missed occurrence — a restart at 3:59 AM or a server down
// overnight must not silently skip a night. Each occurrence is computed fresh
// with time.Date in the location, which makes DST automatic: 4 AM Denver is
// 10:00 UTC in summer and 11:00 UTC in winter, and this loop never needs to
// know.
func runDrChatLabMemoryScheduler(ctx context.Context, h *handlers.DrChatLabHandler, cfg *config.Config) {
	loc, err := time.LoadLocation(cfg.DRChatLabMemoryJobTZ)
	if err != nil {
		// Fail-open on scheduling, never crash boot: the job still runs, just
		// pinned to UTC until the zone name is fixed.
		log.Printf("dr chatlab memory scheduler: invalid DR_CHATLAB_MEMORY_JOB_TZ %q (falling back to UTC): %v", cfg.DRChatLabMemoryJobTZ, err)
		loc = time.UTC
	}
	hour := cfg.DRChatLabMemoryJobHour

	// Boot catch-up (one minute of warm-up first).
	warmup := time.NewTimer(time.Minute)
	defer warmup.Stop()
	select {
	case <-ctx.Done():
		return
	case <-warmup.C:
		h.MaybeRunNightlyMemoryCatchUp(ctx, hour, loc)
	}

	// The hand-rolled timer loop (no cron dependency): sleep until the next
	// HH:00 in loc, run, recompute, repeat.
	for {
		next := handlers.NextMemoryJobRun(time.Now(), hour, loc)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			h.RunNightlyMemoryJob(ctx)
		}
	}
}

// runMigrationsAtBoot applies any pending migrations using the same go-migrate
// machinery as the standalone runner.
func runMigrationsAtBoot(cfg *config.Config, logging *slog.Logger) error {
	path := strings.TrimSpace(cfg.MigrationsPath)
	if path == "" {
		candidates := []string{
			"internal/migrations/migrations",
			"../internal/migrations/migrations",
			"../../internal/migrations/migrations",
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				abs, _ := filepath.Abs(c)
				path = abs
				break
			}
		}
	}
	if path == "" {
		return errors.New("migrations directory not found (set MIGRATIONS_PATH)")
	}
	source := "file://" + filepath.ToSlash(path)
	m, err := migrate.New(source, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	logging.Info("api auto-migrate up complete", "path", path)
	return nil
}

// silence unused-import linters for packages we use indirectly via reflection
// hooks above.
var (
	_ = pgxpool.New
	_ = redis.Nil
)
