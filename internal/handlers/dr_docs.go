package handlers

import (
	"context"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// DrDocsHandler serves the Double Raven partner portal document endpoints
// (GET /api/dr/docs, GET /api/dr/docs/:slug). It is Postgres-backed by the
// dr_documents table (see the init_double_raven_docs migration) and, like the
// Content Studio handler, degrades to 503 rather than panicking when no DB pool
// is configured (the repo's DB-opt-in pattern — see db.New).
//
// Authorization is handled entirely upstream by
// middleware.RequireDoubleRavenAuth on the /dr group (see setupRouter); this
// handler assumes the caller is already an allowlisted, verified user.
type DrDocsHandler struct {
	pool *pgxpool.Pool
}

func NewDrDocsHandler(pool *pgxpool.Pool) *DrDocsHandler {
	return &DrDocsHandler{pool: pool}
}

// RegisterDrDocsRoutes wires the read-only document endpoints onto a group that
// is ALREADY prefixed /dr and gated by RequireDoubleRavenAuth (see setupRouter),
// so the concrete paths resolve to /api/dr/docs and /api/dr/docs/:slug.
func RegisterDrDocsRoutes(r gin.IRouter, h *DrDocsHandler) {
	r.GET("/docs", h.ListDocs)
	r.GET("/docs/:slug", h.GetDoc)
}

// drSlugPattern mirrors the kebab-case slug shape the seed + UI use. Validated
// before hitting the DB so obviously bad input is a cheap 400 rather than a
// query.
var drSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func (h *DrDocsHandler) dbReady(c *gin.Context) bool {
	if h.pool == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Document storage is unavailable"})
		return false
	}
	return true
}

// ListDocs returns published documents ordered most-recently-updated first. It
// selects METADATA COLUMNS ONLY — the content JSONB is deliberately excluded so
// a listing never ships document bodies over the wire.
func (h *DrDocsHandler) ListDocs(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	rows, err := h.pool.Query(ctx, `
SELECT id, slug, title, summary, status, created_at, updated_at
FROM dr_documents
WHERE status = 'published'
ORDER BY updated_at DESC
`)
	if err != nil {
		log.Printf("dr docs: list query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list documents"})
		return
	}
	defer rows.Close()

	docs := make([]models.DrDocSummary, 0)
	for rows.Next() {
		var d models.DrDocSummary
		if err := rows.Scan(&d.ID, &d.Slug, &d.Title, &d.Summary, &d.Status, &d.CreatedAt, &d.UpdatedAt); err != nil {
			log.Printf("dr docs: list scan failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list documents"})
			return
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		log.Printf("dr docs: list rows error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list documents"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"docs": docs})
}

// GetDoc returns a single document including its content block JSON. The slug is
// shape-validated first (cheap 400), then looked up; a missing row is a 404.
func (h *DrDocsHandler) GetDoc(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	slug := strings.TrimSpace(c.Param("slug"))
	if !drSlugPattern.MatchString(slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid document slug"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var d models.DrDoc
	// jsonb → []byte returns the raw stored JSON; assigned to Content
	// (json.RawMessage) it passes through to the client unmodified.
	var content []byte
	err := h.pool.QueryRow(ctx, `
SELECT id, slug, title, summary, status, content_format, content, created_at, updated_at
FROM dr_documents
WHERE slug = $1
`, slug).Scan(&d.ID, &d.Slug, &d.Title, &d.Summary, &d.Status, &d.ContentFormat, &content, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	if err != nil {
		log.Printf("dr docs: get query failed for slug %q: %v", slug, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load document"})
		return
	}
	d.Content = content
	c.JSON(http.StatusOK, d)
}
