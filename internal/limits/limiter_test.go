package limits

import (
	"strings"
	"testing"

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
