package main

import (
	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/handlers"
)

// summarizeStartPath is the one video summary route that spends allowance
// (MM74): ai_ops in quotaRoutes and the summarize_start rate bucket.
const summarizeStartPath = "/api/summarize/start"

// summarizeQuotaGate is the ai_ops pre-quota gate. It refuses an anonymous
// POST /api/summarize/start with 403 signin_required BEFORE RequireQuota
// charges anything, and is a no-op for every other ai_ops route.
func summarizeQuotaGate() gin.HandlerFunc {
	signIn := handlers.SummarizeSignInGate()
	return func(c *gin.Context) {
		if c.Request.URL.Path == summarizeStartPath {
			signIn(c)
		}
	}
}
