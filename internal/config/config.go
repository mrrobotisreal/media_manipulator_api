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
