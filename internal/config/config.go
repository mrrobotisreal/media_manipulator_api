package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const DefaultPort = "59997"

type Config struct {
	Port            string
	UploadDir       string
	OutputDir       string
	TempDir         string
	MaxFileSize     int64
	MaxVideoUpload  int64
	CommandTimeout  time.Duration
	AnalysisWorkers int
	AWSRegion       string
	S3Bucket        string
	S3Endpoint      string
	S3PresignTTL    time.Duration

	// Local AI toolchain (Phase 1: audio + image AI tools)
	AIEnabled         bool
	AIRootDir         string
	AICUDAGPU         int
	AIVulkanGPU       int
	DeepFilterBin     string
	DemucsBin         string
	VisionPython      string
	FacePrivacyScript string
	TextRedactScript  string
	RembgBin          string
	RembgEnvScript    string
	RembgModelDir     string
	RealESRGANBin     string
	VulkanEnvScript   string
}

func Load() *Config {
	maxFileSize := getEnvInt64("MAX_FILE_SIZE_BYTES", 1000*1024*1024)
	return &Config{
		Port:            DefaultPort,
		UploadDir:       getEnv("UPLOAD_DIR", "uploads"),
		OutputDir:       getEnv("OUTPUT_DIR", "outputs"),
		TempDir:         getEnv("TEMP_DIR", "temp"),
		MaxFileSize:     maxFileSize,
		MaxVideoUpload:  getEnvInt64("MAX_VIDEO_UPLOAD_SIZE_BYTES", maxFileSize),
		CommandTimeout:  time.Duration(getEnvInt("COMMAND_TIMEOUT_SECONDS", 6*60*60)) * time.Second,
		AnalysisWorkers: getEnvInt("ANALYSIS_WORKERS", 1),
		AWSRegion:       getEnv("AWS_REGION", "us-west-2"),
		S3Bucket:        getEnv("S3_BUCKET", "media-manipulator"),
		S3Endpoint:      getEnv("AWS_S3_ENDPOINT", ""),
		S3PresignTTL:    time.Duration(getEnvInt("S3_PRESIGN_TTL_SECONDS", 15*60)) * time.Second,

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
