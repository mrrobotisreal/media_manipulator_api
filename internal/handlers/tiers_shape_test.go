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

// GET /api/tiers: the retired batch flag is gone (no batch feature exists) and
// exportsPerDay is present on every tier.
func TestTiersResponseShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	h := NewAccountHandler(nil, tiers.New(cfg), cfg, nil, nil, nil)
	r := gin.New()
	r.GET("/api/tiers", h.Tiers)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/tiers", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var body struct {
		Tiers []map[string]any `json:"tiers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tiers) != 3 {
		t.Fatalf("want 3 public tiers, got %d", len(body.Tiers))
	}
	for _, d := range body.Tiers {
		limits, _ := d["limits"].(map[string]any)
		if limits == nil {
			// Limits may be embedded flat instead of nested.
			limits = d
		}
		if _, has := limits["batch"+"Enabled"]; has {
			t.Errorf("%v: retired batch flag must be absent", d["tier"])
		}
		if _, has := limits["exportsPerDay"]; !has {
			t.Errorf("%v: exportsPerDay must be present", d["tier"])
		}
	}
}
