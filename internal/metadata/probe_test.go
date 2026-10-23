package metadata

import (
	"context"
	"errors"
	"testing"
)

func TestProbeWith(t *testing.T) {
	r := &fakeRunner{handler: func(_ context.Context, name string, _ []string) (string, string, error) {
		if name == "exiftool" {
			return "12.76\n", "", nil
		}
		return "ffprobe version 7.1 Copyright (c) 2007-2024\n", "", nil
	}}
	if e, f := probeWith(context.Background(), r); e != "12.76" || f != "7.1" {
		t.Fatalf("%q %q", e, f)
	}
	r = &fakeRunner{handler: func(context.Context, string, []string) (string, string, error) {
		return "", "", errors.New("not found")
	}}
	if e, f := probeWith(context.Background(), r); e != "missing" || f != "missing" {
		t.Fatalf("%q %q", e, f)
	}
}
