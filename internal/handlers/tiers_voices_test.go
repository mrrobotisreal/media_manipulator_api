package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// MM77 acceptance 3: GET /api/tiers carries the four voices fields on every
// public tier, with the published values, and env overrides reach it.
func TestTiersResponseCarriesVoicesFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("TIER_PREMIUM_VOICES_SAVED", "7")
	cfg := config.Load()
	h := NewAccountHandler(nil, tiers.New(cfg), cfg, nil, nil, nil)
	r := gin.New()
	r.GET("/api/tiers", h.Tiers)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/tiers", nil))
	var body struct {
		Tiers []struct {
			Tier   string         `json:"tier"`
			Limits map[string]any `json:"limits"`
		} `json:"tiers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string][4]any{
		"anonymous": {0.0, 30.0, false, 0.0},
		"free":      {1.0, 60.0, true, 60.0},
		"premium":   {7.0, 300.0, true, 300.0},
		"pro":       {25.0, 900.0, true, 900.0},
	}
	if len(body.Tiers) != 4 {
		t.Fatalf("tiers: %s", w.Body.String())
	}
	for _, d := range body.Tiers {
		l := d.Limits
		if l == nil {
			t.Fatalf("%s: limits must be nested: %s", d.Tier, w.Body.String())
		}
		got := [4]any{l["voicesSaved"], l["voicesMaxSeconds"], l["voicesConvert"], l["voicesConvertMaxSeconds"]}
		if got != want[d.Tier] {
			t.Errorf("%s voices = %v want %v", d.Tier, got, want[d.Tier])
		}
	}
}
