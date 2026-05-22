package geo

import (
	"net"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestExtractIP_CFConnecting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("CF-Connecting-IP", "1.2.3.4")
	if got := ExtractIP(c); got != "1.2.3.4" {
		t.Errorf("expected 1.2.3.4 from CF header, got %q", got)
	}
}

func TestExtractIP_XFFFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("X-Forwarded-For", "8.8.8.8, 10.0.0.1")
	if got := ExtractIP(c); got != "8.8.8.8" {
		t.Errorf("expected first XFF entry, got %q", got)
	}
}

func TestExtractIP_LoopbackBecomesEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.RemoteAddr = "127.0.0.1:12345"
	if got := ExtractIP(c); got != "" {
		t.Errorf("loopback should be empty, got %q", got)
	}
}

func TestIsLikelyPublic(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":     true,
		"1.1.1.1":     true,
		"10.0.0.1":    false,
		"192.168.0.1": false,
		"127.0.0.1":   false,
		"::1":         false,
		"169.254.1.1": false,
	}
	for s, want := range cases {
		ip := net.ParseIP(s)
		if got := IsLikelyPublic(ip); got != want {
			t.Errorf("IsLikelyPublic(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestLookup_NilEnricher_NoOp(t *testing.T) {
	var e *Enricher
	entry, err := e.Lookup(nil, "1.2.3.4")
	if err != nil {
		t.Fatalf("nil enricher should return nil, nil, got err=%v", err)
	}
	if entry != nil {
		t.Errorf("expected nil entry on nil enricher, got %+v", entry)
	}
}

func TestLookup_PrivateIPSkipped(t *testing.T) {
	e := &Enricher{}
	entry, err := e.Lookup(nil, "192.168.1.1")
	if err != nil {
		t.Fatalf("err on private IP: %v", err)
	}
	if entry != nil {
		t.Errorf("private IP should not produce an entry, got %+v", entry)
	}
}
