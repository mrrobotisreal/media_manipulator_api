// Command gen-email-locales copies the UI's email strings into the API.
//
// The UI owns the wording: media-manipulator-ui/i18n/locales/<locale>/interface/email.json.
// The API renders mail server-side from a shipped copy at
// internal/email/locales/<locale>.json. Run this from media_manipulator_api/
// whenever the UI strings change, then commit the result:
//
//	go run ./scripts/gen-email-locales [-ui ../media-manipulator-ui] [-out internal/email/locales]
//
// The UI file may be {"email": {...}} or the bare group; the output is always
// {"email": {...}}. A locale with no UI file is skipped (the renderer falls back
// to en-us key by key), so the committed English file is only replaced once the
// UI file exists. -check exits 1 when any output would change (CI drift guard).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mrrobotisreal/media_manipulator_api/internal/email"
)

func main() {
	ui := flag.String("ui", "../media-manipulator-ui", "path to the UI repo")
	out := flag.String("out", "internal/email/locales", "output directory")
	check := flag.Bool("check", false, "fail if any output would change")
	flag.Parse()

	drift := false
	for _, loc := range email.Locales {
		src := filepath.Join(*ui, "i18n", "locales", loc, "interface", "email.json")
		raw, err := os.ReadFile(src)
		if err != nil {
			fmt.Printf("skip %s: %v\n", loc, err)
			continue
		}
		var tree map[string]any
		if err := json.Unmarshal(raw, &tree); err != nil {
			fmt.Fprintf(os.Stderr, "%s: invalid JSON: %v\n", src, err)
			os.Exit(2)
		}
		if _, ok := tree["email"]; !ok {
			tree = map[string]any{"email": tree}
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(tree); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		dst := filepath.Join(*out, loc+".json")
		if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, buf.Bytes()) {
			fmt.Printf("ok   %s\n", dst)
			continue
		}
		if *check {
			fmt.Printf("DRIFT %s\n", dst)
			drift = true
			continue
		}
		if err := os.WriteFile(dst, buf.Bytes(), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Printf("wrote %s\n", dst)
	}
	if drift {
		os.Exit(1)
	}
}
