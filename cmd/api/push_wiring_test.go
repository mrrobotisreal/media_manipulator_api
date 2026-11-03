package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/push"
)

type stubEmail struct{}

func (stubEmail) AIJobFinished(models.JobFinished) {}

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestPushDisabledWiresEmailOnly(t *testing.T) {
	called := false
	mk := func(context.Context, string) (push.Sender, error) { called = true; return &push.FakeSender{}, nil }
	em := stubEmail{}

	// PUSH_ENABLED unset: email alone, the sender is never built.
	got := buildJobNotifier(context.Background(), &config.Config{FirebaseProjectID: "p"}, nil, em, nil, nil, quietLog, mk)
	if _, ok := got.(stubEmail); !ok || called {
		t.Fatalf("disabled: got %T (sender built=%v), want the email service", got, called)
	}
	// Enabled but no project id / no database: still email only.
	got = buildJobNotifier(context.Background(), &config.Config{PushEnabled: true}, nil, em, nil, nil, quietLog, mk)
	if _, ok := got.(stubEmail); !ok || called {
		t.Fatalf("enabled without project/db: got %T", got)
	}
	// nil config is also the email path.
	if _, ok := buildJobNotifier(context.Background(), nil, nil, em, nil, nil, quietLog, mk).(stubEmail); !ok {
		t.Fatal("nil config must not wrap")
	}
	_ = errors.New
}
