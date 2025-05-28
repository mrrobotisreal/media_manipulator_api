package main

import (
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

	// Initialize services
	jobManager := services.NewJobManager()
	converter := services.NewConverter()

	// Initialize handlers
	conversionHandler := handlers.NewConversionHandler(jobManager, converter)

	// Setup router
	router := setupRouter(conversionHandler)

	// Start server
	log.Printf("Starting server on port %s", cfg.Port)
	log.Fatal(http.ListenAndServe(":"+cfg.Port, router))
}

func setupRouter(conversionHandler *handlers.ConversionHandler) *gin.Engine {
	// Set Gin mode
	gin.SetMode(gin.ReleaseMode)

	router := gin.New()
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	// CORS configuration
	corsConfig := cors.DefaultConfig()
	corsConfig.AllowOrigins = []string{"http://localhost:3000", "http://localhost:5173", "http://localhost:5174", "http://192.168.4.22:5174"} // Vite dev server
	corsConfig.AllowMethods = []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"}
	corsConfig.AllowHeaders = []string{"Origin", "Content-Type", "Accept", "Authorization"}
	router.Use(cors.New(corsConfig))

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
