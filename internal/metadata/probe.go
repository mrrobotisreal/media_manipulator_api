package metadata

import (
	"context"
	"strings"
)

// Probe reports the installed exiftool and ffprobe versions for the startup
// log. Missing tools are reported as "missing", never as an error.
func Probe(ctx context.Context) (exiftool, ffprobe string) {
	return probeWith(ctx, execRunner{})
}

func probeWith(ctx context.Context, r Runner) (exiftool, ffprobe string) {
	exiftool, ffprobe = "missing", "missing"
	cctx, cancel := context.WithTimeout(ctx, ToolTimeout)
	defer cancel()
	if out, _, err := r.Run(cctx, "exiftool", "-ver"); err == nil && strings.TrimSpace(out) != "" {
		exiftool = strings.TrimSpace(out)
	}
	if out, _, err := r.Run(cctx, "ffprobe", "-version"); err == nil {
		if f := strings.Fields(strings.SplitN(out, "\n", 2)[0]); len(f) >= 3 {
			ffprobe = f[2]
		} else if len(f) > 0 {
			ffprobe = "present"
		}
	}
	return exiftool, ffprobe
}
