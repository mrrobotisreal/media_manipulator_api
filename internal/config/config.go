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

	// --- Operational telemetry / observability ----------------------------

	DatabaseURL            string
	AdminDatabaseURL       string
	APIAutoMigrate         bool
	MigrationsPath         string
	TelemetryDBEnabled     bool
	TelemetryAuditCommands bool
	TelemetryWriteTimeout  time.Duration

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
}

func Load() *Config {
	maxFileSize := getEnvInt64("MAX_FILE_SIZE_BYTES", 1000*1024*1024)
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

		// Operational DB
		DatabaseURL:            getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/media_manipulator?sslmode=disable"),
		AdminDatabaseURL:       getEnv("POSTGRES_ADMIN_DATABASE_URL", ""),
		APIAutoMigrate:         getEnvBool("API_AUTO_MIGRATE", false),
		MigrationsPath:         getEnv("MIGRATIONS_PATH", ""),
		TelemetryDBEnabled:     getEnvBool("TELEMETRY_DB_ENABLED", true),
		TelemetryAuditCommands: getEnvBool("TELEMETRY_AUDIT_COMMANDS", true),
		TelemetryWriteTimeout:  time.Duration(getEnvInt("TELEMETRY_WRITE_TIMEOUT_SECONDS", 5)) * time.Second,

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
