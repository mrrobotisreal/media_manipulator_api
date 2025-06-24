package config

import (
	"os"
)

type Config struct {
	Port        string
	UploadDir   string
	OutputDir   string
	MaxFileSize int64 // in bytes
}

func Load() *Config {
	return &Config{
		Port:        getEnv("PORT", "9090"),
		UploadDir:   getEnv("UPLOAD_DIR", "uploads"),
		OutputDir:   getEnv("OUTPUT_DIR", "outputs"),
		MaxFileSize: 100 * 1024 * 1024, // 100MB default
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
