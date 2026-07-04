package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// DrCommentsHandler serves the Double Raven document commenting endpoints on the
// /api/dr group (so they inherit RequireDoubleRavenAuth). Authorship always
// comes from the verified claims in the gin context (DRContextKey), never from
// the request body. Image attachments use Media Manipulator's standard S3
// handshake — presign -> client PUT -> complete — reusing the shared S3 client
// and bucket/config; there is no second S3 client.
type DrCommentsHandler struct {
	pool      *pgxpool.Pool
	cfg       *config.Config
	s3Client  *s3.Client
	s3Presign *s3.PresignClient
}

func NewDrCommentsHandler(pool *pgxpool.Pool, cfg *config.Config, s3Client *s3.Client) *DrCommentsHandler {
	var presign *s3.PresignClient
	if s3Client != nil {
		presign = s3.NewPresignClient(s3Client)
	}
	return &DrCommentsHandler{pool: pool, cfg: cfg, s3Client: s3Client, s3Presign: presign}
}

// RegisterDrCommentsRoutes wires the comment endpoints onto the already-prefixed
// and already-authed /dr group (see setupRouter).
func RegisterDrCommentsRoutes(r gin.IRouter, h *DrCommentsHandler) {
	r.GET("/docs/:slug/comments", h.ListComments)
	r.POST("/docs/:slug/comments", h.CreateComment)
	r.POST("/comments/:id/attachments", h.CreateCommentAttachment)
	r.POST("/comments/:id/publish", h.PublishComment)
	r.POST("/comments/:id/replies", h.CreateReply)
	r.DELETE("/comments/:id", h.DeleteComment)
	r.POST("/attachments/:id/complete", h.CompleteAttachment)
	r.DELETE("/attachments/:id", h.DeleteAttachment)
	r.POST("/replies/:id/attachments", h.CreateReplyAttachment)
	r.POST("/replies/:id/publish", h.PublishReply)
	r.DELETE("/replies/:id", h.DeleteReply)
}

// ----------------------------------------------------------------------- //
// Pure helpers (unit-tested in dr_comments_test.go)
// ----------------------------------------------------------------------- //

const (
	drMaxAttachmentBytes int64 = 15 << 20 // 15 MB
	drMaxAttachments           = 6
)

var (
	errDrNotAuthor = errors.New("not author")
	errDrNotDraft  = errors.New("not draft")
)

// attachmentExt maps an allowlisted image content type to its file extension.
// Anything else is rejected (ok=false → 400 at the call site).
func attachmentExt(contentType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "image/png":
		return "png", true
	case "image/jpeg":
		return "jpg", true
	case "image/webp":
		return "webp", true
	case "image/gif":
		return "gif", true
	default:
		return "", false
	}
}

// commentAttachmentKey / replyAttachmentKey build the exact S3 keys from the
// Mission spec. imageID is the attachment row's uuid.
func commentAttachmentKey(docID, commentID, imageID, ext string) string {
	return fmt.Sprintf("documents/%s/comments/%s/%s.%s", docID, commentID, imageID, ext)
}

func replyAttachmentKey(docID, commentID, replyID, imageID, ext string) string {
	return fmt.Sprintf("documents/%s/comments/%s/replies/%s/%s.%s", docID, commentID, replyID, imageID, ext)
}

// checkDraftMutation authorizes publish/cancel: author-only, draft-only.
func checkDraftMutation(status, authorUID, callerUID string) error {
	if authorUID != callerUID {
		return errDrNotAuthor
	}
	if status != "draft" {
		return errDrNotDraft
	}
	return nil
}

// validatePublishInput enforces the publish rules: bounded body, no in-flight
// uploads, and a non-empty body OR at least one uploaded attachment.
func validatePublishInput(body string, uploadedCount, pendingCount int) error {
	if len(body) > models.DrCommentMaxBody {
		return errors.New("comment is too long")
	}
	if pendingCount > 0 {
		return errors.New("an attachment is still uploading")
	}
	if strings.TrimSpace(body) == "" && uploadedCount == 0 {
		return errors.New("comment needs text or at least one image")
	}
	return nil
}

// ----------------------------------------------------------------------- //
// Shared handler plumbing
// ----------------------------------------------------------------------- //

func (h *DrCommentsHandler) dbReady(c *gin.Context) bool {
	if h.pool == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Comment storage is unavailable"})
		return false
	}
	return true
}

func (h *DrCommentsHandler) s3Ready(c *gin.Context) bool {
	if h.s3Client == nil || h.s3Presign == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Attachment storage is unavailable"})
		return false
	}
	return true
}

func drCallerClaims(c *gin.Context) (*middleware.DRClaims, bool) {
	v, ok := c.Get(middleware.DRContextKey)
	if !ok {
		return nil, false
	}
	claims, ok := v.(*middleware.DRClaims)
	return claims, ok
}

// abortAuthzError maps the authz sentinels to their HTTP status.
func abortAuthzError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errDrNotAuthor):
		c.JSON(http.StatusForbidden, gin.H{"error": "You can only modify your own drafts"})
	case errors.Is(err, errDrNotDraft):
		c.JSON(http.StatusConflict, gin.H{"error": "Only drafts can be modified"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Request failed"})
	}
}

func (h *DrCommentsHandler) documentIDBySlug(ctx context.Context, slug string) (string, error) {
	var id string
	err := h.pool.QueryRow(ctx, `SELECT id FROM dr_documents WHERE slug = $1`, slug).Scan(&id)
	return id, err
}

type drCommentRow struct {
	id, documentID, authorUID, status string
}

func (h *DrCommentsHandler) loadComment(ctx context.Context, id string) (*drCommentRow, error) {
	var r drCommentRow
	err := h.pool.QueryRow(ctx,
		`SELECT id, document_id, author_uid, status FROM dr_document_comments WHERE id = $1`, id).
		Scan(&r.id, &r.documentID, &r.authorUID, &r.status)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

type drReplyRow struct {
	id, commentID, documentID, authorUID, status string
}

func (h *DrCommentsHandler) loadReply(ctx context.Context, id string) (*drReplyRow, error) {
	var r drReplyRow
	err := h.pool.QueryRow(ctx, `
SELECT rep.id, rep.comment_id, com.document_id, rep.author_uid, rep.status
FROM dr_comment_replies rep
JOIN dr_document_comments com ON com.id = rep.comment_id
WHERE rep.id = $1`, id).Scan(&r.id, &r.commentID, &r.documentID, &r.authorUID, &r.status)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// presignGet returns a presigned GET URL for key. When downloadName is
// non-empty the URL forces a browser download (Content-Disposition: attachment)
// with that filename. Returns "" (best-effort) if presigning fails.
func (h *DrCommentsHandler) presignGet(ctx context.Context, key, downloadName string) string {
	if h.s3Presign == nil || key == "" {
		return ""
	}
	ttl := h.cfg.S3ResultPresignTTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	in := &s3.GetObjectInput{Bucket: aws.String(h.cfg.S3Bucket), Key: aws.String(key)}
	if downloadName != "" {
		in.ResponseContentDisposition = aws.String(fmt.Sprintf(`attachment; filename="%s"`, downloadName))
	}
	out, err := h.s3Presign.PresignGetObject(ctx, in, func(o *s3.PresignOptions) { o.Expires = ttl })
	if err != nil {
		log.Printf("dr comments: presign get %s: %v", key, err)
		return ""
	}
	return out.URL
}

func (h *DrCommentsHandler) deleteObject(ctx context.Context, key string) {
	if h.s3Client == nil || key == "" {
		return
	}
	if _, err := h.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	}); err != nil {
		log.Printf("dr comments: best-effort delete %s failed: %v", key, err)
	}
}

// ----------------------------------------------------------------------- //
// GET /docs/:slug/comments
// ----------------------------------------------------------------------- //

func (h *DrCommentsHandler) ListComments(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	slug := strings.TrimSpace(c.Param("slug"))
	if !drSlugPattern.MatchString(slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid document slug"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	docID, err := h.documentIDBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	if err != nil {
		log.Printf("dr comments: lookup doc %q: %v", slug, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load comments"})
		return
	}

	// Comments (published), ordered by anchor position.
	rows, err := h.pool.Query(ctx, `
SELECT id, author_uid, author_email, anchor, body, created_at, updated_at
FROM dr_document_comments
WHERE document_id = $1 AND status = 'published'
ORDER BY (anchor->>'blockIndex')::int, COALESCE((anchor->>'start')::int, 0), created_at`, docID)
	if err != nil {
		log.Printf("dr comments: list query: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load comments"})
		return
	}
	defer rows.Close()

	comments := make([]models.DrCommentDTO, 0)
	commentIndex := map[string]int{}
	commentIDs := make([]string, 0)
	for rows.Next() {
		var d models.DrCommentDTO
		var anchor []byte
		if err := rows.Scan(&d.ID, &d.AuthorUID, &d.AuthorEmail, &anchor, &d.Body, &d.CreatedAt, &d.UpdatedAt); err != nil {
			log.Printf("dr comments: scan comment: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load comments"})
			return
		}
		d.Anchor = anchor
		d.Attachments = []models.DrAttachmentDTO{}
		d.Replies = []models.DrReplyDTO{}
		commentIndex[d.ID] = len(comments)
		commentIDs = append(commentIDs, d.ID)
		comments = append(comments, d)
	}
	if err := rows.Err(); err != nil {
		log.Printf("dr comments: list rows: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load comments"})
		return
	}

	if len(commentIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{"comments": comments})
		return
	}

	// Comment attachments (uploaded).
	if err := h.attachComments(ctx, commentIDs, comments, commentIndex); err != nil {
		log.Printf("dr comments: load comment attachments: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load comments"})
		return
	}

	// Replies (published) + their attachments.
	if err := h.attachReplies(ctx, commentIDs, comments, commentIndex); err != nil {
		log.Printf("dr comments: load replies: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load comments"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"comments": comments})
}

// attachComments loads uploaded attachments for the given comments and fills
// each comment's Attachments slice with presigned view/download URLs.
func (h *DrCommentsHandler) attachComments(ctx context.Context, commentIDs []string, comments []models.DrCommentDTO, idx map[string]int) error {
	rows, err := h.pool.Query(ctx, `
SELECT id, comment_id, content_type, size_bytes, width, height, s3_key
FROM dr_comment_attachments
WHERE comment_id = ANY($1) AND status = 'uploaded'
ORDER BY created_at`, commentIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var commentID string
		att, key, err := scanAttachment(rows, &commentID)
		if err != nil {
			return err
		}
		att.ViewURL = h.presignGet(ctx, key, "")
		att.DownloadURL = h.presignGet(ctx, key, path.Base(key))
		if i, ok := idx[commentID]; ok {
			comments[i].Attachments = append(comments[i].Attachments, att)
		}
	}
	return rows.Err()
}

func (h *DrCommentsHandler) attachReplies(ctx context.Context, commentIDs []string, comments []models.DrCommentDTO, idx map[string]int) error {
	rows, err := h.pool.Query(ctx, `
SELECT id, comment_id, author_uid, author_email, body, created_at, updated_at
FROM dr_comment_replies
WHERE comment_id = ANY($1) AND status = 'published'
ORDER BY created_at`, commentIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	replyIndex := map[string][2]int{} // replyID -> {commentIdx, replyIdx}
	replyIDs := make([]string, 0)
	for rows.Next() {
		var commentID string
		var r models.DrReplyDTO
		if err := rows.Scan(&r.ID, &commentID, &r.AuthorUID, &r.AuthorEmail, &r.Body, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return err
		}
		r.Attachments = []models.DrAttachmentDTO{}
		ci, ok := idx[commentID]
		if !ok {
			continue
		}
		comments[ci].Replies = append(comments[ci].Replies, r)
		comments[ci].ReplyCount++
		replyIndex[r.ID] = [2]int{ci, len(comments[ci].Replies) - 1}
		replyIDs = append(replyIDs, r.ID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(replyIDs) == 0 {
		return nil
	}

	arows, err := h.pool.Query(ctx, `
SELECT id, reply_id, content_type, size_bytes, width, height, s3_key
FROM dr_comment_attachments
WHERE reply_id = ANY($1) AND status = 'uploaded'
ORDER BY created_at`, replyIDs)
	if err != nil {
		return err
	}
	defer arows.Close()
	for arows.Next() {
		var replyID string
		att, key, err := scanAttachment(arows, &replyID)
		if err != nil {
			return err
		}
		att.ViewURL = h.presignGet(ctx, key, "")
		att.DownloadURL = h.presignGet(ctx, key, path.Base(key))
		if loc, ok := replyIndex[replyID]; ok {
			comments[loc[0]].Replies[loc[1]].Attachments = append(comments[loc[0]].Replies[loc[1]].Attachments, att)
		}
	}
	return arows.Err()
}

// scanAttachment scans the shared attachment columns; parentID receives the
// comment_id or reply_id selected as the second column. key returns the raw
// s3_key (not serialized — used only to presign).
func scanAttachment(rows pgx.Rows, parentID *string) (models.DrAttachmentDTO, string, error) {
	var att models.DrAttachmentDTO
	var key string
	err := rows.Scan(&att.ID, parentID, &att.ContentType, &att.SizeBytes, &att.Width, &att.Height, &key)
	return att, key, err
}

// ----------------------------------------------------------------------- //
// POST /docs/:slug/comments  (create draft)
// ----------------------------------------------------------------------- //

func (h *DrCommentsHandler) CreateComment(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	slug := strings.TrimSpace(c.Param("slug"))
	if !drSlugPattern.MatchString(slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid document slug"})
		return
	}
	var req models.DrCreateCommentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if err := req.Anchor.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	anchorJSON, err := json.Marshal(req.Anchor)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create comment"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	docID, err := h.documentIDBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Document not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create comment"})
		return
	}

	var commentID string
	err = h.pool.QueryRow(ctx, `
INSERT INTO dr_document_comments (document_id, author_uid, author_email, anchor, status)
VALUES ($1, $2, $3, $4, 'draft')
RETURNING id`, docID, claims.UID, claims.Email, anchorJSON).Scan(&commentID)
	if err != nil {
		log.Printf("dr comments: insert comment: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create comment"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"commentId": commentID})
}

// ----------------------------------------------------------------------- //
// Attachments (presign) — comment + reply
// ----------------------------------------------------------------------- //

func (h *DrCommentsHandler) CreateCommentAttachment(c *gin.Context) {
	h.createAttachment(c, false)
}

func (h *DrCommentsHandler) CreateReplyAttachment(c *gin.Context) {
	h.createAttachment(c, true)
}

// createAttachment handles both comment and reply attachment presigning. The
// key layout differs (reply keys nest under .../replies/<reply-id>/) but the
// validation, ownership, count-limit and presign flow are identical.
func (h *DrCommentsHandler) createAttachment(c *gin.Context, isReply bool) {
	if !h.dbReady(c) || !h.s3Ready(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	var req models.DrPresignAttachmentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	ext, okExt := attachmentExt(req.ContentType)
	if !okExt {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unsupported image type"})
		return
	}
	if req.SizeBytes <= 0 || req.SizeBytes > drMaxAttachmentBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Image must be between 1 byte and 15 MB"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	parentID := strings.TrimSpace(c.Param("id"))
	attachmentID := uuid.NewString()

	var key string
	var parentCol string // "comment_id" or "reply_id"
	if isReply {
		reply, err := h.loadReply(ctx, parentID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Reply not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
			return
		}
		if err := checkAuthorOnly(reply.authorUID, claims.UID); err != nil {
			abortAuthzError(c, err)
			return
		}
		key = replyAttachmentKey(reply.documentID, reply.commentID, reply.id, attachmentID, ext)
		parentCol = "reply_id"
	} else {
		comment, err := h.loadComment(ctx, parentID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Comment not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
			return
		}
		if err := checkAuthorOnly(comment.authorUID, claims.UID); err != nil {
			abortAuthzError(c, err)
			return
		}
		key = commentAttachmentKey(comment.documentID, comment.id, attachmentID, ext)
		parentCol = "comment_id"
	}

	// Enforce the per-parent attachment cap.
	var count int
	countSQL := fmt.Sprintf(`SELECT count(*) FROM dr_comment_attachments WHERE %s = $1`, parentCol)
	if err := h.pool.QueryRow(ctx, countSQL, parentID).Scan(&count); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}
	if count >= drMaxAttachments {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("At most %d images are allowed", drMaxAttachments)})
		return
	}

	insertSQL := fmt.Sprintf(`
INSERT INTO dr_comment_attachments (id, %s, author_uid, s3_key, content_type, size_bytes, width, height, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending')`, parentCol)
	if _, err := h.pool.Exec(ctx, insertSQL, attachmentID, parentID, claims.UID, key, req.ContentType, req.SizeBytes, req.Width, req.Height); err != nil {
		log.Printf("dr comments: insert attachment: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to prepare upload"})
		return
	}

	presignCtx, pcancel := context.WithTimeout(ctx, 5*time.Second)
	defer pcancel()
	out, err := h.s3Presign.PresignPutObject(presignCtx, &s3.PutObjectInput{
		Bucket:      aws.String(h.cfg.S3Bucket),
		Key:         aws.String(key),
		ContentType: aws.String(req.ContentType),
	}, func(o *s3.PresignOptions) { o.Expires = h.cfg.S3PresignTTL })
	if err != nil {
		log.Printf("dr comments: presign put: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create upload URL"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"attachmentId": attachmentID, "s3Key": key, "uploadUrl": out.URL})
}

// checkAuthorOnly authorizes an attachment action: the caller must own the
// parent comment/reply. (Attachments are added while composing your own draft.)
func checkAuthorOnly(authorUID, callerUID string) error {
	if authorUID != callerUID {
		return errDrNotAuthor
	}
	return nil
}

// ----------------------------------------------------------------------- //
// POST /attachments/:id/complete
// ----------------------------------------------------------------------- //

func (h *DrCommentsHandler) CompleteAttachment(c *gin.Context) {
	if !h.dbReady(c) || !h.s3Ready(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	id := strings.TrimSpace(c.Param("id"))

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	var (
		key, contentType, authorUID, status string
		declaredSize                        int64
	)
	err := h.pool.QueryRow(ctx, `
SELECT s3_key, content_type, size_bytes, author_uid, status
FROM dr_comment_attachments WHERE id = $1`, id).Scan(&key, &contentType, &declaredSize, &authorUID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to confirm upload"})
		return
	}
	if err := checkAuthorOnly(authorUID, claims.UID); err != nil {
		abortAuthzError(c, err)
		return
	}

	head, err := h.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(h.cfg.S3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("dr comments: head attachment %s: %v", key, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded image was not found"})
		return
	}
	objectSize := aws.ToInt64(head.ContentLength)
	if objectSize <= 0 || objectSize > drMaxAttachmentBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded image is empty or too large"})
		return
	}
	// Size within a small tolerance of the declared size (S3 may not know it
	// exactly for some clients, but a gross mismatch is rejected).
	if declaredSize > 0 && absInt64(objectSize-declaredSize) > 1024 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded image size does not match"})
		return
	}
	if ct := aws.ToString(head.ContentType); ct != "" && !strings.EqualFold(ct, contentType) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Uploaded image type does not match"})
		return
	}

	if _, err := h.pool.Exec(ctx, `UPDATE dr_comment_attachments SET status = 'uploaded' WHERE id = $1`, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to confirm upload"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// DeleteAttachment removes a single attachment (row + best-effort S3 object).
// Author-only. Used by the composer's per-thumbnail remove ✕ so a completed
// upload the user changed their mind about doesn't survive to publish.
func (h *DrCommentsHandler) DeleteAttachment(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	var key, authorUID string
	err := h.pool.QueryRow(ctx, `SELECT s3_key, author_uid FROM dr_comment_attachments WHERE id = $1`, id).Scan(&key, &authorUID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete"})
		return
	}
	if err := checkAuthorOnly(authorUID, claims.UID); err != nil {
		abortAuthzError(c, err)
		return
	}
	h.deleteObject(ctx, key)
	if _, err := h.pool.Exec(ctx, `DELETE FROM dr_comment_attachments WHERE id = $1`, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// ----------------------------------------------------------------------- //
// Publish — comment + reply
// ----------------------------------------------------------------------- //

func (h *DrCommentsHandler) PublishComment(c *gin.Context) {
	h.publish(c, false)
}

func (h *DrCommentsHandler) PublishReply(c *gin.Context) {
	h.publish(c, true)
}

func (h *DrCommentsHandler) publish(c *gin.Context, isReply bool) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	var req models.DrPublishRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	id := strings.TrimSpace(c.Param("id"))

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// Authorize (author + draft) and count attachments in one load.
	var authorUID, status, parentCol, table string
	if isReply {
		table, parentCol = "dr_comment_replies", "reply_id"
		r, err := h.loadReply(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Reply not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish"})
			return
		}
		authorUID, status = r.authorUID, r.status
	} else {
		table, parentCol = "dr_document_comments", "comment_id"
		com, err := h.loadComment(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Comment not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish"})
			return
		}
		authorUID, status = com.authorUID, com.status
	}
	if err := checkDraftMutation(status, authorUID, claims.UID); err != nil {
		abortAuthzError(c, err)
		return
	}

	uploaded, pending, err := h.countAttachments(ctx, parentCol, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish"})
		return
	}
	if err := validatePublishInput(req.Body, uploaded, pending); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	tag, err := h.pool.Exec(ctx, fmt.Sprintf(`
UPDATE %s SET body = $2, status = 'published', updated_at = now()
WHERE id = $1 AND status = 'draft' AND author_uid = $3`, table), id, req.Body, claims.UID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to publish"})
		return
	}
	if tag.RowsAffected() == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "Draft could not be published"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *DrCommentsHandler) countAttachments(ctx context.Context, parentCol, parentID string) (uploaded, pending int, err error) {
	sql := fmt.Sprintf(`
SELECT
  count(*) FILTER (WHERE status = 'uploaded'),
  count(*) FILTER (WHERE status = 'pending')
FROM dr_comment_attachments WHERE %s = $1`, parentCol)
	err = h.pool.QueryRow(ctx, sql, parentID).Scan(&uploaded, &pending)
	return uploaded, pending, err
}

// ----------------------------------------------------------------------- //
// POST /comments/:id/replies  (create draft reply on a published comment)
// ----------------------------------------------------------------------- //

func (h *DrCommentsHandler) CreateReply(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	commentID := strings.TrimSpace(c.Param("id"))

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	com, err := h.loadComment(ctx, commentID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Comment not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create reply"})
		return
	}
	if com.status != "published" {
		c.JSON(http.StatusConflict, gin.H{"error": "Can only reply to a published comment"})
		return
	}

	var replyID string
	err = h.pool.QueryRow(ctx, `
INSERT INTO dr_comment_replies (comment_id, author_uid, author_email, status)
VALUES ($1, $2, $3, 'draft')
RETURNING id`, commentID, claims.UID, claims.Email).Scan(&replyID)
	if err != nil {
		log.Printf("dr comments: insert reply: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create reply"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"replyId": replyID})
}

// ----------------------------------------------------------------------- //
// DELETE — comment + reply (author-only, draft-only; Cancel in the composer)
// ----------------------------------------------------------------------- //

func (h *DrCommentsHandler) DeleteComment(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	com, err := h.loadComment(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Comment not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete"})
		return
	}
	if err := checkDraftMutation(com.status, com.authorUID, claims.UID); err != nil {
		abortAuthzError(c, err)
		return
	}

	// Best-effort S3 delete for any attachments (of the comment or its replies)
	// before the cascade removes their rows.
	for _, key := range h.attachmentKeysForComment(ctx, id) {
		h.deleteObject(ctx, key)
	}
	if _, err := h.pool.Exec(ctx, `DELETE FROM dr_document_comments WHERE id = $1`, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *DrCommentsHandler) DeleteReply(c *gin.Context) {
	if !h.dbReady(c) {
		return
	}
	claims, ok := drCallerClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	r, err := h.loadReply(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Reply not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete"})
		return
	}
	if err := checkDraftMutation(r.status, r.authorUID, claims.UID); err != nil {
		abortAuthzError(c, err)
		return
	}
	for _, key := range h.attachmentKeysForReply(ctx, id) {
		h.deleteObject(ctx, key)
	}
	if _, err := h.pool.Exec(ctx, `DELETE FROM dr_comment_replies WHERE id = $1`, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *DrCommentsHandler) attachmentKeysForComment(ctx context.Context, commentID string) []string {
	rows, err := h.pool.Query(ctx, `
SELECT s3_key FROM dr_comment_attachments WHERE comment_id = $1
UNION ALL
SELECT a.s3_key FROM dr_comment_attachments a
JOIN dr_comment_replies r ON r.id = a.reply_id
WHERE r.comment_id = $1`, commentID)
	if err != nil {
		log.Printf("dr comments: gather keys for comment %s: %v", commentID, err)
		return nil
	}
	defer rows.Close()
	return collectKeys(rows)
}

func (h *DrCommentsHandler) attachmentKeysForReply(ctx context.Context, replyID string) []string {
	rows, err := h.pool.Query(ctx, `SELECT s3_key FROM dr_comment_attachments WHERE reply_id = $1`, replyID)
	if err != nil {
		log.Printf("dr comments: gather keys for reply %s: %v", replyID, err)
		return nil
	}
	defer rows.Close()
	return collectKeys(rows)
}

func collectKeys(rows pgx.Rows) []string {
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err == nil {
			keys = append(keys, k)
		}
	}
	return keys
}

// ----------------------------------------------------------------------- //
// Draft reaper (daily) — abandoned drafts + pending attachments > 24h
// ----------------------------------------------------------------------- //

// ReapStaleDrafts deletes draft comments/replies and pending attachments older
// than 24h, best-effort removing their S3 objects first. Called on a daily
// ticker from cmd/api. Safe to run repeatedly; errors are logged, not fatal.
func (h *DrCommentsHandler) ReapStaleDrafts(ctx context.Context) {
	if h.pool == nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)

	// 1) Stale draft comments (cascades to their replies + attachment rows).
	staleComments := h.selectIDs(ctx, `SELECT id FROM dr_document_comments WHERE status = 'draft' AND created_at < $1`, cutoff)
	for _, id := range staleComments {
		for _, key := range h.attachmentKeysForComment(ctx, id) {
			h.deleteObject(ctx, key)
		}
	}
	if len(staleComments) > 0 {
		if _, err := h.pool.Exec(ctx, `DELETE FROM dr_document_comments WHERE id = ANY($1)`, staleComments); err != nil {
			log.Printf("dr comments reaper: delete stale comments: %v", err)
		}
	}

	// 2) Stale draft replies whose parent survived (drafts on published comments).
	staleReplies := h.selectIDs(ctx, `SELECT id FROM dr_comment_replies WHERE status = 'draft' AND created_at < $1`, cutoff)
	for _, id := range staleReplies {
		for _, key := range h.attachmentKeysForReply(ctx, id) {
			h.deleteObject(ctx, key)
		}
	}
	if len(staleReplies) > 0 {
		if _, err := h.pool.Exec(ctx, `DELETE FROM dr_comment_replies WHERE id = ANY($1)`, staleReplies); err != nil {
			log.Printf("dr comments reaper: delete stale replies: %v", err)
		}
	}

	// 3) Orphaned pending attachments (never completed).
	rows, err := h.pool.Query(ctx, `SELECT id, s3_key FROM dr_comment_attachments WHERE status = 'pending' AND created_at < $1`, cutoff)
	if err != nil {
		log.Printf("dr comments reaper: select pending attachments: %v", err)
		return
	}
	var pendingIDs []string
	func() {
		defer rows.Close()
		for rows.Next() {
			var id, key string
			if err := rows.Scan(&id, &key); err != nil {
				continue
			}
			h.deleteObject(ctx, key)
			pendingIDs = append(pendingIDs, id)
		}
	}()
	if len(pendingIDs) > 0 {
		if _, err := h.pool.Exec(ctx, `DELETE FROM dr_comment_attachments WHERE id = ANY($1)`, pendingIDs); err != nil {
			log.Printf("dr comments reaper: delete pending attachments: %v", err)
		}
	}
	if len(staleComments)+len(staleReplies)+len(pendingIDs) > 0 {
		log.Printf("dr comments reaper: removed %d draft comments, %d draft replies, %d pending attachments",
			len(staleComments), len(staleReplies), len(pendingIDs))
	}
}

func (h *DrCommentsHandler) selectIDs(ctx context.Context, sql string, args ...any) []string {
	rows, err := h.pool.Query(ctx, sql, args...)
	if err != nil {
		log.Printf("dr comments reaper: query ids: %v", err)
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}
