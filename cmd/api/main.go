package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func main() {
	// Load configuration
	cfg := config.Load()

	// Create required directories
	createDirs()

	// Check if we're in development mode
	isDev := os.Getenv("DEV") == "true"

	// Initialize services
	jobManager := services.NewJobManager()
	converter := services.NewConverter()

	// Initialize handlers
	conversionHandler := handlers.NewConversionHandler(jobManager, converter)

	// Setup router
	router := setupRouter(conversionHandler, isDev)

	// Start server based on environment
	if isDev {
		// Development mode - use HTTP
		log.Printf("Starting HTTP server on port %s (Development Mode)", cfg.Port)
		log.Printf("Set DEV=true environment variable detected - using HTTP for development")
		log.Fatal(http.ListenAndServe(":"+cfg.Port, router))
	} else {
		// Production mode - use HTTPS with TLS
		certFile := "/etc/letsencrypt/live/api.converter.winapps.io/fullchain.pem"
		keyFile := "/etc/letsencrypt/live/api.converter.winapps.io/privkey.pem"

		// Validate TLS certificate files exist
		if err := validateTLSFiles(certFile, keyFile); err != nil {
			log.Fatalf("TLS certificate validation failed: %v", err)
		}

		log.Printf("Starting HTTPS server on port %s (Production Mode)", cfg.Port)
		log.Printf("Using TLS cert: %s", certFile)
		log.Printf("Using TLS key: %s", keyFile)
		log.Fatal(http.ListenAndServeTLS(":"+cfg.Port, certFile, keyFile, router))
	}
}

func setupRouter(conversionHandler *handlers.ConversionHandler, isDev bool) *gin.Engine {
	// Set Gin mode
	gin.SetMode(gin.ReleaseMode)

	router := gin.New()
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	// CORS configuration - different for dev vs production
	corsConfig := cors.DefaultConfig()

	if isDev {
		// Development mode - more permissive CORS
		corsConfig.AllowAllOrigins = true
		log.Printf("CORS: Allowing all origins (Development Mode)")
	} else {
		// Production mode - restricted CORS (include variations with/without trailing slash)
		corsConfig.AllowOrigins = []string{
			"https://ui.converter.winapps.io",
			"https://ui.converter.winapps.io/",  // Handle trailing slash
			"https://api.converter.winapps.io",  // Allow HTTPS self-requests
			"https://api.converter.winapps.io/", // Handle trailing slash for self-requests
		}
		log.Printf("CORS: Restricting origins to production domains (including trailing slash variations)")
	}

	corsConfig.AllowMethods = []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"}
	corsConfig.AllowHeaders = []string{
		"Origin",
		"Content-Type",
		"Accept",
		"Authorization",
		"X-Requested-With",
		"Content-Length",
		"Accept-Encoding",
		"X-CSRF-Token",
	}
	corsConfig.AllowCredentials = true
	corsConfig.ExposeHeaders = []string{"Content-Length", "Content-Disposition"}
	router.Use(cors.New(corsConfig))

	// Add debug middleware to log request origins (helps with CORS debugging)
	router.Use(func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			log.Printf("Request from origin: %s, Method: %s, Path: %s", origin, c.Request.Method, c.Request.URL.Path)
		}
		c.Next()
	})

	// API routes
	api := router.Group("/api")
	{
		api.GET("/health", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "healthy"})
		})

		api.POST("/upload", conversionHandler.UploadFile)
		api.GET("/job/:jobId", conversionHandler.GetJobStatus)
		api.GET("/download/:jobId", conversionHandler.DownloadFile)
	}

	return router
}

func createDirs() {
	dirs := []string{"uploads", "outputs"}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Fatalf("Failed to create directory %s: %v", dir, err)
		}
	}
}

func validateTLSFiles(certFile, keyFile string) error {
	if _, err := os.Stat(certFile); os.IsNotExist(err) {
		return fmt.Errorf("TLS certificate file %s does not exist", certFile)
	}
	if _, err := os.Stat(keyFile); os.IsNotExist(err) {
		return fmt.Errorf("TLS key file %s does not exist", keyFile)
	}
	return nil
}
