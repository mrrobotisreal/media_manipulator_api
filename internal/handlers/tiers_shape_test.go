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

// GET /api/tiers: the retired batch flag is gone (no batch feature exists),
// exportsPerDay is present on every tier, the four public tiers are listed in
// ladder order (MM39) and the Pro purchase/price fields sit beside Premium's.
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
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("Cache-Control = %q", got)
	}
	var body struct {
		Tiers []map[string]any `json:"tiers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tiers) != 4 {
		t.Fatalf("want 4 public tiers, got %d", len(body.Tiers))
	}
	for i, want := range []string{"anonymous", "free", "premium", "pro"} {
		if body.Tiers[i]["tier"] != want {
			t.Errorf("tiers[%d] = %v, want %s", i, body.Tiers[i]["tier"], want)
		}
	}
	var top map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"premiumPurchasable", "premiumPriceUSD", "premiumPriceYearlyUSD", "proPurchasable", "proPriceUSD", "proPriceYearlyUSD"} {
		if _, has := top[k]; !has {
			t.Errorf("missing top-level field %s", k)
		}
	}
	if _, isBool := top["proPurchasable"].(bool); !isBool {
		t.Errorf("proPurchasable must be a bool, got %T", top["proPurchasable"])
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
