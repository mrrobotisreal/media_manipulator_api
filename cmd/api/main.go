package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

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

	// Check if it's in development mode
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
		// certFile := "/etc/letsencrypt/live/api.converter.winapps.io/fullchain.pem"
		// keyFile := "/etc/letsencrypt/live/api.converter.winapps.io/privkey.pem"

		// Validate TLS certificate files exist
		// if err := validateTLSFiles(certFile, keyFile); err != nil {
		// 	log.Fatalf("TLS certificate validation failed: %v", err)
		// }

		log.Printf("Starting HTTPS server on port %s (Production Mode)", cfg.Port)
		// log.Printf("Using TLS cert: %s", certFile)
		// log.Printf("Using TLS key: %s", keyFile)
		// log.Fatal(http.ListenAndServeTLS(":"+cfg.Port, certFile, keyFile, router))
		// USING HTTP and letting Cloudflare handle TLS
		log.Fatal(http.ListenAndServe(":"+cfg.Port, router))
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
		// Production mode - TEMPORARILY more permissive for debugging
		// TODO: Restrict this once the exact origin is identified
		// corsConfig.AllowAllOrigins = true
		// log.Printf("CORS: TEMPORARILY allowing all origins for debugging (Production Mode)")
		// log.Printf("TODO: Restrict CORS once the exact origin causing issues is identified")

		corsConfig.AllowOrigins = []string{
			"https://www.media-manipulator.com",
			"https://www.media-manipulator.com/",
			"https://media-manipulator.com",
			"https://media-manipulator.com/",
			"https://www.wintrow.io",
			"https://www.wintrow.io/",
			"https://wintrow.io",
			"https://wintrow.io/",
			"https://ui.converter.winapps.io",
			"https://ui.converter.winapps.io/",
			"https://api.converter.winapps.io",
			"https://api.converter.winapps.io/",
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

	// Enhanced debug middleware to log ALL request details
	router.Use(func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		referer := c.GetHeader("Referer")
		userAgent := c.GetHeader("User-Agent")
		method := c.Request.Method
		path := c.Request.URL.Path

		log.Printf("=== INCOMING REQUEST ===")
		log.Printf("Method: %s", method)
		log.Printf("Path: %s", path)
		log.Printf("Origin: '%s'", origin)
		log.Printf("Referer: '%s'", referer)
		log.Printf("User-Agent: %s", userAgent)
		log.Printf("Content-Type: %s", c.GetHeader("Content-Type"))
		log.Printf("Host: %s", c.Request.Host)
		log.Printf("Remote Addr: %s", c.Request.RemoteAddr)

		// Log all headers for debugging
		log.Printf("All Headers:")
		for name, values := range c.Request.Header {
			for _, value := range values {
				log.Printf("  %s: %s", name, value)
			}
		}
		log.Printf("========================")

		c.Next()
	})

	// API routes
	api := router.Group("/api")
	{
		api.GET("/health", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "healthy"})
		})

		// Debug endpoint for CORS testing
		api.GET("/debug", func(c *gin.Context) {
			response := gin.H{
				"message":     "Debug endpoint reached successfully",
				"origin":      c.GetHeader("Origin"),
				"referer":     c.GetHeader("Referer"),
				"user_agent":  c.GetHeader("User-Agent"),
				"host":        c.Request.Host,
				"remote_addr": c.Request.RemoteAddr,
				"method":      c.Request.Method,
				"path":        c.Request.URL.Path,
				"timestamp":   fmt.Sprintf("%v", time.Now()),
			}
			c.JSON(http.StatusOK, response)
		})

		// Debug endpoint for POST requests (like upload)
		api.POST("/debug", func(c *gin.Context) {
			response := gin.H{
				"message":      "Debug POST endpoint reached successfully",
				"origin":       c.GetHeader("Origin"),
				"content_type": c.GetHeader("Content-Type"),
				"method":       c.Request.Method,
				"timestamp":    fmt.Sprintf("%v", time.Now()),
			}
			c.JSON(http.StatusOK, response)
		})

		api.POST("/details", conversionHandler.IdentifyFile)
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
