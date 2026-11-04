package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// Medi (MM104, code name `assistant`, design MEDI_ASSISTANT.md §10):
//
//	GET    /assistant/capabilities              always mounted; {enabled:false} when off
//	POST   /assistant/chat                      SSE over POST (assistant/events.go)
//	GET    /assistant/conversations             ?surface=&projectId= (signed-in, own only)
//	GET    /assistant/conversations/:id         with messages
//	DELETE /assistant/conversations/:id         soft delete + ledger expire now
//	POST   /assistant/messages/:id/feedback     {rating: up|down, reason?}
//
// With ASSISTANT_ENABLED=false or no ASSISTANT_OPENROUTER_API_KEY every route
// but capabilities is 404. With ASSISTANT_OPERATOR_ONLY only the operator tier
// sees enabled:true. Charging (decision 7): the route is a DeferQuota route in
// cmd/api quotaRoutes, and this handler spends after every refusal is decided:
// guide messages, editResult continuations and the operator are never charged;
// an anonymous copilot message is 401 sign_in_required before any charge; a
// copilot message spends one assistant_turns unit, "Plan my edit"
// ASSISTANT_PLAN_TURN_COST units. The engine refunds them when the upstream
// fails before any content reached the user (MM73).

// AssistantHandler serves /api/assistant/*.
type AssistantHandler struct {
	cfg      config.AssistantConfig
	engine   *assistant.Engine
	store    assistant.Store
	registry *tiers.Registry
	expirer  Expirer
	logger   *slog.Logger
	now      func() time.Time
}

// NewAssistantHandler builds the handler. store may be nil (no database: the
// guide still answers, nothing is stored, conversation routes answer 503).
func NewAssistantHandler(cfg config.AssistantConfig, engine *assistant.Engine, store assistant.Store,
	registry *tiers.Registry, logger *slog.Logger) *AssistantHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &AssistantHandler{cfg: cfg, engine: engine, store: store, registry: registry, logger: logger, now: time.Now}
}

// SetDeletion attaches the user-delete sweeper (conversation DELETE).
func (h *AssistantHandler) SetDeletion(e Expirer) {
	if h != nil {
		h.expirer = e
	}
}

// RegisterAssistantRoutes mounts the routes. nil mounts nothing.
func RegisterAssistantRoutes(r gin.IRouter, h *AssistantHandler) {
	if h == nil {
		return
	}
	r.GET("/assistant/capabilities", h.Capabilities)
	r.POST("/assistant/chat", h.Chat)
	r.GET("/assistant/conversations", h.ListConversations)
	r.GET("/assistant/conversations/:id", h.GetConversation)
	r.DELETE("/assistant/conversations/:id", h.DeleteConversation)
	r.POST("/assistant/messages/:id/feedback", h.Feedback)
}

// AssistantChatPath is POST /api/assistant/chat (quotaRoutes, the buckets).
const AssistantChatPath = "/api/assistant/chat"

// assistantCaller is the resolved identity of one request.
type assistantCaller struct {
	tier   tiers.Tier
	limits tiers.Limits
	user   *models.User
}

func (h *AssistantHandler) caller(c *gin.Context) assistantCaller {
	if res, ok := middleware.ResolveTier(c); ok {
		return assistantCaller{tier: res.Tier, limits: res.Limits, user: res.User}
	}
	// Tier system off: everyone is anonymous-shaped, nothing counted.
	return assistantCaller{tier: tiers.TierAnonymous}
}

func (a assistantCaller) userID() string {
	if a.user != nil {
		return a.user.ID
	}
	return ""
}

// live reports whether Medi answers this caller at all.
func (h *AssistantHandler) live(a assistantCaller) bool {
	if !h.cfg.Configured() || h.engine == nil {
		return false
	}
	if h.cfg.OperatorOnly && a.tier != tiers.TierOperator {
		return false
	}
	return true
}

func (h *AssistantHandler) notFound(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
}

// enabledOr404 is the gate of every route but capabilities.
func (h *AssistantHandler) enabledOr404(c *gin.Context) (assistantCaller, bool) {
	a := h.caller(c)
	if !h.live(a) {
		h.notFound(c)
		return a, false
	}
	return a, true
}

type assistantTurns struct {
	Limit     int    `json:"limit"`
	Remaining int    `json:"remaining"`
	ResetsAt  string `json:"resetsAt"`
}

// Capabilities handles GET /assistant/capabilities.
func (h *AssistantHandler) Capabilities(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	a := h.caller(c)
	if !h.live(a) {
		c.JSON(http.StatusOK, gin.H{"enabled": false})
		return
	}
	limit := a.limits.AssistantTurns
	if a.tier == tiers.TierOperator {
		limit = tiers.Unlimited
	}
	remaining := limit
	if limit > 0 {
		remaining = limit - h.usedTurns(c, a)
		if remaining < 0 {
			remaining = 0
		}
	}
	surfaces := make([]string, 0, 3)
	for _, s := range []string{assistant.SurfacePhoto, assistant.SurfaceStudio, assistant.SurfaceVector} {
		if h.cfg.CopilotSurface(s) {
			surfaces = append(surfaces, s)
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled": true,
		"guide":   gin.H{"enabled": h.cfg.GuideEnabled},
		"copilot": gin.H{"enabled": len(surfaces) > 0, "surfaces": surfaces, "signedInOnly": true,
			// Surfaces whose op schema shipped (apply_edits is offered there).
			"editSurfaces": h.editSurfaces()},
		"turns": assistantTurns{Limit: limit, Remaining: remaining,
			ResetsAt: middleware.NextUTCMidnight(h.now()).Format(time.RFC3339)},
		"frames":          true,
		"maxFrameBytes":   h.cfg.MaxFrameBytes,
		"maxMessageChars": assistant.MaxMessageChars,
		"spendCapReached": h.engine.Spend.Reached(c.Request.Context()),
	})
}

func (h *AssistantHandler) editSurfaces() []string {
	out := []string{}
	for _, s := range []string{assistant.SurfacePhoto, assistant.SurfaceStudio, assistant.SurfaceVector} {
		if h.cfg.CopilotSurface(s) && h.engine.Ops.Has(s) {
			out = append(out, s)
		}
	}
	return out
}

// TurnUsageStore reads today's counters (capabilities' remaining turns).
type TurnUsageStore interface {
	UsageForDay(ctx context.Context, subjectKind models.UsageSubjectKind, subjectKey string, day time.Time) (map[string]int64, error)
}

func (h *AssistantHandler) usedTurns(c *gin.Context, a assistantCaller) int {
	us, ok := h.store.(TurnUsageStore)
	if !ok || a.user == nil {
		return 0
	}
	counted, err := us.UsageForDay(c.Request.Context(), models.UsageSubjectUser, a.user.ID, h.now().UTC())
	if err != nil {
		return 0
	}
	return int(counted[models.MetricAssistantTurns])
}

// assistantVisitorSalt salts the guest visitor hash on usage rows.
var assistantVisitorSalt = sha256.Sum256([]byte("mm-api-assistant-visitor-salt"))

func visitorHash(c *gin.Context) string {
	v := strings.TrimSpace(c.GetHeader("X-MM-Visitor-ID"))
	if v == "" {
		v = callerSession(c)
	}
	if v == "" {
		return ""
	}
	s := sha256.New()
	s.Write(assistantVisitorSalt[:])
	s.Write([]byte(v))
	return base64.RawURLEncoding.EncodeToString(s.Sum(nil))[:24]
}

// sseEmitter writes `data: {json}\n\n` records and `: ping` comments.
type sseEmitter struct {
	w       gin.ResponseWriter
	started bool
	start   func()
}

func (s *sseEmitter) begin() {
	if !s.started {
		s.started = true
		s.start()
	}
}

func (s *sseEmitter) Send(ev any) error {
	s.begin()
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	s.w.Flush()
	return nil
}

func (s *sseEmitter) Ping() error {
	s.begin()
	if _, err := io.WriteString(s.w, ": ping\n\n"); err != nil {
		return err
	}
	s.w.Flush()
	return nil
}

// Chat handles POST /assistant/chat.
func (h *AssistantHandler) Chat(c *gin.Context) {
	a, ok := h.enabledOr404(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, assistant.MaxBodyBytes)
	var body assistant.ChatBody
	if err := json.NewDecoder(c.Request.Body).Decode(&body); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "body_too_large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "body is not JSON"})
		return
	}
	frame, frameAfter, err := body.Validate(h.cfg.MaxFrameBytes)
	if err != nil {
		be, _ := assistant.AsBodyError(err)
		status := http.StatusBadRequest
		if be != nil && (be.Code == "frame_too_large" || be.Code == "digest_too_large") {
			status = http.StatusRequestEntityTooLarge
		}
		if be == nil {
			be = &assistant.BodyError{Code: "invalid_body", Message: err.Error()}
		}
		c.JSON(status, gin.H{"error": be.Code, "message": be.Message})
		return
	}
	// Surface gates (the quota gate already refused anonymous copilot turns).
	if body.Surface == assistant.SurfaceGuide && !h.cfg.GuideEnabled {
		h.notFound(c)
		return
	}
	if body.Surface != assistant.SurfaceGuide {
		if !h.cfg.CopilotSurface(body.Surface) {
			h.notFound(c)
			return
		}
		if a.user == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "sign_in_required", "message": "Sign in to use Medi in the editors"})
			return
		}
		// Copilot at the cap: refused before anything streams (no charge kept:
		// refundIfRefused gives the unit back on this 503).
		if h.engine.Spend.Reached(c.Request.Context()) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": assistant.CodePaused,
				"message":  "Medi has reached today's limit and is paused until 00:00 UTC.",
				"resetsAt": middleware.NextUTCMidnight(h.now()).Format(time.RFC3339)})
			return
		}
	}
	turn := &assistant.Turn{Body: body, FrameURL: frame, FrameAfterURL: frameAfter, UserID: a.userID(), VisitorHash: visitorHash(c),
		AnalyticsVisitorID: strings.TrimSpace(c.GetHeader("X-MM-Visitor-ID")),
		AnalyticsSessionID: strings.TrimSpace(c.GetHeader("X-MM-Session-ID"))}
	if body.ConversationID != "" && turn.UserID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "sign_in_required"})
		return
	}
	var prep *assistant.Prepared
	if turn.UserID != "" && h.store != nil {
		ctx := c.Request.Context()
		if body.ConversationID != "" {
			conv, err := h.store.GetAssistantConversation(ctx, body.ConversationID, turn.UserID)
			if errors.Is(err, telemetry.ErrAssistantConversationNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "conversation_not_found"})
				return
			}
			if err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assistant_unavailable"})
				return
			}
			if conv.Surface != body.Surface {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "conversation belongs to another surface"})
				return
			}
			turn.Conversation = conv
		}
		if body.EditResult != nil {
			p, err := h.engine.PrepareEditResult(ctx, turn.Conversation, body.TurnID, body.EditResult.CallID)
			switch {
			case errors.Is(err, assistant.ErrStaleEditResult):
				c.JSON(http.StatusConflict, gin.H{"error": "stale_edit_result"})
				return
			case errors.Is(err, assistant.ErrEditRoundsExceeded):
				c.JSON(http.StatusConflict, gin.H{"error": "edit_rounds_exceeded"})
				return
			case err != nil:
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assistant_unavailable"})
				return
			}
			prep = p
		}
		owner := retentionOwnerFor(c)
		turn.RegisterConversation = func(ctx context.Context, id string) {
			retention.Default().Register(ctx, retention.RegisterInput{
				Kind: retention.KindAssistantConversation, Locator: id, ProjectID: id, Owner: owner})
		}
	} else if body.EditResult != nil {
		// A guest or a database-less deployment cannot continue a stored turn.
		c.JSON(http.StatusConflict, gin.H{"error": "stale_edit_result"})
		return
	}
	// Charge last: everything that can refuse has refused.
	if body.Surface != assistant.SurfaceGuide && body.EditResult == nil {
		n := 1
		if body.Plan {
			n = h.cfg.PlanTurnCost
		}
		// A plan that does not fit is refused before counting: the middleware
		// would count the over-limit unit and the user would lose their last
		// single turns to a plan that never ran.
		if limit := a.limits.AssistantTurns; n > 1 && limit > 0 && a.tier != tiers.TierOperator {
			if used := h.usedTurns(c, a); used+n > limit {
				c.JSON(http.StatusTooManyRequests, gin.H{"error": "quota_exceeded", "tier": string(a.tier),
					"metric": models.MetricAssistantTurns, "limit": limit, "used": used, "needed": n,
					"resetsAt": middleware.NextUTCMidnight(h.now()).Format(time.RFC3339), "upgradePath": "premium"})
				return
			}
		}
		charges, ok := middleware.SpendDeferredQuotaN(c, models.MetricAssistantTurns, n)
		if !ok {
			return // the 429 quota_exceeded is written
		}
		for _, ch := range charges {
			turn.Charges = append(turn.Charges, ch)
		}
	}
	turn.Live = h.liveContext(c, a, body.Surface)

	em := &sseEmitter{w: c.Writer, start: func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)
	}}
	h.engine.Run(c.Request.Context(), turn, em, prep)
}

func (h *AssistantHandler) liveContext(c *gin.Context, a assistantCaller, surface string) assistant.LiveContext {
	lc := assistant.LiveContext{Tier: string(a.tier), TierSummary: h.tierSummary()}
	if surface != assistant.SurfaceGuide {
		lc.TurnsLimit = a.limits.AssistantTurns
		if a.tier == tiers.TierOperator || lc.TurnsLimit < 0 {
			lc.TurnsLimit, lc.TurnsRemaining = -1, -1
		} else {
			// The quota middleware's header carries what is left after this
			// message; without it (quota off, continuation) fall back to the limit.
			lc.TurnsRemaining = lc.TurnsLimit
			if v := c.Writer.Header().Get(middleware.HeaderQuotaRemaining); v != "" {
				_, _ = fmt.Sscan(v, &lc.TurnsRemaining)
			}
		}
	}
	return lc
}

// tierSummary is the live plan line (no prices: MM105 adds them from config).
func (h *AssistantHandler) tierSummary() string {
	if h.registry == nil {
		return ""
	}
	var parts []string
	for _, t := range tiers.All() {
		l := h.registry.For(t)
		parts = append(parts, fmt.Sprintf("%s: %s ops/day, %s AI ops/day, %s Medi edits/day",
			t, countText(l.OpsPerDay), countText(l.AIOpsPerDay), countText(l.AssistantTurns)))
	}
	return strings.Join(parts, "; ") + "."
}

func countText(n int) string {
	if n < 0 {
		return "unlimited"
	}
	return fmt.Sprint(n)
}

// conversationBody is one conversation on the wire.
type conversationBody struct {
	ID          string  `json:"id"`
	Surface     string  `json:"surface"`
	ProjectKind *string `json:"projectKind"`
	ProjectID   *string `json:"projectId"`
	Title       string  `json:"title"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
}

func convBody(cv telemetry.AssistantConversation) conversationBody {
	b := conversationBody{ID: cv.ID, Surface: cv.Surface, Title: cv.Title,
		CreatedAt: cv.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: cv.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	if cv.ProjectKind != "" {
		k := cv.ProjectKind
		b.ProjectKind = &k
	}
	if cv.ProjectID != "" {
		p := cv.ProjectID
		b.ProjectID = &p
	}
	return b
}

// conversationOwner gates the conversation routes: enabled, a database,
// signed in, a uuid id. Strangers get the same 404 a missing id gets.
func (h *AssistantHandler) conversationOwner(c *gin.Context, needID bool) (userID, id string, ok bool) {
	a, ok := h.enabledOr404(c)
	if !ok {
		return "", "", false
	}
	if h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assistant_unavailable"})
		return "", "", false
	}
	if a.user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "sign_in_required"})
		return "", "", false
	}
	if needID {
		id = strings.TrimSpace(c.Param("id"))
		if !assistant.IsUUID(id) {
			h.notFound(c)
			return "", "", false
		}
	}
	return a.user.ID, id, true
}

// ListConversations handles GET /assistant/conversations.
func (h *AssistantHandler) ListConversations(c *gin.Context) {
	userID, _, ok := h.conversationOwner(c, false)
	if !ok {
		return
	}
	surface := strings.TrimSpace(c.Query("surface"))
	if surface != "" && !assistant.ValidSurface(surface) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_surface"})
		return
	}
	list, err := h.store.ListAssistantConversations(c.Request.Context(), userID, surface, clipQuery(c.Query("projectId"), 100), 50)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assistant_unavailable"})
		return
	}
	out := make([]conversationBody, 0, len(list))
	for _, cv := range list {
		out = append(out, convBody(cv))
	}
	c.JSON(http.StatusOK, gin.H{"conversations": out})
}

func clipQuery(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// messageBody is one stored message on the wire (provider state never leaves).
type messageBody struct {
	ID        string          `json:"id"`
	Seq       int             `json:"seq"`
	TurnID    string          `json:"turnId"`
	Role      string          `json:"role"`
	Status    string          `json:"status"`
	Content   json.RawMessage `json:"content"`
	CreatedAt string          `json:"createdAt"`
}

// GetConversation handles GET /assistant/conversations/:id.
func (h *AssistantHandler) GetConversation(c *gin.Context) {
	userID, id, ok := h.conversationOwner(c, true)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	cv, err := h.store.GetAssistantConversation(ctx, id, userID)
	if errors.Is(err, telemetry.ErrAssistantConversationNotFound) {
		h.notFound(c)
		return
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assistant_unavailable"})
		return
	}
	msgs, err := h.store.AssistantConversationMessages(ctx, id)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assistant_unavailable"})
		return
	}
	out := make([]messageBody, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, messageBody{ID: m.ID, Seq: m.Seq, TurnID: m.TurnID, Role: m.Role, Status: m.Status,
			Content: m.Content, CreatedAt: m.CreatedAt.UTC().Format(time.RFC3339Nano)})
	}
	c.JSON(http.StatusOK, gin.H{"conversation": convBody(*cv), "messages": out})
}

// DeleteConversation handles DELETE /assistant/conversations/:id: soft delete,
// then the ledger row expires now and the inline sweep hard-deletes the row.
func (h *AssistantHandler) DeleteConversation(c *gin.Context) {
	userID, id, ok := h.conversationOwner(c, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), deleteTimeout)
	defer cancel()
	if _, err := h.store.GetAssistantConversation(ctx, id, userID); errors.Is(err, telemetry.ErrAssistantConversationNotFound) {
		h.notFound(c)
		return
	} else if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assistant_unavailable"})
		return
	}
	if err := h.store.SoftDeleteAssistantConversation(ctx, id, userID); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Deletion is temporarily unavailable"})
		return
	}
	if h.expirer != nil {
		if _, err := h.expirer.ExpireNow(ctx, ledgerFilterForProject(id)); err != nil && !errors.Is(err, retention.ErrNoExpire) {
			h.logger.Warn("assistant: ledger expire failed", "error", err.Error())
		}
	} else if err := h.store.DeleteAssistantConversationRow(ctx, id); err != nil {
		h.logger.Warn("assistant: hard delete failed", "error", err.Error())
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// Feedback handles POST /assistant/messages/:id/feedback.
func (h *AssistantHandler) Feedback(c *gin.Context) {
	userID, id, ok := h.conversationOwner(c, true)
	if !ok {
		return
	}
	var body struct {
		Rating string `json:"rating"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(c.Request.Body, 4096)).Decode(&body); err != nil ||
		(body.Rating != "up" && body.Rating != "down") || len([]rune(body.Reason)) > 500 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "rating must be up or down; reason <= 500 characters"})
		return
	}
	err := h.store.SetAssistantFeedback(c.Request.Context(), id, userID, body.Rating, strings.TrimSpace(body.Reason))
	if errors.Is(err, telemetry.ErrAssistantMessageNotFound) {
		h.notFound(c)
		return
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "assistant_unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
