package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/middleware"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

type settingsVerifier struct{}

func (settingsVerifier) Verify(_ context.Context, token string) (*middleware.Principal, error) {
	if token == "" || token == "bad" {
		return nil, context.Canceled
	}
	return &middleware.Principal{UID: token}, nil
}

// fakeSettingsStore models just enough of the ledger to prove scoping: rows
// belong to a user, and re-stamp / preview only look at the caller's live rows.
type fakeSettingsStore struct {
	users    map[string]*models.User // by firebase uid
	settings map[string]*telemetry.UserSettings
	objects  []fakeObj
	applied  []telemetry.SettingsPatch
	writes   int
}

type fakeObj struct {
	owner   string
	kind    string
	created time.Time
	expires time.Time
	deleted bool
	size    int64
}

func (f *fakeSettingsStore) Enabled() bool { return true }
func (f *fakeSettingsStore) UserByFirebaseUID(_ context.Context, uid string) (*models.User, error) {
	return f.users[uid], nil
}
func (f *fakeSettingsStore) GetOrCreateUserSettings(_ context.Context, id string, def int) (*telemetry.UserSettings, error) {
	if s, ok := f.settings[id]; ok {
		return s, nil
	}
	f.writes++
	s := &telemetry.UserSettings{UserID: id, RetentionHours: def, RetentionScope: "all", HistoryVisibility: "full", UpdatedAt: time.Now()}
	f.settings[id] = s
	return s, nil
}
func (f *fakeSettingsStore) ApplyUserSettings(ctx context.Context, id string, def int, p telemetry.SettingsPatch, now time.Time) (*telemetry.UserSettings, int64, error) {
	s, _ := f.GetOrCreateUserSettings(ctx, id, def)
	f.writes++
	f.applied = append(f.applied, p)
	var n int64
	if p.RetentionHours != nil {
		s.RetentionHours = *p.RetentionHours
		for i := range f.objects {
			o := &f.objects[i]
			if o.owner != id || o.deleted {
				continue
			}
			exp := o.created.Add(time.Duration(s.RetentionHours) * time.Hour)
			if floor := now.Add(time.Hour); exp.Before(floor) {
				exp = floor
			}
			o.expires = exp
			n++
		}
	}
	if p.HistoryVisibility != nil {
		s.HistoryVisibility = *p.HistoryVisibility
	}
	if p.DeleteMetadataReports != nil {
		s.DeleteMetadataReports = *p.DeleteMetadataReports
	}
	return s, n, nil
}
func (f *fakeSettingsStore) PreviewRetentionShrink(_ context.Context, id string, hours int, now time.Time) (telemetry.RetentionPreview, error) {
	var out telemetry.RetentionPreview
	for _, o := range f.objects {
		if o.owner != id || o.deleted || !o.created.Add(time.Duration(hours)*time.Hour).Before(now) {
			continue
		}
		switch o.kind {
		case "output_dir":
			out.Results++
		case "studio_project":
			out.Projects++
		default:
			out.Other++
		}
		out.Bytes += o.size
	}
	return out, nil
}

func settingsRouter(store *fakeSettingsStore, now time.Time) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewSettingsHandler(store, nil)
	h.now = func() time.Time { return now }
	authed := r.Group("/api/account")
	authed.Use(middleware.RequireFirebaseAuth(true, settingsVerifier{}))
	authed.GET("/settings", h.Get)
	authed.PATCH("/settings", h.Patch)
	authed.GET("/settings/preview", h.Preview)
	return r
}

func newSettingsStore(now time.Time) *fakeSettingsStore {
	return &fakeSettingsStore{
		users:    map[string]*models.User{"alice": {ID: "ua", Tier: "free"}, "bob": {ID: "ub", Tier: "premium"}},
		settings: map[string]*telemetry.UserSettings{},
		objects: []fakeObj{
			{owner: "ua", kind: "output_dir", created: now.Add(-10 * 24 * time.Hour), expires: now.Add(100 * time.Hour), size: 100},
			{owner: "ua", kind: "studio_project", created: now.Add(-40 * 24 * time.Hour), size: 50},
			{owner: "ua", kind: "s3_object", created: now.Add(-1 * time.Hour), size: 7},
			{owner: "ua", kind: "output_dir", created: now.Add(-400 * 24 * time.Hour), deleted: true, size: 999},
			{owner: "ub", kind: "output_dir", created: now.Add(-10 * 24 * time.Hour), expires: now.Add(7 * time.Hour), size: 5},
		},
	}
}

func doReq(r *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestSettingsGuestGets401(t *testing.T) {
	now := time.Now().UTC()
	r := settingsRouter(newSettingsStore(now), now)
	for _, tc := range [][2]string{
		{"GET", "/api/account/settings"},
		{"PATCH", "/api/account/settings"},
		{"GET", "/api/account/settings/preview?retentionHours=48"},
	} {
		if w := doReq(r, tc[0], tc[1], "", `{"retentionHours":48}`); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s guest = %d, want 401", tc[0], tc[1], w.Code)
		}
	}
}

func TestSettingsGetCreatesDefaultRowLazily(t *testing.T) {
	now := time.Now().UTC()
	st := newSettingsStore(now)
	r := settingsRouter(st, now)
	w := doReq(r, "GET", "/api/account/settings", "alice", "")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["retentionHours"].(float64) != 4320 || got["retentionScope"] != "all" ||
		got["deleteMetadataReports"] != false || got["historyVisibility"] != "full" {
		t.Errorf("defaults wrong: %v", got)
	}
	lim := got["limits"].(map[string]any)
	if lim["minHours"].(float64) != 24 || lim["maxHours"].(float64) != 17520 || lim["defaultHours"].(float64) != 4320 {
		t.Errorf("limits wrong: %v", lim)
	}
	if st.settings["ua"] == nil {
		t.Error("row not created")
	}
}

func TestSettingsPatchValidation(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name, body, field string
		want              int
	}{
		{"23 too low", `{"retentionHours":23}`, "retentionHours", 400},
		{"17521 too high", `{"retentionHours":17521}`, "retentionHours", 400},
		{"zero", `{"retentionHours":0}`, "retentionHours", 400},
		{"negative", `{"retentionHours":-1}`, "retentionHours", 400},
		{"bad visibility", `{"historyVisibility":"nope"}`, "historyVisibility", 400},
		{"bad scope", `{"retentionScope":"results"}`, "retentionScope", 400},
		{"empty body", `{}`, "", 400},
		{"not json", `x`, "", 400},
		{"string hours", `{"retentionHours":"48"}`, "", 400},
		{"24 ok", `{"retentionHours":24}`, "", 200},
		{"17520 ok", `{"retentionHours":17520}`, "", 200},
		{"names_only ok", `{"historyVisibility":"names_only"}`, "", 200},
		{"scope all ok", `{"retentionScope":"all"}`, "", 200},
		{"bool only ok", `{"deleteMetadataReports":true}`, "", 200},
	}
	for _, tc := range cases {
		st := newSettingsStore(now)
		w := doReq(settingsRouter(st, now), "PATCH", "/api/account/settings", "alice", tc.body)
		if w.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, w.Code, tc.want, w.Body)
			continue
		}
		if tc.want == 400 {
			var e settingsError
			_ = json.Unmarshal(w.Body.Bytes(), &e)
			if e.Field != tc.field || e.Error == "" {
				t.Errorf("%s: error body %+v, want field %q", tc.name, e, tc.field)
			}
			if st.writes != 0 {
				t.Errorf("%s: invalid patch wrote", tc.name)
			}
		}
	}
}

func TestSettingsPatchRestampsOnlyCallersLiveRows(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	st := newSettingsStore(now)
	bobBefore := st.objects[4].expires
	w := doReq(settingsRouter(st, now), "PATCH", "/api/account/settings", "alice", `{"retentionHours":24}`)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp settingsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.RetentionHours != 24 || resp.Restamped == nil || *resp.Restamped != 3 {
		t.Errorf("resp = %+v restamped=%v", resp, resp.Restamped)
	}
	floor := now.Add(time.Hour)
	if !st.objects[0].expires.Equal(floor) || !st.objects[1].expires.Equal(floor) {
		t.Errorf("old rows should floor to now+1h: %v %v", st.objects[0].expires, st.objects[1].expires)
	}
	if want := st.objects[2].created.Add(24 * time.Hour); !st.objects[2].expires.Equal(want) {
		t.Errorf("recent row = %v, want %v", st.objects[2].expires, want)
	}
	if !st.objects[3].expires.IsZero() {
		t.Error("deleted row must not be re-stamped")
	}
	if !st.objects[4].expires.Equal(bobBefore) {
		t.Error("another user's row was re-stamped")
	}
}

func TestSettingsPreviewAndDryRunWriteNothing(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	st := newSettingsStore(now)
	st.settings["ua"] = &telemetry.UserSettings{UserID: "ua", RetentionHours: 4320, RetentionScope: "all", HistoryVisibility: "full"}
	r := settingsRouter(st, now)

	w := doReq(r, "GET", "/api/account/settings/preview?retentionHours=24", "alice", "")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var p previewResponse
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.WouldDelete.Results != 1 || p.WouldDelete.Projects != 1 || p.WouldDelete.Other != 0 || p.WouldDelete.Bytes != 150 {
		t.Errorf("preview = %+v", p.WouldDelete)
	}
	if p.EarliestDeletionAt == nil || !p.EarliestDeletionAt.Equal(now.Add(time.Hour)) {
		t.Errorf("earliestDeletionAt = %v", p.EarliestDeletionAt)
	}

	w = doReq(r, "PATCH", "/api/account/settings?dryRun=1", "alice", `{"retentionHours":24}`)
	if w.Code != 200 {
		t.Fatalf("dryRun status %d: %s", w.Code, w.Body)
	}
	var d previewResponse
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if d.WouldDelete != p.WouldDelete {
		t.Errorf("dryRun %+v != preview %+v", d.WouldDelete, p.WouldDelete)
	}
	if len(st.applied) != 0 || st.settings["ua"].RetentionHours != 4320 {
		t.Error("preview / dryRun changed state")
	}

	// Nothing doomed: no earliest date.
	w = doReq(r, "GET", "/api/account/settings/preview?retentionHours=17520", "alice", "")
	var none previewResponse
	_ = json.Unmarshal(w.Body.Bytes(), &none)
	if none.EarliestDeletionAt != nil || none.WouldDelete.Results != 0 {
		t.Errorf("expected empty preview, got %+v", none)
	}

	for _, q := range []string{"", "?retentionHours=23", "?retentionHours=x", "?retentionHours=17521"} {
		if w := doReq(r, "GET", "/api/account/settings/preview"+q, "alice", ""); w.Code != 400 {
			t.Errorf("preview%s = %d, want 400", q, w.Code)
		}
	}
}
