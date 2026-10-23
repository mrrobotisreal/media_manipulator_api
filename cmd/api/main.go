package main

import (
	"context"
	"crypto/sha256"
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

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/cleanup"
	"github.com/mrrobotisreal/media_manipulator_api/internal/cmdaudit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/db"
	"github.com/mrrobotisreal/media_manipulator_api/internal/geo"
	"github.com/mrrobotisreal/media_manipulator_api/internal/gpu"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/limits"
	"github.com/mrrobotisreal/media_manipulator_api/internal/logger"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metadata"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metrics"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/redisx"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

func loadDotEnv() {
	_ = godotenv.Load(".env")
	_ = godotenv.Load(".env.local")
}

func main() {
	loadDotEnv()
	cfg := config.Load()
	handlers.SetResultOwnershipEnforce(cfg.ResultOwnershipEnforce)
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

	// Server-truth product analytics. A SECOND, non-authoritative consumer that
	// runs alongside the mm_* telemetry above and never replaces it: mm_* is the
	// operational, audit-grade record; this feed exists so the analytics service
	// has ground truth for job outcomes, downloads, quota rejections and signups
	// that a browser cannot reliably report. Disabled unless ANALYTICS_SERVER_KEY
	// is set, in which case every emission below is a no-op.
	analyticsEmitter := analyticsemit.New(cfg, logging, metricsReg)

	limiter := limits.New(redisClient, cfg, store, metricsReg)
	// An exported field rather than a New() parameter, so the limiter's existing
	// callers and tests are untouched.
	limiter.Analytics = analyticsEmitter
	gpuMgr := gpu.NewManager(cfg, store, metricsReg, logging)

	// Existing services
	jobManager := services.NewJobManager()
	// Give the in-memory job model a durable shadow. mm_conversion_jobs is
	// written on creation (from the handlers, which know the owner) and on
	// lifecycle transitions (from here); it is never read back in the job-status
	// path, so a database problem cannot slow or fail a conversion. A nil store
	// leaves the recorder unset and every record call a no-op.
	if store.Enabled() {
		jobManager.SetRecorder(store)
	}
	// And the product-analytics shadow. Independent of the recorder above: a box
	// with no telemetry database can still report product analytics, and vice
	// versa. Job lifecycle emission lives in the job manager because completion and
	// failure happen inside the pipeline, long after any request context is gone.
	jobManager.SetAnalyticsEmitter(analyticsEmitter)
	converter := services.NewConverter(cfg)
	inspector := services.NewMediaInspector(cfg.CommandTimeout)
	// MM07: log (never fatal) whether the metadata inspector's tools exist.
	etVer, ffVer := metadata.Probe(context.Background())
	slog.Info("metadata inspector tools", "exiftool", etVer, "ffprobe", ffVer)
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
	// Tier limits are resolved once from config and shared by the quota
	// middleware, the validators, the transcode ladder and the public
	// /api/tiers endpoint. One table, read in one place.
	tierRegistry := tiers.New(cfg)
	if n := tierRegistry.OperatorCount(); n > 0 {
		logging.Info("operator tier enabled", "allowlisted_accounts", n)
	}
	// The per-route hourly abuse buckets skip operators. The predicate reads
	// the request's tier resolution, which TierContext derived from a verified
	// Firebase token — so it is exactly as spoof-proof as the quota itself.
	limiter.RouteExempt = func(c *gin.Context) bool {
		res, ok := middleware.ResolveTier(c)
		return ok && res.Limits.RateLimitExempt
	}
	conversionHandler := handlers.NewConversionHandler(jobManager, converter, cfg, inspector, analysisQueue, transcription, s3Client, faceDetectionStore, tierRegistry)
	// `server_download_served` — the one server-truth event whose call site is a
	// plain request handler.
	conversionHandler.SetAnalyticsEmitter(analyticsEmitter)
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
	// DR Tasks (/dr/tasks): Jira-style kanban task tracker. Same always-
	// Firebase-gated /api/dr group + shared pool/config; no S3 dependency.
	drTasksHandler := handlers.NewDrTasksHandler(pool, cfg)
	// DR Portal desktop app installers: short-TTL presigned download URLs for
	// the private Mac (arm64/Intel) and Windows builds. Presign-only — no DB,
	// no uploads; nil presign client (S3 unconfigured) → 503.
	var drDesktopPresign *s3.PresignClient
	if s3Client != nil {
		drDesktopPresign = s3.NewPresignClient(s3Client)
	}
	drDesktopHandler := handlers.NewDrDesktopHandler(cfg, drDesktopPresign)

	// Future auth seam (default OFF): when RESTORE_REQUIRE_FIREBASE_AUTH is
	// set, /api/video-restore/* verifies Firebase ID tokens. Init failure
	// leaves the verifier nil and the middleware fails CLOSED on that group.
	// One Firebase ID-token verifier, shared by two consumers with different
	// postures:
	//   - the restore routes, gated behind RESTORE_REQUIRE_FIREBASE_AUTH, which
	//     fail CLOSED when the flag is on and init failed;
	//   - accounts and tier resolution, which are always on and fail OPEN — a
	//     request with no usable verifier is simply anonymous.
	// It is therefore initialized unconditionally rather than only when the
	// restore flag is set.
	var firebaseVerifier middleware.TokenVerifier
	if verifier, err := middleware.NewFirebaseVerifier(ctx, cfg.FirebaseProjectID); err != nil {
		logging.Error("firebase auth init failed; sign-in and tier resolution are unavailable (all callers anonymous)", "error", err.Error())
		if cfg.RestoreRequireFirebaseAuth {
			logging.Error("RESTORE_REQUIRE_FIREBASE_AUTH is on; /api/video-restore/* will reject all requests")
		}
	} else {
		firebaseVerifier = verifier
	}
	restoreAuthVerifier := firebaseVerifier

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

	// The presign client is what turns a conversion-history row into a working
	// download link; nil (no S3 configured) simply means history lists results
	// without links.
	var accountPresign *s3.PresignClient
	if s3Client != nil {
		accountPresign = s3.NewPresignClient(s3Client)
	}
	accountHandler := handlers.NewAccountHandler(store, tierRegistry, cfg, logging, accountPresign, jobManager)
	// `server_signup_completed` — the exact signup number, which only the account
	// sync can produce (see AccountHandler.emitSignupCompleted).
	accountHandler.SetAnalyticsEmitter(analyticsEmitter)
	// Billing (MM17) ships dark: every /api/billing route answers 503
	// billing_unavailable unless BILLING_ENABLED and all Stripe settings are set.
	accountHandler.SetBilling(newBillingHandler(cfg, store, tierRegistry, analyticsEmitter, logging))

	router := setupRouter(cfg, conversionHandler, studioHandler, videoRestoreHandler, imageRestoreHandler, documentScanHandler, accountHandler, tierRegistry, restoreAuthVerifier, firebaseVerifier, drVerifier, drDocsHandler, drCommentsHandler, drFeedbackHandler, drChatLabHandler, drTasksHandler, drDesktopHandler, store, enricher, limiter, metricsReg, analyticsEmitter)

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
	// Best-effort flush of queued analytics events, AFTER the HTTP server has
	// stopped accepting requests so nothing new is being queued. Bounded at 5s: a
	// wedged analytics service must not hold up the API's restart, and the events
	// it would take are the least important thing this process owns.
	if !analyticsEmitter.Close(5 * time.Second) {
		logging.Warn("analytics emitter did not flush within the shutdown deadline; queued events dropped")
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

func setupRouter(cfg *config.Config, conversionHandler *handlers.ConversionHandler, studioHandler *handlers.StudioHandler, videoRestoreHandler *handlers.VideoRestoreHandler, imageRestoreHandler *handlers.ImageRestoreHandler, documentScanHandler *handlers.DocumentScanHandler, accountHandler *handlers.AccountHandler, tierRegistry *tiers.Registry, restoreAuthVerifier middleware.TokenVerifier, firebaseVerifier middleware.TokenVerifier, drVerifier middleware.ClaimsVerifier, drDocsHandler *handlers.DrDocsHandler, drCommentsHandler *handlers.DrCommentsHandler, drFeedbackHandler *handlers.DrFeedbackHandler, drChatLabHandler *handlers.DrChatLabHandler, drTasksHandler *handlers.DrTasksHandler, drDesktopHandler *handlers.DrDesktopHandler, store *telemetry.Store, enricher *geo.Enricher, limiter *limits.Limiter, m *metrics.Registry, analyticsEmitter *analyticsemit.Emitter) *gin.Engine {
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
	corsConfig.ExposeHeaders = []string{
		"Content-Length", "Content-Disposition", "Content-Range", "Accept-Ranges", "X-MM-Request-ID",
		// Quota state, read by the UI's meter after every gated request. The
		// browser hides response headers that are not listed here.
		middleware.HeaderQuotaLimit, middleware.HeaderQuotaRemaining,
		middleware.HeaderQuotaReset, middleware.HeaderTier,
	}
	router.Use(cors.New(corsConfig))
	router.Use(middleware.RequestContext())
	router.Use(middleware.AccessLog(store, enricher))
	router.Use(m.Middleware())
	// Global per-IP rate limit guard.
	router.Use(limiter.GlobalIPRPS())

	// FIX B3: this used to be called at the END of setupRouter, after every
	// route had already been registered. Gin's group.combineHandlers snapshots
	// the middleware slice at registration time, so an engine-level Use() after
	// the fact only reaches the 404/405 chains — all sixteen per-route buckets
	// were dead code that never executed. It has to be installed before the
	// route groups below are created. The dispatcher does its own path
	// matching, so running globally is correct.
	router.Use(routeLimitDispatcher(cfg, limiter, newRouteGuard(limiter)))

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "healthy", "service": "media_manipulator_api"})
	})

	if cfg.MetricsEnabled {
		router.GET("/metrics", gin.WrapH(promhttp.HandlerFor(m.Reg, promhttp.HandlerOpts{})))
	}

	api := router.Group("/api")
	{
		// Identity, then tier, then quota — installed on the group BEFORE any
		// route is registered, so they are part of every handler chain the
		// Register* calls below snapshot. Installing them after registration is
		// what made B3, and it fails silently.
		//
		// OptionalFirebaseAuth is safe everywhere on /api: with no Bearer header
		// it returns immediately, and it never rejects. Quota is applied only to
		// the paths in quotaRoutes.
		api.Use(middleware.OptionalFirebaseAuth(true, firebaseVerifier))
		// TierContext resolves the caller's entitlement lazily and once. The
		// quota middleware, the upload byte caps and the transcode ladder all
		// read the same answer through middleware.ResolveTier.
		api.Use(middleware.TierContext(middleware.TierConfig{
			Store:    store,
			Registry: tierRegistry,
			Logger:   slog.Default(),
		}, cfg.TierQuotaEnabled))
		api.Use(quotaDispatcher(middleware.QuotaConfig{
			Store:    store,
			Registry: tierRegistry,
			Enabled:  cfg.TierQuotaEnabled,
			// A fixed salt, like internal/limits/limiter.go's. The goal is that
			// the ledger holds no reversible address, not cryptographic secrecy.
			IPHashSecret: quotaIPSalt[:],
			Logger:       slog.Default(),
			// Every 429 also becomes a `server_quota_rejected` product-analytics
			// event. Nil when analytics is unconfigured, which is a no-op.
			Analytics: analyticsEmitter,
			// MM18: an export whose project is taller than the plan's
			// MaxOutputHeight is refused (403 output_height_limited) before
			// the export allowance is spent.
		}, map[string]gin.HandlerFunc{
			models.MetricExport: handlers.ExportHeightGate(studioHandler.ExportHeightLookup),
		}))

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
		// DR Tasks — Jira-style kanban board (list/create/detail/patch/move/
		// archive/restore), on the same group. No reapers, no schedulers.
		handlers.RegisterDrTasksRoutes(drGroup, drTasksHandler)
		// DR Portal desktop app installer downloads, on the same group.
		handlers.RegisterDrDesktopRoutes(drGroup, drDesktopHandler)

		telemetryHandler := handlers.NewTelemetryHandler(store, enricher)
		telemetryHandler.Register(api)

		// Accounts and the public tier matrix. /sync and /me require a verified
		// token; /usage works anonymously so a signed-out visitor's quota meter
		// is real; /tiers is fully public and is what /pricing renders from.
		handlers.RegisterAccountRoutes(api, accountHandler, cfg, restoreAuthVerifier)
	}

	return router
}

// newRouteGuard builds the per-route limiter middleware factory the dispatcher
// calls. It is a package variable so cmd/api's test can substitute a spy and
// prove the dispatcher actually runs on a real route — the failure mode in B3
// was silent, and only a test keeps it fixed.
var newRouteGuard = func(limiter *limits.Limiter) func(routeKey, tool string, sessionLimit, ipLimit int) gin.HandlerFunc {
	return func(routeKey, tool string, sessionLimit, ipLimit int) gin.HandlerFunc {
		return limiter.Route(routeKey, tool, sessionLimit, ipLimit)
	}
}

// quotaIPSalt salts the hashed-IP quota subject. Fixed, like the rate
// limiter's — non-reversibility within a snapshot, not secrecy.
var quotaIPSalt = sha256.Sum256([]byte("mm-api-quota-ip-salt"))

// quotaRoute maps one gated endpoint to the metric it spends.
type quotaRoute struct {
	metric string
	// matches overrides the default exact-path comparison, for the
	// parameterized Content Studio routes.
	path    string
	matches func(path string) bool
}

// quotaRoutes is the whole gated surface. Anything not listed here is free of
// charge — status polls, downloads, capability probes and the account
// endpoints themselves must never spend a visitor's allowance.
var quotaRoutes = []quotaRoute{
	// `ops` — the everyday conversion work the daily allowance is about.
	{metric: models.MetricOps, path: "/api/upload"},
	{metric: models.MetricOps, path: "/api/video-upload/complete"},
	{metric: models.MetricOps, path: "/api/video-transcode/start"},
	{metric: models.MetricOps, path: "/api/tools/caption-translator"},
	{metric: models.MetricOps, path: "/api/tools/stitch-audio-to-video"},

	// `ai_ops` — GPU-bound work with a much tighter allowance.
	{metric: models.MetricAIOps, path: "/api/document-scan/start"},
	{metric: models.MetricAIOps, path: "/api/image-restore/start"},
	{metric: models.MetricAIOps, path: "/api/video-restore/start"},
	{
		metric: models.MetricAIOps,
		matches: func(path string) bool {
			return strings.HasPrefix(path, "/api/studio/projects/") && strings.HasSuffix(path, "/captions/generate")
		},
	},
	{
		metric: models.MetricAIOps,
		matches: func(path string) bool {
			return strings.HasPrefix(path, "/api/studio/assets/") && strings.HasSuffix(path, "/derive")
		},
	},

	// `export` — a Content Studio render, counted against the ops allowance.
	{
		metric: models.MetricExport,
		matches: func(path string) bool {
			return strings.HasPrefix(path, "/api/studio/projects/") && strings.HasSuffix(path, "/export")
		},
	},
}

// gateThenQuota runs a refusal BEFORE the allowance is spent, so a request that
// was never going to run does not cost a unit. A gate that aborts ends the
// chain; one that does not falls through to RequireQuota.
func gateThenQuota(gate, quota gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		gate(c)
		if c.IsAborted() {
			return
		}
		quota(c)
	}
}

// quotaDispatcher applies RequireQuota to the paths in quotaRoutes.
//
// It is installed on the /api group before any route is registered, so it is
// genuinely part of each route's handler chain. The per-metric handlers are
// built once at startup rather than per request.
func quotaDispatcher(cfg middleware.QuotaConfig, preQuotaGates ...map[string]gin.HandlerFunc) gin.HandlerFunc {
	gates := map[string]gin.HandlerFunc{}
	for _, g := range preQuotaGates {
		for k, v := range g {
			gates[k] = v
		}
	}
	byMetric := map[string]gin.HandlerFunc{}
	for _, r := range quotaRoutes {
		if _, ok := byMetric[r.metric]; !ok {
			quota := middleware.RequireQuota(cfg, r.metric)
			if gate := gates[r.metric]; gate != nil {
				quota = gateThenQuota(gate, quota)
			}
			byMetric[r.metric] = quota
		}
	}

	return func(c *gin.Context) {
		// Only mutating requests spend quota. A GET is a status poll or a
		// download of work already paid for.
		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}
		path := c.Request.URL.Path
		for _, r := range quotaRoutes {
			hit := r.matches != nil && r.matches(path)
			if !hit {
				hit = r.path != "" && r.path == path
			}
			if hit {
				byMetric[r.metric](c)
				return
			}
		}
		c.Next()
	}
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
		// Version-history checkpoints (part 09) are cheap DB writes, but the
		// client fires them on a timer — share the generous upload bucket
		// rather than minting a new limiter type. Only POSTs are dispatched,
		// so the versions LIST is never limited.
		{
			routeKey:     "studio_versions_create",
			tool:         "studio_versions",
			sessionLimit: cfg.RateLimitUploadsPerSessionPerHour,
			ipLimit:      cfg.RateLimitUploadsPerIPPerHour,
			matches: func(path string) bool {
				return strings.HasPrefix(path, "/api/studio/projects/") && strings.HasSuffix(path, "/versions")
			},
		},
		{
			routeKey:     "studio_versions_restore",
			tool:         "studio_versions",
			sessionLimit: cfg.RateLimitUploadsPerSessionPerHour,
			ipLimit:      cfg.RateLimitUploadsPerIPPerHour,
			matches: func(path string) bool {
				return strings.HasPrefix(path, "/api/studio/projects/") &&
					strings.Contains(path, "/versions/") && strings.HasSuffix(path, "/restore")
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
