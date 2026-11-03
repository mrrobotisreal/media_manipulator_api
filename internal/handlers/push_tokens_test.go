package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

type fakePushStore struct {
	disabled bool
	users    map[string]*models.User
	rows     map[string]string // token -> owner user id
	last     struct{ platform, version, locale string }
}

func (f *fakePushStore) Enabled() bool { return !f.disabled }
func (f *fakePushStore) UserByFirebaseUID(_ context.Context, uid string) (*models.User, error) {
	return f.users[uid], nil
}
func (f *fakePushStore) UpsertPushToken(_ context.Context, userID, token, platform, appVersion, locale string, _ time.Time) error {
	f.rows[token] = userID
	f.last.platform, f.last.version, f.last.locale = platform, appVersion, locale
	return nil
}
func (f *fakePushStore) DeletePushToken(_ context.Context, userID, token string) (bool, error) {
	if f.rows[token] != userID {
		return false, nil
	}
	delete(f.rows, token)
	return true, nil
}

func pushRouter(f *fakePushStore) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPushTokenHandler(f, nil)
	authed := r.Group("/api/account")
	authed.Use(middleware.RequireFirebaseAuth(true, settingsVerifier{}))
	authed.POST("/push-tokens", h.Register)
	authed.DELETE("/push-tokens", h.Unregister)
	return r
}

func newPushStore() *fakePushStore {
	return &fakePushStore{
		users: map[string]*models.User{"alice": {ID: "ua"}, "bob": {ID: "ub"}},
		rows:  map[string]string{},
	}
}

func TestPostPushTokenValidation(t *testing.T) {
	f := newPushStore()
	r := pushRouter(f)
	cases := []struct {
		name, user, body string
		code             int
		errKey           string
	}{
		{"invalid platform", "alice", `{"token":"t","platform":"web"}`, 400, "invalid_platform"},
		{"missing platform", "alice", `{"token":"t"}`, 400, "invalid_platform"},
		{"blank token", "alice", `{"token":"   ","platform":"ios"}`, 400, "invalid_token"},
		{"huge token", "alice", `{"token":"` + strings.Repeat("x", 4097) + `","platform":"ios"}`, 400, "invalid_token"},
		{"long version", "alice", `{"token":"t","platform":"ios","appVersion":"` + strings.Repeat("1", 65) + `"}`, 400, "invalid_app_version"},
		{"not synced", "carol", `{"token":"t","platform":"android"}`, 409, "account_not_synced"},
		{"ok", "alice", `{"token":"tok1","platform":"android","appVersion":"1.0.0","locale":"pt_BR"}`, 204, ""},
	}
	for _, tc := range cases {
		w := doReq(r, "POST", "/api/account/push-tokens", tc.user, tc.body)
		if w.Code != tc.code {
			t.Errorf("%s: %d, want %d (%s)", tc.name, w.Code, tc.code, w.Body)
		}
		if tc.errKey != "" && !strings.Contains(w.Body.String(), tc.errKey) {
			t.Errorf("%s: body %s lacks %s", tc.name, w.Body, tc.errKey)
		}
	}
	if f.rows["tok1"] != "ua" || f.last.locale != "pt-br" {
		t.Errorf("stored %v locale %q (want normalised pt-br)", f.rows, f.last.locale)
	}
	f.disabled = true
	if w := doReq(r, "POST", "/api/account/push-tokens", "alice", `{"token":"t","platform":"ios"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("store disabled = %d, want 503", w.Code)
	}
	if w := doReq(r, "POST", "/api/account/push-tokens", "", `{"token":"t","platform":"ios"}`); w.Code != 401 {
		t.Errorf("unauthenticated = %d, want 401", w.Code)
	}
}

func TestDeletePushTokenIdempotent(t *testing.T) {
	f := newPushStore()
	f.rows["mine"] = "ua"
	f.rows["theirs"] = "ub"
	r := pushRouter(f)
	del := func(user, tok string) *httptest.ResponseRecorder {
		return doReq(r, "DELETE", "/api/account/push-tokens", user, `{"token":"`+tok+`"}`)
	}
	if w := del("alice", "mine"); w.Code != 204 {
		t.Errorf("first delete = %d", w.Code)
	}
	if w := del("alice", "mine"); w.Code != 204 {
		t.Errorf("repeat delete = %d, want 204", w.Code)
	}
	if w := del("alice", "theirs"); w.Code != 204 {
		t.Errorf("foreign delete = %d, want 204 (no leak)", w.Code)
	}
	if f.rows["theirs"] != "ub" {
		t.Error("alice deleted bob's token")
	}
	if w := doReq(r, "DELETE", "/api/account/push-tokens", "alice", `{"token":""}`); w.Code != 400 {
		t.Errorf("blank = %d, want 400", w.Code)
	}
}
