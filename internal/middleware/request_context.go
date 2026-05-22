// Package middleware contains gin middlewares that build the per-request
// context (request id, visitor/session ids, ip, geo) and the access log
// writer.
package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mrrobotisreal/media_manipulator_api/internal/geo"
	"github.com/mrrobotisreal/media_manipulator_api/internal/logger"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

const (
	headerVisitorID = "X-MM-Visitor-ID"
	headerSessionID = "X-MM-Session-ID"
	headerRequestID = "X-MM-Request-ID"
)

// RequestContext attaches a request id, visitor/session ids, and a Fields
// struct to the gin context. It also sets the `X-MM-Request-ID` response
// header so the client can correlate.
func RequestContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := strings.TrimSpace(c.GetHeader(headerRequestID))
		if requestID == "" {
			requestID = uuid.NewString()
		}
		visitorID := normalizeUUID(c.GetHeader(headerVisitorID))
		sessionID := normalizeUUID(c.GetHeader(headerSessionID))

		fields := &logger.Fields{
			RequestID: requestID,
			VisitorID: visitorID,
			SessionID: sessionID,
			Route:     c.FullPath(),
		}
		c.Set(logger.GinKey, fields)
		c.Writer.Header().Set(headerRequestID, requestID)

		// Stitch the context.Context too so services that don't take a
		// *gin.Context still see our IDs.
		ctx := c.Request.Context()
		ctx = context.WithValue(ctx, logger.CtxRequestID, requestID)
		if visitorID != "" {
			ctx = context.WithValue(ctx, logger.CtxVisitorID, visitorID)
		}
		if sessionID != "" {
			ctx = context.WithValue(ctx, logger.CtxSessionID, sessionID)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// AccessLog records mm_api_requests for every request that passes through.
//
// Geo enrichment is best-effort: when the enricher is nil or the lookup
// fails we still write the row with the raw IP.
func AccessLog(store *telemetry.Store, enricher *geo.Enricher) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		duration := time.Since(start)

		ip := geo.ExtractIP(c)
		cfIP := strings.TrimSpace(c.GetHeader("CF-Connecting-IP"))
		xff := strings.TrimSpace(c.GetHeader("X-Forwarded-For"))
		cfRay := strings.TrimSpace(c.GetHeader("CF-Ray"))
		cfCountry := strings.TrimSpace(c.GetHeader("CF-IPCountry"))
		ua := strings.TrimSpace(c.GetHeader("User-Agent"))
		origin := strings.TrimSpace(c.GetHeader("Origin"))
		referer := strings.TrimSpace(c.GetHeader("Referer"))

		fields, _ := c.Get(logger.GinKey)
		f, _ := fields.(*logger.Fields)
		if f == nil {
			f = &logger.Fields{}
		}

		// Geo enrich session/visitor in the background (best-effort).
		if store != nil && enricher != nil && ip != "" && (f.SessionID != "" || f.VisitorID != "") {
			go func(ip, sessionID, visitorID, ua string) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				entry, _ := enricher.Lookup(ctx, ip)
				if entry == nil {
					entry = &geo.Entry{}
				}
				if visitorID != "" {
					store.UpsertVisitor(ctx, telemetry.VisitorUpsert{
						VisitorID:   visitorID,
						UserAgent:   ua,
						IP:          ip,
						CountryCode: entry.CountryCode,
					})
				}
				if sessionID != "" {
					store.UpsertSession(ctx, telemetry.SessionUpsert{
						SessionID:   sessionID,
						VisitorID:   visitorID,
						UserAgent:   ua,
						IP:          ip,
						CountryCode: entry.CountryCode,
						Region:      entry.Region,
						City:        entry.City,
						Lat:         entry.Lat,
						Lon:         entry.Lon,
						Timezone:    entry.Timezone,
						ASNNumber:   entry.ASNNumber,
						ASNOrg:      entry.ASNOrg,
					})
				}
			}(ip, f.SessionID, f.VisitorID, ua)
		}

		if store == nil {
			return
		}
		go store.InsertAPIRequest(context.Background(), telemetry.RequestLog{
			RequestID:      f.RequestID,
			VisitorID:      f.VisitorID,
			SessionID:      f.SessionID,
			JobID:          f.JobID,
			Method:         c.Request.Method,
			Route:          c.FullPath(),
			Path:           c.Request.URL.Path,
			QueryHash:      hashQuery(c.Request.URL.RawQuery),
			StatusCode:     c.Writer.Status(),
			DurationMS:     int(duration / time.Millisecond),
			RequestBytes:   c.Request.ContentLength,
			ResponseBytes:  int64(c.Writer.Size()),
			IP:             ip,
			CFConnectingIP: cfIP,
			XForwardedFor:  xff,
			CFRay:          cfRay,
			CFIPCountry:    cfCountry,
			UserAgent:      ua,
			Origin:         origin,
			Referer:        referer,
			Tool:           f.Tool,
			Stage:          f.Stage,
			CreatedAt:      time.Now().UTC(),
		})
	}
}

// SetTool annotates the current request with a tool/stage label so the
// access log writes them out.
func SetTool(c *gin.Context, tool, stage string) {
	if v, ok := c.Get(logger.GinKey); ok {
		if f, ok := v.(*logger.Fields); ok && f != nil {
			if tool != "" {
				f.Tool = tool
			}
			if stage != "" {
				f.Stage = stage
			}
		}
	}
}

// SetJobID annotates the current request with a job id.
func SetJobID(c *gin.Context, jobID string) {
	if v, ok := c.Get(logger.GinKey); ok {
		if f, ok := v.(*logger.Fields); ok && f != nil {
			f.JobID = jobID
			ctx := context.WithValue(c.Request.Context(), logger.CtxJobID, jobID)
			c.Request = c.Request.WithContext(ctx)
		}
	}
}

func normalizeUUID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if _, err := uuid.Parse(s); err != nil {
		return ""
	}
	return s
}

func hashQuery(q string) string {
	if q == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(q))
	return hex.EncodeToString(sum[:])[:16]
}
