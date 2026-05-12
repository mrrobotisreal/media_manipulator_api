package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

func loadDotEnv() {
	_ = godotenv.Load(".env")
	_ = godotenv.Load(".env.local")
}

func main() {
	loadDotEnv()

	cfg := config.Load()
	createDirs(cfg)

	jobManager := services.NewJobManager()
	converter := services.NewConverter()
	inspector := services.NewMediaInspector(cfg.CommandTimeout)
	analysisQueue := services.NewAnalysisQueue(cfg, inspector)
	analysisQueue.Start()

	conversionHandler := handlers.NewConversionHandler(jobManager, converter, cfg, inspector, analysisQueue)
	router := setupRouter(conversionHandler)

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 15 * time.Second,
	}

	log.Printf("media-manipulator-api listening on :%s", cfg.Port)
	log.Fatal(server.ListenAndServe())
}

func setupRouter(conversionHandler *handlers.ConversionHandler) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	router := gin.Default()
	_ = router.SetTrustedProxies([]string{"127.0.0.1", "::1"})

	corsConfig := cors.DefaultConfig()
	corsConfig.AllowOrigins = []string{
		"https://media-manipulator.com",
		"https://www.media-manipulator.com",
	}
	corsConfig.AllowMethods = []string{"GET", "POST", "OPTIONS"}
	corsConfig.AllowHeaders = []string{
		"Origin",
		"Content-Type",
		"Accept",
		"Authorization",
		"X-Requested-With",
		"X-MM-Visitor-ID",
		"X-MM-Session-ID",
	}
	corsConfig.AllowCredentials = false
	corsConfig.ExposeHeaders = []string{"Content-Length", "Content-Disposition"}
	router.Use(cors.New(corsConfig))

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "healthy", "service": "media_manipulator_api"})
	})

	api := router.Group("/api")
	{
		api.GET("/health", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "healthy", "service": "media_manipulator_api"})
		})
		handlers.RegisterConversionRoutes(api, conversionHandler)
	}

	return router
}

func createDirs(cfg *config.Config) {
	for _, dir := range []string{cfg.UploadDir, cfg.OutputDir, cfg.TempDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Fatalf("failed to create directory %s: %v", dir, err)
		}
	}
}
