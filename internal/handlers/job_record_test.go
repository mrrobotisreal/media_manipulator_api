package handlers

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// jobRecordStore is the slice of the store the tier resolver needs.
type jobRecordStore struct{ user *models.User }

func (s *jobRecordStore) Enabled() bool { return true }

func (s *jobRecordStore) UserByFirebaseUID(_ context.Context, _ string) (*models.User, error) {
	return s.user, nil
}

func (s *jobRecordStore) IncrementUsage(_ context.Context, _ models.UsageSubjectKind, _, _ string, _ time.Time) (int64, error) {
	return 1, nil
}

func (s *jobRecordStore) DecrementUsage(context.Context, models.UsageSubjectKind, string, string, time.Time) (int64, error) {
	return 0, nil
}

// ownerTestContext builds a request context with the tier resolver installed the
// way the /api group installs it.
func ownerTestContext(t *testing.T, user *models.User, signedIn bool, headers map[string]string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/api/upload", nil)
	for k, v := range headers {
		c.Request.Header.Set(k, v)
	}
	if signedIn {
		c.Set(middleware.CtxPrincipal, &middleware.Principal{UID: "firebase-uid"})
	}
	registry := tiers.New(&config.Config{})
	middleware.TierContext(middleware.TierConfig{Store: &jobRecordStore{user: user}, Registry: registry}, true)(c)
	return c
}

func TestJobOwnerForSignedInCallerUsesTheAccountID(t *testing.T) {
	c := ownerTestContext(t,
		&models.User{ID: "11111111-1111-1111-1111-111111111111", FirebaseUID: "firebase-uid", Tier: "premium"},
		true,
		map[string]string{"X-MM-Session-ID": "session-abc", "X-MM-Visitor-ID": "visitor-abc"},
	)

	owner := jobOwnerFor(c)
	if owner.UserID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("user id: got %q", owner.UserID)
	}
	if owner.Tier != string(tiers.TierPremium) {
		t.Errorf("tier: got %q want premium", owner.Tier)
	}
	if owner.SessionID != "session-abc" || owner.VisitorID != "visitor-abc" {
		t.Errorf("correlation headers not carried: %+v", owner)
	}
}

func TestJobOwnerForAnonymousCallerHasNoUserID(t *testing.T) {
	c := ownerTestContext(t, nil, false, map[string]string{"X-MM-Session-ID": "session-abc"})

	owner := jobOwnerFor(c)
	if owner.UserID != "" {
		t.Errorf("an anonymous caller must not get a user id, got %q", owner.UserID)
	}
	if owner.Tier != string(tiers.TierAnonymous) {
		t.Errorf("tier: got %q want anonymous", owner.Tier)
	}
	if owner.SessionID != "session-abc" {
		t.Errorf("session id: got %q", owner.SessionID)
	}
}

func TestJobOwnerForWithoutTierResolutionRecordsNoOwner(t *testing.T) {
	// TIER_QUOTA_ENABLED=false: no resolver is installed, so there is no
	// authenticated account lookup to read. The job is still recorded, without
	// an owner — the same pre-tier behaviour that flag buys everywhere else.
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/api/upload", nil)
	c.Request.Header.Set("X-MM-Session-ID", "session-abc")

	owner := jobOwnerFor(c)
	if owner.UserID != "" || owner.Tier != "" {
		t.Errorf("expected no owner facts without a resolver, got %+v", owner)
	}
	if owner.SessionID != "session-abc" {
		t.Errorf("session id should still be carried, got %q", owner.SessionID)
	}
}

func TestMediaKindStaysInsideTheCheckConstraint(t *testing.T) {
	// mm_conversion_jobs.media_kind allows image|video|audio|unknown only. A
	// document must map to NULL: violating the CHECK would cost the whole row.
	cases := map[models.FileType]string{
		models.FileTypeImage:    "image",
		models.FileTypeVideo:    "video",
		models.FileTypeAudio:    "audio",
		models.FileTypeUnknown:  "unknown",
		models.FileTypeDocument: "",
	}
	for in, want := range cases {
		if got := mediaKindFor(in); got != want {
			t.Errorf("mediaKindFor(%q): got %q want %q", in, got, want)
		}
	}
}

func TestFormatOfKeepsOnlyTheExtension(t *testing.T) {
	cases := map[string]string{
		"holiday_photo.WEBP":       "webp",
		"my.tax.return.2025.pdf":   "pdf",
		"no-extension":             "",
		"":                         "",
		"weird.name with spaces":   "",
		"trailing.":                "",
		"x.averyverylongextension": "",
	}
	for in, want := range cases {
		if got := formatOf(in); got != want {
			t.Errorf("formatOf(%q): got %q want %q", in, got, want)
		}
	}
}

func TestConversionToolForNamesTheTool(t *testing.T) {
	cases := []struct {
		name string
		job  *models.ConversionJob
		want string
	}{
		{"plain conversion", &models.ConversionJob{Options: map[string]any{"format": "webp"}}, "convert"},
		{"transcribe", &models.ConversionJob{Options: map[string]any{"mode": "transcribe"}}, "transcribe"},
		{"specialized", &models.ConversionJob{Options: map[string]any{"mode": "extract_audio"}}, "extract_audio"},
		{"published mode wins", &models.ConversionJob{Mode: "transcode", Options: map[string]any{}}, "transcode"},
		{
			"ai operation",
			&models.ConversionJob{Options: map[string]any{"ai": map[string]any{"enabled": true, "operation": "upscale"}}},
			"ai_upscale",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := conversionToolFor(tc.job); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}
