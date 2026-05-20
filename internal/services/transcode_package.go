package services

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// createTarGz writes a gzipped tar of every file rooted at sourceDir to dest.
// Paths inside the archive are stored relative to sourceDir using forward
// slashes so the package extracts cleanly on any OS.
func createTarGz(sourceDir, dest string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	out, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	gz := gzip.NewWriter(out)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	err = filepath.Walk(sourceDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return 0, err
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	st, err := os.Stat(dest)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// createZip writes a regular ZIP archive of every file rooted at sourceDir to
// dest. Paths inside the archive are stored relative to sourceDir using
// forward slashes (the Info-ZIP convention) so the archive extracts cleanly
// on Windows, macOS, and Linux. Compression is deflate; .ts/.m4s/.jpg payloads
// are already compressed and benefit little from extra deflate, but the cost
// is minimal and it keeps the playlists + manifest.mpd + report.json small.
func createZip(sourceDir, dest string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	out, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	zw := zip.NewWriter(out)
	defer zw.Close()

	err = filepath.Walk(sourceDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// zip requires the name to use forward slashes; directories end with "/".
		name := filepath.ToSlash(rel)
		if info.IsDir() {
			name += "/"
			if _, err := zw.Create(name); err != nil {
				return err
			}
			return nil
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = name
		header.Method = zip.Deflate
		w, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if err != nil {
		return 0, err
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	st, err := os.Stat(dest)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// writeReadme drops a small explanatory text file into the package root so
// downloaders aren't staring at a tarball full of init.mp4 files with no clue
// what they are.
func writeReadme(dest string, protocol string, dashCodec string, captionsIncluded, storyboardsIncluded bool) error {
	var b strings.Builder
	b.WriteString("Media Manipulator transcode package\n")
	b.WriteString("====================================\n\n")
	b.WriteString(fmt.Sprintf("Protocol: %s\n", strings.ToUpper(protocol)))
	if protocol == "dash" && dashCodec != "" {
		b.WriteString(fmt.Sprintf("Codec: %s\n", strings.ToUpper(dashCodec)))
	}
	b.WriteString("\nLayout:\n")
	if protocol == "hls" {
		b.WriteString("  hls/master.m3u8        Top-level HLS playlist\n")
		b.WriteString("  hls/<rung>/index.m3u8  Per-rendition variant playlist\n")
		b.WriteString("  hls/<rung>/segments/   MPEG-TS segments (.ts)\n")
	} else {
		b.WriteString("  dash/manifest.mpd               DASH manifest\n")
		b.WriteString("  dash/<rung>/init.mp4            Per-rendition init segment\n")
		b.WriteString("  dash/<rung>/seg-*.m4s           Per-rendition media segments\n")
		b.WriteString("  dash/audio/128k/init.mp4        Audio init (if source has audio)\n")
		b.WriteString("  dash/audio/128k/seg-*.m4s       Audio segments\n")
	}
	if captionsIncluded {
		b.WriteString("  captions/auto.vtt      Auto-generated WebVTT captions\n")
	}
	if storyboardsIncluded {
		b.WriteString("  storyboards/storyboard.vtt and sprite_NNN.jpg files for scrubber thumbnails\n")
	}
	b.WriteString("\nreport.json contains the source probe, generated variants, and warnings.\n")
	b.WriteString("\nYour package is hosted on a temporary S3 URL — download promptly; the link expires.\n")
	return os.WriteFile(dest, []byte(b.String()), 0o644)
}
