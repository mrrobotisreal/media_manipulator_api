package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const DefaultPort = "59997"

type Config struct {
	Port               string
	UploadDir          string
	OutputDir          string
	TempDir            string
	MaxFileSize        int64
	MaxVideoUpload     int64
	CommandTimeout     time.Duration
	AnalysisWorkers    int
	AWSRegion          string
	S3Bucket           string
	S3Endpoint         string
	S3PresignTTL       time.Duration
	S3ResultPresignTTL time.Duration
	S3ResultPrefix     string
	// AWSCLIBin is the AWS CLI executable used for multipart uploads of
	// artifacts that exceed S3's 5GB single-PUT limit (notably the AI Video
	// Restoration results tarball). `aws s3 cp` does a multipart upload
	// transparently; PutObject cannot. Default "aws" (resolved on PATH).
	// Credentials, region and endpoint are inherited from the same process
	// environment the SDK client already reads.
	AWSCLIBin string

	// Local AI toolchain (Phase 1: audio + image AI tools)
	AIEnabled     bool
	AIRootDir     string
	AICUDAGPU     int
	AIVulkanGPU   int
	DeepFilterBin string
	DemucsBin     string
	VisionPython  string
	// FacePrivacyScript is the path to the runtime face privacy script on the
	// GPU host (configured via AI_FACE_PRIVACY_SCRIPT, default
	// /opt/media-manipulator-ai/scripts/face_privacy.py). A reference copy of
	// the script lives at scripts/server/face_privacy.py — that copy is not
	// loaded by the API but is checked in so updates are reviewable. Deploy
	// new versions with:
	//   sudo cp scripts/server/face_privacy.py /opt/media-manipulator-ai/scripts/face_privacy.py
	FacePrivacyScript string
	TextRedactScript  string
	RembgBin          string
	RembgEnvScript    string
	RembgModelDir     string
	RealESRGANBin     string
	VulkanEnvScript   string
	// LamaPython is the python interpreter for the simple_lama_inpainting
	// environment (separate venv to avoid clashing with rembg/realesrgan deps).
	// RemoveObjectScript is the runtime path to scripts/server/remove_object_lama.py;
	// override AI_REMOVE_OBJECT_SCRIPT in deployment. The mask itself is generated
	// on the Go side and passed via --mask.
	LamaPython         string
	RemoveObjectScript string

	// AIFrameInterpolationScript is the runtime path on the GPU host to the
	// helper script that drives rife-ncnn-vulkan plus ffmpeg I/O. The Go API
	// shells out to this path; a reference copy lives in
	// scripts/server/frame_interpolate_rife.py. Override via
	// AI_FRAME_INTERPOLATION_SCRIPT.
	AIFrameInterpolationEnabled            bool
	AIFrameInterpolationScript             string
	AIRIFEBin                              string
	AIRIFEModel                            string
	AIRIFEGPU                              int
	AIRIFEThreads                          string
	AIFrameInterpolationMaxDurationSeconds int
	AIFrameInterpolationMaxHeight          int
	AIFrameInterpolationTempRoot           string

	// --- Operational telemetry / observability ----------------------------

	DatabaseURL            string
	AdminDatabaseURL       string
	APIAutoMigrate         bool
	MigrationsPath         string
	TelemetryDBEnabled     bool
	TelemetryAuditCommands bool
	TelemetryWriteTimeout  time.Duration

	// --- Product-analytics emission (media_manipulator_analytics) ----------
	//
	// Separate from the telemetry settings above, and deliberately so: the mm_*
	// tables are operational truth and always on when a database is present,
	// while this is a lossy feed to a second service that must be able to be
	// off. Both keys unset (the default) means the API behaves exactly as it did
	// before this existed.

	// AnalyticsIngestURL is the analytics service's base URL. Localhost by
	// default because the two services run on the same box — going out through
	// nginx and Cloudflare to reach a process on 127.0.0.1 would add latency,
	// a TLS handshake, and two more things that can break.
	AnalyticsIngestURL string
	// AnalyticsServerKey is a SECRET (unlike the browser write key, which is
	// public by necessity). It authorizes `source='server'` events, which the
	// analytics service trusts more than browser events — it skips bot detection
	// and geo enrichment for them. Empty disables emission entirely.
	AnalyticsServerKey string
	// AnalyticsEmitBuffer bounds the in-memory queue. When it fills, events are
	// dropped and counted rather than blocking a request.
	AnalyticsEmitBuffer int
	// AnalyticsEmitFlush is the batching interval.
	AnalyticsEmitFlush time.Duration
	// AnalyticsEmitTimeout bounds one POST.
	AnalyticsEmitTimeout time.Duration

	RedisURL      string
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	RedisEnabled  bool

	MaxMindCityPath   string
	MaxMindASNPath    string
	GeoCacheTTL       time.Duration
	GeoCacheKeyPrefix string

	// Cleanup worker
	CleanupEnabled             bool
	CleanupInterval            time.Duration
	UploadRetention            time.Duration
	OutputRetention            time.Duration
	TempRetention              time.Duration
	CleanupDryRun              bool
	CleanupAuditMaxPathsPerRun int

	// Ledger-driven retention sweeper (MM23). The sweeper deletes only what
	// mm_retained_objects says has expired.
	//
	// RetentionSweepDryRun DEFAULTS TO TRUE: the sweeper (and the orphan walker)
	// log, meter and audit what they WOULD delete and delete nothing. Flip
	// RETENTION_SWEEP_DRY_RUN=false deliberately, after reading a day of dry-run
	// output in mm_cleanup_runs (RUNBOOK 4.2.1).
	RetentionSweepDryRun bool
	RetentionSweepBatch  int           // rows per batch (default 200)
	RetentionSweepBudget time.Duration // per-tick time budget (default 5m)
	RetentionMaxAttempts int           // give-up threshold per row (default 10)
	// RetentionJobMapMaxAge is how long a finished job stays in the in-memory job
	// map (default 24h). History no longer depends on it.
	RetentionJobMapMaxAge time.Duration

	// Observability
	MetricsEnabled     bool
	PProfEnabled       bool
	AdminDebugBindAddr string
	LogLevel           string
	LogFormat          string

	// Rate limiting
	RateLimitEnabled                     bool
	RateLimitPerIPRPS                    float64
	RateLimitPerIPBurst                  int
	RateLimitUploadsPerSessionPerHour    int
	RateLimitUploadsPerIPPerHour         int
	RateLimitTranscodesPerSessionPerHour int
	RateLimitTranscodesPerIPPerHour      int
	RateLimitAnalysisPerSessionPerHour   int
	RateLimitAnalysisPerIPPerHour        int
	RateLimitAuditAllowed                bool

	// Tier limits
	//
	// The defaults here ARE the published tier matrix (/pricing renders them
	// from GET /api/tiers, which serves internal/tiers). Overriding one of
	// these env vars changes what the product promises, so change the default
	// rather than setting an env var unless you are deliberately running a
	// deployment with different economics.
	//
	// Anonymous is deliberately generous enough to be genuinely useful — the
	// SEO landing pages have to convert a first-time visitor with zero
	// friction, and "convert one file" is the job that brings them.
	TierQuotaEnabled bool

	// ResultOwnershipEnforce (RESULT_OWNERSHIP_ENFORCE, default true) makes
	// result/download routes 404 for callers who do not own the job. False logs
	// would-be denials and allows them (rollout mode).
	ResultOwnershipEnforce bool

	TierAnonOpsPerDay int
	// *ExportsPerDay is the Content Studio export allowance, a separate counter
	// from ops. Each defaults to the matching *OpsPerDay value.
	TierAnonExportsPerDay   int
	TierAnonAIOpsPerDay     int
	TierAnonMaxFileBytes    int64
	TierAnonMaxVideoSeconds int
	TierAnonMaxOutputHeight int
	TierAnonMaxLadderHeight int
	TierAnonStudioProjects  int
	TierAnonRetentionHours  int
	// Version-history auto-checkpoint retention per project (part 09).
	// Uniform 20 across tiers by design; per-tier env levers exist so a future
	// deployment can monetize depth without a code change.
	TierAnonStudioVersionAutoKeep int

	TierFreeOpsPerDay             int
	TierFreeExportsPerDay         int
	TierFreeAIOpsPerDay           int
	TierFreeMaxFileBytes          int64
	TierFreeMaxVideoSeconds       int
	TierFreeMaxOutputHeight       int
	TierFreeMaxLadderHeight       int
	TierFreeStudioProjects        int
	TierFreeRetentionHours        int
	TierFreeStudioVersionAutoKeep int

	TierPremiumOpsPerDay       int
	TierPremiumExportsPerDay   int
	TierPremiumAIOpsPerDay     int
	TierPremiumMaxFileBytes    int64
	TierPremiumMaxVideoSeconds int
	TierPremiumMaxOutputHeight int
	TierPremiumMaxLadderHeight int
	// -1 = unlimited. getEnvIntDefault, not getEnvInt, because getEnvInt
	// rejects non-positive values.
	TierPremiumStudioProjects        int
	TierPremiumRetentionHours        int
	TierPremiumStudioVersionAutoKeep int

	// OperatorEmails is the allowlist behind the `operator` tier: the site's
	// own people, who get no daily allowance, no saved-project cap and no
	// per-route hourly bucket so they can test every tool without hitting a
	// wall. Comma-separated, lowercased at load.
	//
	// Spoof-resistance comes from WHERE the email is read, not from secrecy of
	// the list: tiers.Registry.IsOperator only ever sees the email claim of a
	// Firebase ID token the Admin SDK verified, and only honours it when the
	// token also says email_verified=true. A request header, a query string,
	// a client-side flag or an mm_users row can never produce the tier.
	// Empty (the default) disables the tier entirely.
	OperatorEmails []string

	// Billing (MM17). Ships dark: BillingEnabled defaults to false, and
	// BillingConfigured() additionally demands every secret/id below, so a
	// half-filled env can never expose a broken checkout. StripeSecretKey and
	// StripeWebhookSecret are SECRETS: never log them.
	BillingEnabled            bool
	StripeSecretKey           string
	StripeWebhookSecret       string
	StripePricePremiumMonthly string
	StripePricePremiumYearly  string
	// StripeAPIVersion optionally overrides the API version the SDK sends
	// (default: the stripe-go constant). Empty = use the SDK's.
	StripeAPIVersion string
	// StripeAutomaticTax adds Stripe Tax on top of the price at Checkout
	// (STRIPE_AUTOMATIC_TAX, default true). Turn it off if Stripe rejects sessions
	// because Stripe Tax is not activated in the Dashboard yet.
	StripeAutomaticTax bool
	// BillingGraceDays is how long Premium survives a failed payment.
	BillingGraceDays int
	// BillingPublicBaseURL prefixes Checkout success/cancel and Portal return URLs.
	BillingPublicBaseURL string
	// Display-only prices for GET /api/tiers; the charged amount is the Stripe Price.
	BillingPriceMonthlyUSD int
	BillingPriceYearlyUSD  int

	// FFmpeg validation
	MaxVideoDurationSeconds int
	MaxVideoWidth           int
	MaxVideoHeight          int
	MaxVideoPixels          int64
	MaxVideoFPS             int
	MaxAudioDurationSeconds int

	// GPU scheduler
	GPUSchedulerEnabled                        bool
	GPUSchedulerDevices                        []string
	GPUSchedulerDefaultWhisperDevice           string
	GPUSchedulerDefaultRealESRGANDevice        string
	GPUSchedulerDefaultVLMDevice               string
	GPUSchedulerWhisperConcurrencyPerDevice    int
	GPUSchedulerRealESRGANConcurrencyPerDevice int
	GPUSchedulerVLMConcurrencyPerDevice        int

	// Safety / compliance
	SafetyIncidentRetentionDays int

	// AI Video Restoration (multi-model comparison pipeline). A short clip is
	// trimmed from the upload, fanned out across up to six restoration /
	// super-resolution models, and every result is packaged into one tarball.
	// Heavy on GPU and disk (10–30GB per job) — keep RESTORE_MAX_CONCURRENT_JOBS
	// at 1 unless the host has headroom. Python scripts follow the same
	// repo-source-of-truth / deployed-copy convention as frame interpolation
	// (see AIFrameInterpolationScript above).
	RestoreEnabled                bool
	RestoreBasicVSRPPEnabled      bool
	RestoreMaxClipSeconds         float64
	RestoreRecommendedClipSeconds float64
	RestoreMaxFrames              int
	RestoreMaxSourceWidth         int
	RestoreMaxSourceHeight        int
	RestoreMaxConcurrentJobs      int
	RestoreModelTimeout           time.Duration
	// RestoreUploadTimeout bounds the multipart upload of the (multi-GB)
	// results tarball on its own. It is detached from the whole-job deadline so
	// a slow client link isn't starved by time already spent in the GPU stages.
	RestoreUploadTimeout              time.Duration
	RestoreResultPresignTTL           time.Duration
	RestoreRateLimitPerSessionPerHour int
	RestoreRateLimitPerIPPerHour      int
	// AIRestoreCUDAGPU is the CUDA index of the big-VRAM card (RTX 5080 16GB);
	// AIRestoreVulkanGPU is the Vulkan index realesrgan-ncnn-vulkan should
	// prefer. getEnvIntDefault so device 0 is a valid value.
	AIRestoreCUDAGPU      int
	AIRestoreVulkanGPU    int
	AIRestorePython       string
	AIRestoreMMPython     string
	AIRestoreFramesScript string
	AIRestoreVideoScript  string
	AIRestoreModelsDir    string
	AIRestoreReposDir     string
	// Per-model tuning knobs, keyed by restore model id ("realesrgan",
	// "swinir", "hat", "basicvsrpp", "rvrt", "vrt"). Estimated seconds per
	// frame feeds the UI time estimates; VRAM MiB drives GPU scheduler
	// requests. Both are env-overridable so the operator can tune after
	// real runs.
	RestoreEstSecondsPerFrame map[string]float64
	RestoreVRAMMiB            map[string]int64
	// Future auth seam for the restricted deployment at
	// dr.media-manipulator.com: when RESTORE_REQUIRE_FIREBASE_AUTH is true,
	// /api/video-restore/* requires a Firebase ID token (Authorization:
	// Bearer). Default OFF — current public behavior is unchanged. The Admin
	// SDK reads GOOGLE_APPLICATION_CREDENTIALS itself; the field exists so
	// deployments can see/validate what's configured. Firebase only — this
	// project never uses Supabase.
	RestoreRequireFirebaseAuth   bool
	FirebaseProjectID            string
	GoogleApplicationCredentials string

	// Double Raven partner portal (path-based /dr routes served on the main
	// site; see app/dr/* in media-manipulator-ui). DRAllowedEmails is the authz
	// backstop for the always-on /api/dr/* endpoints: a comma-separated,
	// case-insensitive allowlist of the Firebase accounts permitted into the
	// portal. Accounts are created manually in the Firebase console (there is no
	// sign-up), so the list is tiny — exactly the two owners. Empty => any
	// verified Firebase user of the project passes, but main.go logs a startup
	// warning recommending it be set. The DR claims verifier reuses the existing
	// FirebaseProjectID / GOOGLE_APPLICATION_CREDENTIALS above and is always
	// initialized, independent of RestoreRequireFirebaseAuth.
	DRAllowedEmails []string

	// AI Image Restoration & Upscaling — the still-image sibling of the video
	// restoration feature. General models (realesrgan/swinir/hat) reuse the
	// AIRestore* paths above (same binaries/venvs/scripts, zero new installs);
	// only the pre-clean and face venvs/scripts are new. CodeFormer is gated
	// behind ImageRestoreCodeFormerEnabled (S-Lab non-commercial license).
	ImageRestoreEnabled                    bool
	ImageRestoreCodeFormerEnabled          bool
	ImageRestoreMaxSourceWidth             int
	ImageRestoreMaxSourceHeight            int
	ImageRestoreMaxOutputPixels            int64
	ImageRestoreMaxOutputs                 int
	ImageRestoreMaxConcurrentJobs          int
	ImageRestoreModelTimeout               time.Duration
	ImageRestoreRateLimitPerSessionPerHour int
	ImageRestoreRateLimitPerIPPerHour      int
	AIFaceRestorePython                    string
	AIFaceRestoreScript                    string
	AIPrecleanPython                       string
	AIPrecleanScript                       string
	// Per-model tuning knobs, keyed by image-restore model id. Estimated
	// seconds per megapixel feeds the UI time estimates + stage weights; VRAM
	// MiB drives GPU scheduler requests. Both are env-overridable.
	ImageRestoreEstSecondsPerMegapixel map[string]float64
	ImageRestoreVRAMMiB                map[string]int64

	// Content Studio (browser-based multi-track NLE). All Content Studio
	// ffmpeg work (proxy, filmstrip, export) is pinned to a dedicated GPU so
	// it doesn't contend with whisper/RIFE on the shared host. We default to
	// GPU 1 (the 16GB RTX 5080) to mirror AI_CUDA_GPU=1.
	ContentStudioGPUIndex    int
	ContentStudioProxyHeight int
	ContentStudioS3Prefix    string
	// ContentStudioFontFile is the TTF used by ffmpeg drawtext for text/location
	// overlays. drawtext needs a real font file on the GPU host.
	ContentStudioFontFile string
	// ContentStudioFontsDir is the directory of curated title-clip fonts
	// (part 16), committed at assets/fonts/studio and mirrored by the UI's
	// public/fonts/studio set — see docs/content-studio/design/04-title-fonts.md.
	// Family names resolve to files ONLY through the fixed map in
	// services/studio_fonts.go; user text never forms a path.
	ContentStudioFontsDir string

	// AI Document Scan — scanned printed documents AND handwritten field notes
	// → searchable PDF + optional structured/transcribed DOCX. Printed pages use
	// OCRmyPDF+Tesseract (CPU) for the faithful searchable layer and PaddleOCR-VL
	// (preferred) / Docling (fallback) for DOCX structure; handwriting uses
	// qwen3-vl via Ollama (primary read) with an optional PaddleOCR-VL / TrOCR
	// second opinion. Dual-GPU: Ollama models are pinned to the 5080
	// (AIDocumentOCRGPU); the served/torch document engines run on the 5060 Ti
	// (AIDocumentOCRSecondaryGPU). This tool is PUBLIC (no Firebase).
	DocumentScanEnabled              bool
	DocumentScanDocxEnabled          bool
	DocumentScanHandwritingEnabled   bool
	DocumentScanSecondOpinionEnabled bool
	DocumentScanSummaryEnabled       bool
	AIDocumentOCRPython              string
	AIDocumentOCRScript              string
	// AIDocumentOCRGPU is the CUDA index of the primary big-VRAM card (RTX 5080)
	// hosting the Ollama-served VLM/text models; AIDocumentOCRSecondaryGPU is the
	// 5060 Ti hosting PaddleOCR-VL/TrOCR/Docling. getEnvIntDefault so device 0 is
	// a valid value.
	AIDocumentOCRGPU          int
	AIDocumentOCRSecondaryGPU int
	PandocBin                 string
	// DocumentScanOllamaURL is the Ollama endpoint THIS feature talks to. It is
	// deliberately separate from the global OLLAMA_URL (which other tools such as
	// the caption translator share) so the operator can point document-scan at a
	// dedicated second Ollama instance pinned to the 5080 — WITHOUT repinning the
	// shared daemon. Defaults to OLLAMA_URL when DOCUMENT_SCAN_OLLAMA_URL is unset.
	DocumentScanOllamaURL                  string
	OllamaVLMModel                         string
	OllamaTextModel                        string
	PaddleOCRVLEndpoint                    string
	PaddleOCRVLModel                       string
	DocumentScanStructureEngine            string
	DocumentScanSecondOpinionEngine        string
	TrOCRModel                             string
	DocumentScanLanguages                  []string
	DocumentScanMaxImages                  int
	DocumentScanMaxPages                   int
	DocumentScanMaxImageBytes              int64
	DocumentScanModelTimeout               time.Duration
	DocumentScanMaxConcurrentJobs          int
	DocumentScanRateLimitPerSessionPerHour int
	DocumentScanRateLimitPerIPPerHour      int

	// DR AI Chat Test Lab (/dr/demos/chat-lab) — a ChatGPT-style chat backed by
	// OpenRouter, called from THIS API (never from the Next.js layer) so the key
	// stays on the home server and every request passes RequireDoubleRavenAuth.
	// An empty OpenRouterAPIKey fails closed: the chat-lab send/models endpoints
	// return 503 (same philosophy as the nil DR verifier). DRChatLabModelRules is
	// a CSV of allow rules against the live OpenRouter catalog — a rule ending in
	// '/' is a provider prefix match (e.g. "anthropic/"), anything else is an
	// exact model-id match (e.g. "z-ai/glm-5.2"). Lowercased at load
	// (splitCSVLower) so matching is a plain case-insensitive compare.
	// DRChatLabTitleModel optionally names a cheap model for auto-titling new
	// chats; empty disables LLM titling (fallback: first-message truncation).
	OpenRouterAPIKey         string
	OpenRouterBaseURL        string
	DRChatLabModelRules      []string
	DRChatLabTitleModel      string
	DRChatLabMaxOutputTokens int
	DRChatLabAttributionURL  string
	// Chat-lab Projects: DRChatLabMemoryModel names a cheap model that
	// regenerates each project's living memory; empty disables memory
	// (projects work, memory shows 'disabled'). Regeneration is nightly and
	// hash-gated (see dr_chatlab_memory_hashes.go): DRChatLabMemoryJobHour +
	// DRChatLabMemoryJobTZ schedule the sweep in a WALL-CLOCK timezone
	// (default 4 AM America/Denver) — the ONE deliberate exception to
	// UTC-thinking, so the hour survives DST; resolved with time.LoadLocation,
	// falling back to UTC (fail-open) when the zone name is invalid.
	// DRChatLabMemoryMaxChars caps the generated memory (the structured
	// six-section briefing needs room — default 8192).
	// DRChatLabToolMaxRounds bounds the read_asset tool-execution loop per
	// send; DRChatLabAssetReadCapBytes caps one text/code asset read returned
	// to the model.
	DRChatLabMemoryModel       string
	DRChatLabMemoryMaxChars    int
	DRChatLabMemoryJobHour     int
	DRChatLabMemoryJobTZ       string
	DRChatLabToolMaxRounds     int
	DRChatLabAssetReadCapBytes int

	// Double Raven Portal desktop app installers (private Electron builds in
	// the shared S3 bucket, served only as short-TTL presigned GETs behind
	// /api/dr). One key per platform so a version bump is an env change plus an
	// upload — no deploy. The default keys contain LITERAL spaces (they mirror
	// the electron-builder artifact names); the SDK presigner handles encoding.
	DRDesktopMacArm64Key string
	DRDesktopMacIntelKey string
	DRDesktopWindowsKey  string
}

func Load() *Config {
	maxFileSize := getEnvInt64("MAX_FILE_SIZE_BYTES", 10000*1024*1024)
	anonOps := getEnvInt("TIER_ANON_OPS_PER_DAY", 5)
	freeOps := getEnvInt("TIER_FREE_OPS_PER_DAY", 30)
	premiumOps := getEnvInt("TIER_PREMIUM_OPS_PER_DAY", 300)
	return &Config{
		Port:               DefaultPort,
		UploadDir:          getEnv("UPLOAD_DIR", "uploads"),
		OutputDir:          getEnv("OUTPUT_DIR", "outputs"),
		TempDir:            getEnv("TEMP_DIR", "temp"),
		MaxFileSize:        maxFileSize,
		MaxVideoUpload:     getEnvInt64("MAX_VIDEO_UPLOAD_SIZE_BYTES", maxFileSize),
		CommandTimeout:     time.Duration(getEnvInt("COMMAND_TIMEOUT_SECONDS", 6*60*60)) * time.Second,
		AnalysisWorkers:    getEnvInt("ANALYSIS_WORKERS", 1),
		AWSRegion:          getEnv("AWS_REGION", "us-west-2"),
		S3Bucket:           getEnv("S3_BUCKET", "media-manipulator"),
		S3Endpoint:         getEnv("AWS_S3_ENDPOINT", ""),
		S3PresignTTL:       time.Duration(getEnvInt("S3_PRESIGN_TTL_SECONDS", 15*60)) * time.Second,
		S3ResultPresignTTL: time.Duration(getEnvInt("S3_RESULT_PRESIGN_TTL_SECONDS", 30*60)) * time.Second,
		S3ResultPrefix:     getEnv("S3_RESULT_PREFIX", "results"),
		AWSCLIBin:          getEnv("AWS_CLI_BIN", "aws"),

		AIEnabled:         getEnvBool("AI_ENABLED", true),
		AIRootDir:         getEnv("AI_ROOT_DIR", "/opt/media-manipulator-ai"),
		AICUDAGPU:         getEnvIntDefault("AI_CUDA_GPU", 1),
		AIVulkanGPU:       getEnvIntDefault("AI_VULKAN_GPU", 1),
		DeepFilterBin:     getEnv("AI_DEEPFILTER_BIN", "/opt/media-manipulator-ai/venvs/audio-clean/bin/deepFilter"),
		DemucsBin:         getEnv("AI_DEMUCS_BIN", "/opt/media-manipulator-ai/venvs/audio-separate/bin/demucs"),
		VisionPython:      getEnv("AI_VISION_PYTHON", "/opt/media-manipulator-ai/venvs/vision-privacy/bin/python"),
		FacePrivacyScript: getEnv("AI_FACE_PRIVACY_SCRIPT", "/opt/media-manipulator-ai/scripts/face_privacy.py"),
		TextRedactScript:  getEnv("AI_TEXT_REDACT_SCRIPT", "/opt/media-manipulator-ai/scripts/redact_text_pii.py"),
		RembgBin:          getEnv("AI_REMBG_BIN", "/opt/media-manipulator-ai/venvs/bg-remove/bin/rembg"),
		RembgEnvScript:    getEnv("AI_REMBG_ENV_SCRIPT", "/opt/media-manipulator-ai/env/onnxruntime-cuda-bg-remove.sh"),
		RembgModelDir:     getEnv("AI_REMBG_MODEL_DIR", "/opt/media-manipulator-ai/models/rembg"),
		RealESRGANBin:     getEnv("AI_REALESRGAN_BIN", "/opt/media-manipulator-ai/bin/realesrgan-ncnn-vulkan/realesrgan-ncnn-vulkan"),
		VulkanEnvScript:   getEnv("AI_VULKAN_ENV_SCRIPT", "/opt/media-manipulator-ai/env/vulkan-nvidia.sh"),

		LamaPython:         getEnv("AI_LAMA_PYTHON", "/opt/media-manipulator-ai/venvs/inpaint/bin/python"),
		RemoveObjectScript: getEnv("AI_REMOVE_OBJECT_SCRIPT", "/opt/media-manipulator-ai/scripts/remove_object_lama.py"),

		AIFrameInterpolationEnabled:            getEnvBool("AI_FRAME_INTERPOLATION_ENABLED", true),
		AIFrameInterpolationScript:             getEnv("AI_FRAME_INTERPOLATION_SCRIPT", "/opt/media-manipulator-ai/scripts/frame_interpolate_rife.py"),
		AIRIFEBin:                              getEnv("AI_RIFE_BIN", "/opt/media-manipulator-ai/bin/rife-ncnn-vulkan/rife-ncnn-vulkan"),
		AIRIFEModel:                            getEnv("AI_RIFE_MODEL", "/opt/media-manipulator-ai/bin/rife-ncnn-vulkan/models/rife-v4.6"),
		AIRIFEGPU:                              getEnvIntDefault("AI_RIFE_GPU", 1),
		AIRIFEThreads:                          getEnv("AI_RIFE_THREADS", "1:2:2"),
		AIFrameInterpolationMaxDurationSeconds: getEnvInt("AI_FRAME_INTERPOLATION_MAX_DURATION_SECONDS", 120),
		AIFrameInterpolationMaxHeight:          getEnvInt("AI_FRAME_INTERPOLATION_MAX_HEIGHT", 720),
		AIFrameInterpolationTempRoot:           getEnv("AI_FRAME_INTERPOLATION_TEMP_ROOT", "/opt/media-manipulator-ai/tmp"),

		// Operational DB
		DatabaseURL:            getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/media_manipulator?sslmode=disable"),
		AdminDatabaseURL:       getEnv("POSTGRES_ADMIN_DATABASE_URL", ""),
		APIAutoMigrate:         getEnvBool("API_AUTO_MIGRATE", false),
		MigrationsPath:         getEnv("MIGRATIONS_PATH", ""),
		TelemetryDBEnabled:     getEnvBool("TELEMETRY_DB_ENABLED", true),
		TelemetryAuditCommands: getEnvBool("TELEMETRY_AUDIT_COMMANDS", true),
		TelemetryWriteTimeout:  time.Duration(getEnvInt("TELEMETRY_WRITE_TIMEOUT_SECONDS", 5)) * time.Second,

		// Product analytics (media_manipulator_analytics, port 59998). Disabled
		// until ANALYTICS_SERVER_KEY is set to a value in that service's
		// SERVER_KEYS.
		AnalyticsIngestURL:   getEnv("ANALYTICS_INGEST_URL", "http://127.0.0.1:59998"),
		AnalyticsServerKey:   getEnv("ANALYTICS_SERVER_KEY", ""),
		AnalyticsEmitBuffer:  getEnvInt("ANALYTICS_EMIT_BUFFER", 512),
		AnalyticsEmitFlush:   time.Duration(getEnvInt("ANALYTICS_EMIT_FLUSH_MS", 2000)) * time.Millisecond,
		AnalyticsEmitTimeout: time.Duration(getEnvInt("ANALYTICS_EMIT_TIMEOUT_SECONDS", 5)) * time.Second,

		// Redis
		RedisURL:      getEnv("REDIS_URL", ""),
		RedisAddr:     getEnv("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword: getEnv("REDIS_PASSWORD", ""),
		RedisDB:       getEnvIntDefault("REDIS_DB", 0),
		RedisEnabled:  getEnvBool("REDIS_ENABLED", true),

		// Geo
		MaxMindCityPath:   getEnv("MAXMIND_CITY_MMDB_PATH", ""),
		MaxMindASNPath:    getEnv("MAXMIND_ASN_MMDB_PATH", ""),
		GeoCacheTTL:       time.Duration(getEnvInt("GEO_CACHE_TTL_SECONDS", 86400)) * time.Second,
		GeoCacheKeyPrefix: getEnv("GEO_CACHE_KEY_PREFIX", "media-manipulator:api:geoip:v1:"),

		// Cleanup worker
		CleanupEnabled:             getEnvBool("CLEANUP_ENABLED", true),
		CleanupInterval:            time.Duration(getEnvInt("CLEANUP_INTERVAL_SECONDS", 900)) * time.Second,
		UploadRetention:            time.Duration(getEnvInt("UPLOAD_RETENTION_SECONDS", 86400)) * time.Second,
		OutputRetention:            time.Duration(getEnvInt("OUTPUT_RETENTION_SECONDS", 86400)) * time.Second,
		TempRetention:              time.Duration(getEnvInt("TEMP_RETENTION_SECONDS", 3600)) * time.Second,
		CleanupDryRun:              getEnvBool("CLEANUP_DRY_RUN", false),
		CleanupAuditMaxPathsPerRun: getEnvInt("CLEANUP_AUDIT_MAX_PATHS_PER_RUN", 1000),
		RetentionSweepDryRun:       getEnvBool("RETENTION_SWEEP_DRY_RUN", true),
		RetentionSweepBatch:        getEnvInt("RETENTION_SWEEP_BATCH", 200),
		RetentionSweepBudget:       time.Duration(getEnvInt("RETENTION_SWEEP_BUDGET_SECONDS", 300)) * time.Second,
		RetentionMaxAttempts:       getEnvInt("RETENTION_MAX_ATTEMPTS", 10),
		RetentionJobMapMaxAge:      time.Duration(getEnvInt("RETENTION_JOB_MAP_MAX_AGE_HOURS", 24)) * time.Hour,

		// Observability
		MetricsEnabled:     getEnvBool("METRICS_ENABLED", true),
		PProfEnabled:       getEnvBool("PPROF_ENABLED", false),
		AdminDebugBindAddr: getEnv("ADMIN_DEBUG_BIND_ADDR", ""),
		LogLevel:           getEnv("LOG_LEVEL", "info"),
		LogFormat:          getEnv("LOG_FORMAT", "json"),

		// Rate limiting
		RateLimitEnabled:                     getEnvBool("RATE_LIMIT_ENABLED", true),
		RateLimitPerIPRPS:                    getEnvFloat("RATE_LIMIT_PER_IP_RPS", 2),
		RateLimitPerIPBurst:                  getEnvInt("RATE_LIMIT_PER_IP_BURST", 20),
		RateLimitUploadsPerSessionPerHour:    getEnvInt("RATE_LIMIT_UPLOADS_PER_SESSION_PER_HOUR", 30),
		RateLimitUploadsPerIPPerHour:         getEnvInt("RATE_LIMIT_UPLOADS_PER_IP_PER_HOUR", 60),
		RateLimitTranscodesPerSessionPerHour: getEnvInt("RATE_LIMIT_TRANSCODES_PER_SESSION_PER_HOUR", 10),
		RateLimitTranscodesPerIPPerHour:      getEnvInt("RATE_LIMIT_TRANSCODES_PER_IP_PER_HOUR", 20),
		RateLimitAnalysisPerSessionPerHour:   getEnvInt("RATE_LIMIT_ANALYSIS_PER_SESSION_PER_HOUR", 20),
		RateLimitAnalysisPerIPPerHour:        getEnvInt("RATE_LIMIT_ANALYSIS_PER_IP_PER_HOUR", 40),
		RateLimitAuditAllowed:                getEnvBool("RATE_LIMIT_AUDIT_ALLOWED", false),

		// Tier limits — see the struct comment. These defaults are the
		// published matrix; /pricing renders them via GET /api/tiers.
		//
		// DEFAULT ON as of the Phase 4 UI release. The quota meter, the
		// contextual account panel and /pricing all ship in the same change, so
		// a 429 now arrives with an explanation and a way forward instead of
		// being a dead end.
		//
		// This is the single kill switch for the whole tier system, not just
		// for counting: with it off, middleware.TierContext attaches no
		// resolver, so the upload byte caps, the source-duration check, the
		// transcode ladder and the Studio project cap all fall back to the
		// pre-tier global-config behaviour exactly.
		TierQuotaEnabled: getEnvBool("TIER_QUOTA_ENABLED", true),

		ResultOwnershipEnforce: getEnvBool("RESULT_OWNERSHIP_ENFORCE", true),

		TierAnonOpsPerDay:             anonOps,
		TierAnonExportsPerDay:         getEnvInt("TIER_ANON_EXPORTS_PER_DAY", anonOps),
		TierAnonAIOpsPerDay:           getEnvInt("TIER_ANON_AI_OPS_PER_DAY", 1),
		TierAnonMaxFileBytes:          getEnvInt64("TIER_ANON_MAX_FILE_BYTES", 200*1024*1024),
		TierAnonMaxVideoSeconds:       getEnvInt("TIER_ANON_MAX_VIDEO_SECONDS", 5*60),
		TierAnonMaxOutputHeight:       getEnvInt("TIER_ANON_MAX_OUTPUT_HEIGHT", 1080),
		TierAnonMaxLadderHeight:       getEnvInt("TIER_ANON_MAX_LADDER_HEIGHT", 720),
		TierAnonStudioProjects:        getEnvIntDefault("TIER_ANON_STUDIO_PROJECTS", 1),
		TierAnonRetentionHours:        getEnvInt("TIER_ANON_RETENTION_HOURS", 24),
		TierAnonStudioVersionAutoKeep: getEnvInt("TIER_ANON_STUDIO_VERSION_AUTO_KEEP", 20),

		TierFreeOpsPerDay:             freeOps,
		TierFreeExportsPerDay:         getEnvInt("TIER_FREE_EXPORTS_PER_DAY", freeOps),
		TierFreeAIOpsPerDay:           getEnvInt("TIER_FREE_AI_OPS_PER_DAY", 5),
		TierFreeMaxFileBytes:          getEnvInt64("TIER_FREE_MAX_FILE_BYTES", 1024*1024*1024),
		TierFreeMaxVideoSeconds:       getEnvInt("TIER_FREE_MAX_VIDEO_SECONDS", 20*60),
		TierFreeMaxOutputHeight:       getEnvInt("TIER_FREE_MAX_OUTPUT_HEIGHT", 1440),
		TierFreeMaxLadderHeight:       getEnvInt("TIER_FREE_MAX_LADDER_HEIGHT", 1080),
		TierFreeStudioProjects:        getEnvIntDefault("TIER_FREE_STUDIO_PROJECTS", 5),
		TierFreeRetentionHours:        getEnvInt("TIER_FREE_RETENTION_HOURS", 7*24),
		TierFreeStudioVersionAutoKeep: getEnvInt("TIER_FREE_STUDIO_VERSION_AUTO_KEEP", 20),

		TierPremiumOpsPerDay:             premiumOps,
		TierPremiumExportsPerDay:         getEnvInt("TIER_PREMIUM_EXPORTS_PER_DAY", premiumOps),
		TierPremiumAIOpsPerDay:           getEnvInt("TIER_PREMIUM_AI_OPS_PER_DAY", 60),
		TierPremiumMaxFileBytes:          getEnvInt64("TIER_PREMIUM_MAX_FILE_BYTES", 5*1024*1024*1024),
		TierPremiumMaxVideoSeconds:       getEnvInt("TIER_PREMIUM_MAX_VIDEO_SECONDS", 2*60*60),
		TierPremiumMaxOutputHeight:       getEnvInt("TIER_PREMIUM_MAX_OUTPUT_HEIGHT", 2160),
		TierPremiumMaxLadderHeight:       getEnvInt("TIER_PREMIUM_MAX_LADDER_HEIGHT", 2160),
		TierPremiumStudioProjects:        getEnvIntDefault("TIER_PREMIUM_STUDIO_PROJECTS", -1),
		TierPremiumRetentionHours:        getEnvInt("TIER_PREMIUM_RETENTION_HOURS", 30*24),
		TierPremiumStudioVersionAutoKeep: getEnvInt("TIER_PREMIUM_STUDIO_VERSION_AUTO_KEEP", 20),

		// Operator allowlist — see the struct doc. Same loader as
		// DR_ALLOWED_EMAILS so membership is a plain lowercase compare.
		OperatorEmails: splitCSVLower(getEnv("OPERATOR_EMAILS", "")),

		// Billing — see the struct doc. Dark by default.
		BillingEnabled:            getEnvBool("BILLING_ENABLED", false),
		StripeSecretKey:           strings.TrimSpace(getEnv("STRIPE_SECRET_KEY", "")),
		StripeWebhookSecret:       strings.TrimSpace(getEnv("STRIPE_WEBHOOK_SECRET", "")),
		StripePricePremiumMonthly: strings.TrimSpace(getEnv("STRIPE_PRICE_PREMIUM_MONTHLY", "")),
		StripePricePremiumYearly:  strings.TrimSpace(getEnv("STRIPE_PRICE_PREMIUM_YEARLY", "")),
		StripeAPIVersion:          strings.TrimSpace(getEnv("STRIPE_API_VERSION", "")),
		StripeAutomaticTax:        getEnvBool("STRIPE_AUTOMATIC_TAX", true),
		BillingGraceDays:          getEnvInt("BILLING_GRACE_DAYS", 7),
		BillingPublicBaseURL:      strings.TrimRight(strings.TrimSpace(getEnv("BILLING_PUBLIC_BASE_URL", "https://media-manipulator.com")), "/"),
		BillingPriceMonthlyUSD:    getEnvInt("BILLING_PRICE_MONTHLY_USD", 7),
		BillingPriceYearlyUSD:     getEnvInt("BILLING_PRICE_YEARLY_USD", 70),

		// FFmpeg validation
		MaxVideoDurationSeconds: getEnvInt("MAX_VIDEO_DURATION_SECONDS", 7200),
		MaxVideoWidth:           getEnvInt("MAX_VIDEO_WIDTH", 3840),
		MaxVideoHeight:          getEnvInt("MAX_VIDEO_HEIGHT", 2160),
		MaxVideoPixels:          getEnvInt64("MAX_VIDEO_PIXELS", 8294400),
		MaxVideoFPS:             getEnvInt("MAX_VIDEO_FPS", 120),
		MaxAudioDurationSeconds: getEnvInt("MAX_AUDIO_DURATION_SECONDS", 14400),

		// GPU scheduler
		GPUSchedulerEnabled:                        getEnvBool("GPU_SCHEDULER_ENABLED", true),
		GPUSchedulerDevices:                        splitCSV(getEnv("GPU_SCHEDULER_DEVICES", "")),
		GPUSchedulerDefaultWhisperDevice:           getEnv("GPU_SCHEDULER_DEFAULT_WHISPER_DEVICE", ""),
		GPUSchedulerDefaultRealESRGANDevice:        getEnv("GPU_SCHEDULER_DEFAULT_REALESRGAN_DEVICE", ""),
		GPUSchedulerDefaultVLMDevice:               getEnv("GPU_SCHEDULER_DEFAULT_VLM_DEVICE", ""),
		GPUSchedulerWhisperConcurrencyPerDevice:    getEnvInt("GPU_SCHEDULER_WHISPER_CONCURRENCY_PER_DEVICE", 1),
		GPUSchedulerRealESRGANConcurrencyPerDevice: getEnvInt("GPU_SCHEDULER_REALESRGAN_CONCURRENCY_PER_DEVICE", 1),
		GPUSchedulerVLMConcurrencyPerDevice:        getEnvInt("GPU_SCHEDULER_VLM_CONCURRENCY_PER_DEVICE", 1),

		// Safety / compliance
		SafetyIncidentRetentionDays: getEnvInt("SAFETY_INCIDENT_RETENTION_DAYS", 365),

		// AI Video Restoration
		RestoreEnabled:                    getEnvBool("RESTORE_ENABLED", true),
		RestoreBasicVSRPPEnabled:          getEnvBool("RESTORE_BASICVSRPP_ENABLED", true),
		RestoreMaxClipSeconds:             getEnvFloat("RESTORE_MAX_CLIP_SECONDS", 15),
		RestoreRecommendedClipSeconds:     getEnvFloat("RESTORE_RECOMMENDED_CLIP_SECONDS", 10),
		RestoreMaxFrames:                  getEnvInt("RESTORE_MAX_FRAMES", 450),
		RestoreMaxSourceWidth:             getEnvInt("RESTORE_MAX_SOURCE_WIDTH", 1920),
		RestoreMaxSourceHeight:            getEnvInt("RESTORE_MAX_SOURCE_HEIGHT", 1080),
		RestoreMaxConcurrentJobs:          getEnvInt("RESTORE_MAX_CONCURRENT_JOBS", 1),
		RestoreModelTimeout:               time.Duration(getEnvInt("RESTORE_MODEL_TIMEOUT_SECONDS", 4500)) * time.Second,
		RestoreUploadTimeout:              time.Duration(getEnvInt("RESTORE_UPLOAD_TIMEOUT_SECONDS", 3*60*60)) * time.Second,
		RestoreResultPresignTTL:           time.Duration(getEnvInt("RESTORE_RESULT_PRESIGN_TTL_SECONDS", 21600)) * time.Second,
		RestoreRateLimitPerSessionPerHour: getEnvInt("RESTORE_RATE_LIMIT_PER_SESSION_PER_HOUR", 2),
		RestoreRateLimitPerIPPerHour:      getEnvInt("RESTORE_RATE_LIMIT_PER_IP_PER_HOUR", 4),
		AIRestoreCUDAGPU:                  getEnvIntDefault("AI_RESTORE_CUDA_GPU", 0),
		AIRestoreVulkanGPU:                getEnvIntDefault("AI_RESTORE_VULKAN_GPU", 1),
		AIRestorePython:                   getEnv("AI_RESTORE_PYTHON", "/opt/media-manipulator-ai/venvs/restore-sr/bin/python"),
		AIRestoreMMPython:                 getEnv("AI_RESTORE_MM_PYTHON", "/opt/media-manipulator-ai/venvs/restore-vsr-mm/bin/python"),
		AIRestoreFramesScript:             getEnv("AI_RESTORE_FRAMES_SCRIPT", "/opt/media-manipulator-ai/scripts/restore_frames.py"),
		AIRestoreVideoScript:              getEnv("AI_RESTORE_VIDEO_SCRIPT", "/opt/media-manipulator-ai/scripts/restore_video.py"),
		AIRestoreModelsDir:                getEnv("AI_RESTORE_MODELS_DIR", "/opt/media-manipulator-ai/models/restore"),
		AIRestoreReposDir:                 getEnv("AI_RESTORE_REPOS_DIR", "/opt/media-manipulator-ai/repos"),
		RestoreEstSecondsPerFrame: map[string]float64{
			"realesrgan": getEnvFloat("RESTORE_EST_SPF_REALESRGAN", 0.8),
			"swinir":     getEnvFloat("RESTORE_EST_SPF_SWINIR", 3.5),
			"hat":        getEnvFloat("RESTORE_EST_SPF_HAT", 5.0),
			"basicvsrpp": getEnvFloat("RESTORE_EST_SPF_BASICVSRPP", 1.0),
			"rvrt":       getEnvFloat("RESTORE_EST_SPF_RVRT", 3.0),
			"vrt":        getEnvFloat("RESTORE_EST_SPF_VRT", 7.0),
		},
		RestoreVRAMMiB: map[string]int64{
			"realesrgan": getEnvInt64("RESTORE_VRAM_MIB_REALESRGAN", 3000),
			"swinir":     getEnvInt64("RESTORE_VRAM_MIB_SWINIR", 9000),
			"hat":        getEnvInt64("RESTORE_VRAM_MIB_HAT", 10000),
			"basicvsrpp": getEnvInt64("RESTORE_VRAM_MIB_BASICVSRPP", 11000),
			"rvrt":       getEnvInt64("RESTORE_VRAM_MIB_RVRT", 12000),
			"vrt":        getEnvInt64("RESTORE_VRAM_MIB_VRT", 14000),
		},
		RestoreRequireFirebaseAuth:   getEnvBool("RESTORE_REQUIRE_FIREBASE_AUTH", false),
		FirebaseProjectID:            getEnv("FIREBASE_PROJECT_ID", ""),
		GoogleApplicationCredentials: getEnv("GOOGLE_APPLICATION_CREDENTIALS", ""),

		// Double Raven portal authz backstop (see struct doc). Lowercased at load
		// so the middleware can do a plain case-insensitive membership check.
		DRAllowedEmails: splitCSVLower(getEnv("DR_ALLOWED_EMAILS", "")),

		ImageRestoreEnabled:                    getEnvBool("IMAGE_RESTORE_ENABLED", true),
		ImageRestoreCodeFormerEnabled:          getEnvBool("IMAGE_RESTORE_CODEFORMER_ENABLED", false),
		ImageRestoreMaxSourceWidth:             getEnvInt("IMAGE_RESTORE_MAX_SOURCE_WIDTH", 12000),
		ImageRestoreMaxSourceHeight:            getEnvInt("IMAGE_RESTORE_MAX_SOURCE_HEIGHT", 12000),
		ImageRestoreMaxOutputPixels:            getEnvInt64("IMAGE_RESTORE_MAX_OUTPUT_PIXELS", 67108864),
		ImageRestoreMaxOutputs:                 getEnvInt("IMAGE_RESTORE_MAX_OUTPUTS", 12),
		ImageRestoreMaxConcurrentJobs:          getEnvInt("IMAGE_RESTORE_MAX_CONCURRENT_JOBS", 1),
		ImageRestoreModelTimeout:               time.Duration(getEnvInt("IMAGE_RESTORE_MODEL_TIMEOUT_SECONDS", 1800)) * time.Second,
		ImageRestoreRateLimitPerSessionPerHour: getEnvInt("IMAGE_RESTORE_RATE_LIMIT_PER_SESSION_PER_HOUR", 6),
		ImageRestoreRateLimitPerIPPerHour:      getEnvInt("IMAGE_RESTORE_RATE_LIMIT_PER_IP_PER_HOUR", 12),
		AIFaceRestorePython:                    getEnv("AI_FACE_RESTORE_PYTHON", "/opt/media-manipulator-ai/venvs/face-restore/bin/python"),
		AIFaceRestoreScript:                    getEnv("AI_FACE_RESTORE_SCRIPT", "/opt/media-manipulator-ai/scripts/restore_image_faces.py"),
		AIPrecleanPython:                       getEnv("AI_PRECLEAN_PYTHON", "/opt/media-manipulator-ai/venvs/preclean/bin/python"),
		AIPrecleanScript:                       getEnv("AI_PRECLEAN_SCRIPT", "/opt/media-manipulator-ai/scripts/preclean_image.py"),
		ImageRestoreEstSecondsPerMegapixel: map[string]float64{
			"fbcnn":      getEnvFloat("IMAGE_RESTORE_EST_SPM_FBCNN", 3),
			"scunet":     getEnvFloat("IMAGE_RESTORE_EST_SPM_SCUNET", 8),
			"nafnet":     getEnvFloat("IMAGE_RESTORE_EST_SPM_NAFNET", 6),
			"realesrgan": getEnvFloat("IMAGE_RESTORE_EST_SPM_REALESRGAN", 2),
			"swinir":     getEnvFloat("IMAGE_RESTORE_EST_SPM_SWINIR", 12),
			"hat":        getEnvFloat("IMAGE_RESTORE_EST_SPM_HAT", 18),
			"gfpgan":     getEnvFloat("IMAGE_RESTORE_EST_SPM_GFPGAN", 6),
			"codeformer": getEnvFloat("IMAGE_RESTORE_EST_SPM_CODEFORMER", 8),
		},
		ImageRestoreVRAMMiB: map[string]int64{
			"fbcnn":      getEnvInt64("IMAGE_RESTORE_VRAM_MIB_FBCNN", 3000),
			"scunet":     getEnvInt64("IMAGE_RESTORE_VRAM_MIB_SCUNET", 4000),
			"nafnet":     getEnvInt64("IMAGE_RESTORE_VRAM_MIB_NAFNET", 4000),
			"realesrgan": getEnvInt64("IMAGE_RESTORE_VRAM_MIB_REALESRGAN", 3000),
			"swinir":     getEnvInt64("IMAGE_RESTORE_VRAM_MIB_SWINIR", 9000),
			"hat":        getEnvInt64("IMAGE_RESTORE_VRAM_MIB_HAT", 10000),
			"gfpgan":     getEnvInt64("IMAGE_RESTORE_VRAM_MIB_GFPGAN", 5000),
			"codeformer": getEnvInt64("IMAGE_RESTORE_VRAM_MIB_CODEFORMER", 6000),
		},

		// Content Studio — getEnvIntDefault for the GPU index so device 0 is a
		// valid override (getEnvInt rejects <= 0).
		ContentStudioGPUIndex:    getEnvIntDefault("CONTENT_STUDIO_GPU_INDEX", 1),
		ContentStudioProxyHeight: getEnvInt("CONTENT_STUDIO_PROXY_HEIGHT", 720),
		ContentStudioS3Prefix:    getEnv("CONTENT_STUDIO_S3_PREFIX", "studio"),
		ContentStudioFontFile:    getEnv("CONTENT_STUDIO_FONT_FILE", "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"),
		ContentStudioFontsDir:    getEnv("CONTENT_STUDIO_FONTS_DIR", "assets/fonts/studio"),

		// AI Document Scan
		DocumentScanEnabled:                    getEnvBool("DOCUMENT_SCAN_ENABLED", true),
		DocumentScanDocxEnabled:                getEnvBool("DOCUMENT_SCAN_DOCX_ENABLED", true),
		DocumentScanHandwritingEnabled:         getEnvBool("DOCUMENT_SCAN_HANDWRITING_ENABLED", true),
		DocumentScanSecondOpinionEnabled:       getEnvBool("DOCUMENT_SCAN_SECOND_OPINION_ENABLED", false),
		DocumentScanSummaryEnabled:             getEnvBool("DOCUMENT_SCAN_SUMMARY_ENABLED", false),
		AIDocumentOCRPython:                    getEnv("AI_DOCUMENT_OCR_PYTHON", "/opt/media-manipulator-ai/venvs/document-ocr/bin/python"),
		AIDocumentOCRScript:                    getEnv("AI_DOCUMENT_OCR_SCRIPT", "/opt/media-manipulator-ai/scripts/document_ocr.py"),
		AIDocumentOCRGPU:                       getEnvIntDefault("AI_DOCUMENT_OCR_GPU", 1),
		AIDocumentOCRSecondaryGPU:              getEnvIntDefault("AI_DOCUMENT_OCR_SECONDARY_GPU", 0),
		PandocBin:                              getEnv("PANDOC_BIN", "pandoc"),
		DocumentScanOllamaURL:                  getEnv("DOCUMENT_SCAN_OLLAMA_URL", getEnv("OLLAMA_URL", "http://localhost:11434")),
		OllamaVLMModel:                         getEnv("OLLAMA_VLM_MODEL", "qwen3-vl:8b-instruct-q8_0"),
		OllamaTextModel:                        getEnv("OLLAMA_TEXT_MODEL", "qwen3.5:9b-q8_0"),
		PaddleOCRVLEndpoint:                    getEnv("PADDLEOCR_VL_ENDPOINT", "http://127.0.0.1:8080/v1"),
		PaddleOCRVLModel:                       getEnv("PADDLEOCR_VL_MODEL", "PaddleOCR-VL-1.6-0.9B"),
		DocumentScanStructureEngine:            getEnv("DOCUMENT_SCAN_STRUCTURE_ENGINE", "paddleocr-vl"),
		DocumentScanSecondOpinionEngine:        getEnv("DOCUMENT_SCAN_SECOND_OPINION_ENGINE", "paddleocr-vl"),
		TrOCRModel:                             getEnv("TROCR_MODEL", "microsoft/trocr-large-handwritten"),
		DocumentScanLanguages:                  splitCSV(getEnv("DOCUMENT_SCAN_LANGUAGES", "eng")),
		DocumentScanMaxImages:                  getEnvInt("DOCUMENT_SCAN_MAX_IMAGES", 50),
		DocumentScanMaxPages:                   getEnvInt("DOCUMENT_SCAN_MAX_PAGES", 100),
		DocumentScanMaxImageBytes:              getEnvInt64("DOCUMENT_SCAN_MAX_IMAGE_BYTES", 25*1024*1024),
		DocumentScanModelTimeout:               time.Duration(getEnvInt("DOCUMENT_SCAN_MODEL_TIMEOUT_SECONDS", 2400)) * time.Second,
		DocumentScanMaxConcurrentJobs:          getEnvInt("DOCUMENT_SCAN_MAX_CONCURRENT_JOBS", 1),
		DocumentScanRateLimitPerSessionPerHour: getEnvInt("DOCUMENT_SCAN_RATE_LIMIT_PER_SESSION_PER_HOUR", 20),
		DocumentScanRateLimitPerIPPerHour:      getEnvInt("DOCUMENT_SCAN_RATE_LIMIT_PER_IP_PER_HOUR", 40),

		// DR AI Chat Test Lab (see struct doc). The key is the one true secret
		// here — never log or echo it.
		OpenRouterAPIKey:  getEnv("OPENROUTER_API_KEY", ""),
		OpenRouterBaseURL: getEnv("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),
		// Gemini/Qwen/Grok additions are deliberately EXACT ids, not google//
		// qwen//xai/ prefixes — those providers publish unrelated models
		// (embeddings, image gen, code variants) that would pollute the
		// picker. Gemini `-preview` slugs drift over time: docs/dr-chatlab-
		// stats-feedback-verification.md has a catalog curl to confirm each
		// slug resolves; a slug missing from the live catalog is silently
		// absent from the picker. (Embedding models for the entity-resolution
		// pipeline are a separate future feature — different endpoint, not
		// this allowlist.)
		// NOTE: OpenRouter's xAI provider slug is HYPHENATED — x-ai, per
		// openrouter.ai/x-ai — an unhyphenated xai/… rule matches nothing.
		DRChatLabModelRules: splitCSVLower(getEnv("DR_CHATLAB_MODEL_RULES",
			"anthropic/,openai/,z-ai/glm-5.2,moonshotai/kimi-k2.6,"+
				"google/gemini-3.1-pro-preview,google/gemini-3-pro-preview,google/gemini-3.1-flash-lite,google/gemini-3.5-flash,qwen/qwen3.7-plus,"+
				"google/gemini-3-flash-preview,google/gemini-2.5-flash,google/gemini-2.0-flash-001,qwen/qwen3.6-plus,qwen/qwen3.6-flash,qwen/qwen3.7-max,qwen/qwen3-vl-235b-a22b-instruct,"+
				"x-ai/grok-4.5,x-ai/grok-4.3")),
		DRChatLabTitleModel:      getEnv("DR_CHATLAB_TITLE_MODEL", ""),
		DRChatLabMaxOutputTokens: getEnvInt("DR_CHATLAB_MAX_OUTPUT_TOKENS", 8192),
		DRChatLabAttributionURL:  getEnv("DR_CHATLAB_ATTRIBUTION_URL", "https://media-manipulator.com"),

		DRChatLabMemoryModel:       getEnv("DR_CHATLAB_MEMORY_MODEL", ""),
		DRChatLabMemoryMaxChars:    getEnvInt("DR_CHATLAB_MEMORY_MAX_CHARS", 8192),
		DRChatLabMemoryJobHour:     getEnvInt("DR_CHATLAB_MEMORY_JOB_HOUR", 4),
		DRChatLabMemoryJobTZ:       getEnv("DR_CHATLAB_MEMORY_JOB_TZ", "America/Denver"),
		DRChatLabToolMaxRounds:     getEnvInt("DR_CHATLAB_TOOL_MAX_ROUNDS", 5),
		DRChatLabAssetReadCapBytes: getEnvInt("DR_CHATLAB_ASSET_READ_CAP_BYTES", 49152),

		// DR Portal desktop app installer keys (see struct doc — literal spaces).
		DRDesktopMacArm64Key: getEnv("DR_DESKTOP_MAC_ARM64_KEY", "double-raven/desktop/mac/apple/Double Raven Portal-0.1.0-arm64.dmg"),
		DRDesktopMacIntelKey: getEnv("DR_DESKTOP_MAC_INTEL_KEY", "double-raven/desktop/mac/intel/Double Raven Portal-0.1.0.dmg"),
		DRDesktopWindowsKey:  getEnv("DR_DESKTOP_WINDOWS_KEY", "double-raven/desktop/windows/Double Raven Portal Setup 0.1.0.exe"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return defaultValue
	}
	return parsed
}

func getEnvIntDefault(key string, defaultValue int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return parsed
}

func getEnvBool(key string, defaultValue bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return defaultValue
	}
	return parsed
}

func getEnvInt64(key string, defaultValue int64) int64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return defaultValue
	}
	return parsed
}

func getEnvFloat(key string, defaultValue float64) float64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed <= 0 {
		return defaultValue
	}
	return parsed
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := make([]string, 0)
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// splitCSVLower is splitCSV plus ASCII-lowercasing. Used for case-insensitive
// allowlists such as DR_ALLOWED_EMAILS so membership checks are a plain compare.
func splitCSVLower(raw string) []string {
	parts := splitCSV(raw)
	for i, p := range parts {
		parts[i] = strings.ToLower(p)
	}
	return parts
}
