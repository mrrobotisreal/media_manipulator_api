package limits

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

func TestHashIP_Stable(t *testing.T) {
	l := New(nil, &config.Config{}, nil, nil)
	a := l.HashIP("1.2.3.4")
	b := l.HashIP("1.2.3.4")
	if a != b {
		t.Errorf("hash not stable: %q != %q", a, b)
	}
	c := l.HashIP("5.6.7.8")
	if a == c {
		t.Errorf("different inputs should hash differently")
	}
}

func TestHashIP_NotPlaintext(t *testing.T) {
	l := New(nil, &config.Config{}, nil, nil)
	h := l.HashIP("1.2.3.4")
	if strings.Contains(h, "1.2.3.4") {
		t.Errorf("hash should not contain raw IP: %q", h)
	}
}

func TestEnabled_NilRedis(t *testing.T) {
	l := New(nil, &config.Config{RateLimitEnabled: true}, nil, nil)
	if l.Enabled() {
		t.Errorf("nil Redis should yield Enabled()=false")
	}
}

// fakeWindow counts per key and records which keys and limits were used.
type fakeWindow struct {
	mu     sync.Mutex
	counts map[string]int
	limits map[string]int
}

func newFakeWindow() *fakeWindow {
	return &fakeWindow{counts: map[string]int{}, limits: map[string]int{}}
}

func (f *fakeWindow) fn(_ context.Context, key string, limit int, _ time.Duration) Decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[key]++
	f.limits[key] = limit
	return Decision{Allowed: f.counts[key] <= limit, LimitCount: limit, RetryAfterSeconds: 5}
}

func (f *fakeWindow) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.counts {
		out = append(out, k)
	}
	return out
}

func testLimiter(f *fakeWindow) *Limiter {
	l := New(nil, &config.Config{RateLimitEnabled: true, RateLimitPerIPRPS: 10, RateLimitPerIPBurst: 20, RateLimitPerIPBurstAuthed: 60}, nil, nil)
	l.SetWindowCounterForTest(f.fn)
	return l
}

func routeEngine(l *Limiter, session, ip int) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(l.Route("upload", "upload", session, ip))
	r.POST("/x", func(c *gin.Context) { c.Status(200) })
	return r
}

func post(r *gin.Engine, sid string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/x", nil)
	req.RemoteAddr = "9.9.9.9:1234"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func hasPrefix(keys []string, p string) bool {
	for _, k := range keys {
		if strings.HasPrefix(k, keyPrefix+p) {
			return true
		}
	}
	return false
}

func TestRouteLimitUserBucket(t *testing.T) {
	t.Run("synced user gets one user bucket at the session limit and 429 scope user", func(t *testing.T) {
		f := newFakeWindow()
		l := testLimiter(f)
		l.UserSubject = func(*gin.Context) (string, bool) { return "user-1", true }
		r := routeEngine(l, 2, 5)
		for i := 0; i < 2; i++ {
			if w := post(r, ""); w.Code != 200 {
				t.Fatalf("call %d: %d", i, w.Code)
			}
		}
		w := post(r, "")
		if w.Code != 429 || !strings.Contains(w.Body.String(), `"scope":"user"`) {
			t.Fatalf("got %d %s", w.Code, w.Body.String())
		}
		keys := f.keys()
		if len(keys) != 1 || !hasPrefix(keys, "user:") || strings.Contains(keys[0], "user-1") {
			t.Fatalf("want one hashed user key, got %v", keys)
		}
	})
	t.Run("unsynced or anonymous falls back to the ip bucket", func(t *testing.T) {
		f := newFakeWindow()
		l := testLimiter(f)
		l.UserSubject = func(*gin.Context) (string, bool) { return "", false }
		r := routeEngine(l, 2, 5)
		post(r, "")
		if !hasPrefix(f.keys(), "ip:") || hasPrefix(f.keys(), "user:") {
			t.Fatalf("keys %v", f.keys())
		}
	})
	t.Run("nil hook is unchanged", func(t *testing.T) {
		f := newFakeWindow()
		r := routeEngine(testLimiter(f), 2, 5)
		post(r, "")
		if !hasPrefix(f.keys(), "ip:") {
			t.Fatalf("keys %v", f.keys())
		}
	})
	t.Run("operator exemption fires before any bucket", func(t *testing.T) {
		f := newFakeWindow()
		l := testLimiter(f)
		l.RouteExempt = func(*gin.Context) bool { return true }
		l.UserSubject = func(*gin.Context) (string, bool) { return "user-1", true }
		r := routeEngine(l, 1, 1)
		for i := 0; i < 5; i++ {
			if w := post(r, ""); w.Code != 200 {
				t.Fatalf("operator blocked: %d", w.Code)
			}
		}
		if len(f.keys()) != 0 {
			t.Fatalf("exempt must touch no bucket: %v", f.keys())
		}
	})
}

func TestGlobalBurstAuthedAllowance(t *testing.T) {
	run := func(authed bool) (allowed int) {
		f := newFakeWindow()
		l := testLimiter(f)
		l.Authed = func(*gin.Context) bool { return authed }
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.Use(l.GlobalIPRPS())
		r.GET("/x", func(c *gin.Context) { c.Status(200) })
		for i := 0; i < 100; i++ {
			req := httptest.NewRequest("GET", "/x", nil)
			req.RemoteAddr = "9.9.9.9:1"
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code == 200 {
				allowed++
			}
		}
		return allowed
	}
	if got := run(false); got != 20 {
		t.Fatalf("anonymous allowed %d want 20", got)
	}
	if got := run(true); got != 60 {
		t.Fatalf("authed allowed %d want 60", got)
	}
}
