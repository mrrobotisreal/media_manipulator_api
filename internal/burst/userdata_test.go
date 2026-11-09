package burst

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/staging"
)

func sampleParams() UserDataParams {
	h := strings.Repeat("cd", 32)
	return UserDataParams{RunID: "0123abcd-0000-4000-8000-000000000000", Hostname: "mm-burst-0123abcd", Tag: "tag:mm-burst",
		TailscaleKey: "tskey-auth-kSECRETKEY-abc123", RunToken: strings.Repeat("9f", 32), Bucket: "media-manipulator", Region: "us-west-2",
		Prefix: "burst/0123abcd-0000-4000-8000-000000000000", WorkerHash: h, WorkerKey: WorkerKey("burst/", h), Port: 7781,
		MaxMinutes: 120, OrphanMinutes: 10, Family: "image", Arch: "x86_64", Prewarm: []string{"/opt/media-manipulator-ai/models/rembg"}}
}

func TestUserData_RenderAndRedact(t *testing.T) {
	p := sampleParams()
	ud, err := RenderUserData(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#!/bin/bash", "set -euo pipefail", "shutdown -h +120", "--hostname=mm-burst-0123abcd", "--advertise-tags=tag:mm-burst",
		"--accept-dns=false", "burst/bin/" + strings.Repeat("cd", 32) + "/burst-worker", "sha256sum -c", "chmod 0600 /etc/mm-burst/run.json", "systemctl start burst-worker", "/opt/dlami/nvme"} {
		if !strings.Contains(ud, want) {
			t.Errorf("missing %q", want)
		}
	}
	// run.json parses and holds the token.
	start := strings.Index(ud, "<<'MMBURSTJSON'\n") + len("<<'MMBURSTJSON'\n")
	end := strings.Index(ud, "\nMMBURSTJSON\n")
	var rj runJSON
	if err := json.Unmarshal([]byte(ud[start:end]), &rj); err != nil || rj.RunToken != p.RunToken || rj.Port != 7781 || rj.Prefix != p.Prefix {
		t.Fatalf("run.json: %+v %v", rj, err)
	}
	red := RedactUserData(ud, p.TailscaleKey, p.RunToken)
	if strings.Contains(red, "SECRETKEY") || strings.Contains(red, p.RunToken) || strings.Count(red, "<redacted>") != 2 {
		t.Fatalf("redaction:\n%s", red)
	}
	t.Logf("rendered user-data (secrets redacted):\n%s", red)
}

func TestUserData_RefusesUnsafeValues(t *testing.T) {
	for name, mut := range map[string]func(*UserDataParams){
		"key with newline": func(p *UserDataParams) { p.TailscaleKey = "k\nrm -rf /" },
		"hostname space":   func(p *UserDataParams) { p.Hostname = "a b" },
		"bucket quote":     func(p *UserDataParams) { p.Bucket = "b'; x" },
		"prewarm subshell": func(p *UserDataParams) { p.Prewarm = []string{"$(id)"} },
		"zero port":        func(p *UserDataParams) { p.Port = 0 },
	} {
		p := sampleParams()
		mut(&p)
		_, err := RenderUserData(p)
		if err == nil {
			t.Errorf("%s accepted", name)
		} else if strings.Contains(err.Error(), "rm -rf") {
			t.Errorf("%s: error carries the value", name)
		}
	}
}

func TestUserData_NeverLogged(t *testing.T) {
	r := newRig(testCfg())
	ub := &UserDataBuilder{Cfg: UserDataConfig{Bucket: "b", Region: "us-west-2", Tag: "tag:mm-burst", Prefix: "burst/", Port: 7781, MaxMinutes: 120, OrphanMinutes: 10},
		Binary: fakeBinary{hash: strings.Repeat("ab", 32), key: WorkerKey("burst/", strings.Repeat("ab", 32))}}
	r.p.UserData = ub.Build
	r.min.key = "tskey-auth-LOGGERSECRET"
	run, err := r.p.Launch(context.Background(), Offer{JobID: "j", Kind: "remove_background"})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := ub.Token(run.ID)
	sent := r.prov.launches[0].UserData
	if !strings.Contains(sent, "LOGGERSECRET") || !strings.Contains(sent, tok) {
		t.Fatal("secrets missing from the launch payload")
	}
	_ = r.p.Terminate(context.Background(), run, StateLostRace)
	if logs := r.logb.String(); strings.Contains(logs, "LOGGERSECRET") || strings.Contains(logs, tok) || strings.Contains(logs, "tskey-auth") {
		t.Fatalf("logger saw a secret:\n%s", logs)
	}
	// The ledger row carries neither.
	if row := r.st.get(run.ID); strings.Contains(row.ErrorType, "SECRET") {
		t.Fatal("row leak")
	}
	// A failing build reports no secret either.
	ub.Cfg.Port = 0
	if _, err := ub.Build(*run, Profile{}, "tskey-auth-LOGGERSECRET"); err == nil || strings.Contains(err.Error(), "LOGGERSECRET") {
		t.Fatalf("%v", err)
	}
}

func TestUserData_LongRemoteTimeoutRaisesShutdown(t *testing.T) {
	ub := &UserDataBuilder{Cfg: UserDataConfig{Bucket: "b", Region: "r", Tag: "tag:mm-burst", Prefix: "burst/", Port: 7781, MaxMinutes: 120, OrphanMinutes: 10},
		Binary: fakeBinary{hash: strings.Repeat("ab", 32), key: "burst/bin/x/burst-worker"}}
	ud, err := ub.Build(Run{ID: "0123abcd-0000-4000-8000-000000000000", Arch: "x86_64"}, Profile{Family: "restore", RemoteTimeout: 7200}, "tskey-auth-x")
	if err != nil || !strings.Contains(ud, "shutdown -h +135") {
		t.Fatalf("%v\n%s", err, ud)
	}
}

func TestWorkerHostname(t *testing.T) {
	if got := WorkerHostname("0123abcd-ef01-4000-8000-000000000000"); got != "mm-burst-0123abcd" {
		t.Fatal(got)
	}
}

func TestWorkerBinary_UploadsByHashOnlyWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "burst-worker")
	os.WriteFile(bin, []byte("ELF-fake-binary"), 0o755)
	mem := staging.NewMemStore()
	wb := &S3WorkerBinary{Store: mem, Path: bin, Prefix: "burst/"}
	ctx := context.Background()
	h, key, err := wb.EnsureUploaded(ctx)
	if err != nil || key != "burst/bin/"+h+"/burst-worker" || len(h) != 64 {
		t.Fatalf("%q %q %v", h, key, err)
	}
	if mem.PutCount != 1 {
		t.Fatal("not uploaded")
	}
	if _, _, err := wb.EnsureUploaded(ctx); err != nil || mem.PutCount != 1 {
		t.Fatalf("re-uploaded: %d %v", mem.PutCount, err)
	}
	// The lifecycle rule expired it: uploaded again.
	_ = mem.DeletePrefix(ctx, "burst/bin/"+h+"/")
	if _, _, err := wb.EnsureUploaded(ctx); err != nil || mem.PutCount != 2 {
		t.Fatalf("%d %v", mem.PutCount, err)
	}
	// A changed build gets a new hash and key.
	os.WriteFile(bin, []byte("ELF-fake-binary-v2"), 0o755)
	h2, _, _ := wb.EnsureUploaded(ctx)
	if h2 == h {
		t.Fatal("hash unchanged")
	}
	if _, ok := FindWorkerBinary([]string{filepath.Join(dir, "nope"), bin}); !ok {
		t.Fatal("not found")
	}
	if _, _, err := (&S3WorkerBinary{Store: mem, Path: filepath.Join(dir, "missing")}).EnsureUploaded(ctx); err == nil {
		t.Fatal("missing binary accepted")
	}
}

func TestReaperPrefixList_SkipsBinDir(t *testing.T) {
	// DeleteRun must refuse bin so the reaper can never delete worker builds.
	st := &S3Staging{API: nil, Bucket: "b", Prefix: "burst/"}
	if err := st.DeleteRun(context.Background(), BinDir); err != nil {
		t.Fatal(err) // returns nil without touching S3 (API is nil)
	}
}

func devicesServer(t *testing.T, gotPath *string, devices string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v2/oauth/token":
			w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
		case strings.HasSuffix(r.URL.Path, "/devices"):
			*gotPath = r.URL.EscapedPath()
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(devices))
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestTailscale_ResolveIPAndDashTailnet(t *testing.T) {
	var path string
	srv := devicesServer(t, &path, `{"devices":[
	 {"id":"1","name":"home.tail1234.ts.net","hostname":"home","addresses":["100.1.1.1","fd7a::1"]},
	 {"id":"2","name":"mm-burst-0123abcd.tail1234.ts.net","hostname":"mm-burst-0123abcd","addresses":["fd7a:115c::9","100.64.0.9"]}]}`)
	defer srv.Close()
	m := &TailscaleMinter{BaseURL: srv.URL, ClientID: "id", ClientSecret: "s", Tailnet: "-", Tag: "tag:mm-burst"}
	if got := m.DevicesURL(); got != srv.URL+"/api/v2/tailnet/-/devices" {
		t.Fatalf("dash tailnet url: %s", got)
	}
	ip, err := m.ResolveIP(context.Background(), "mm-burst-0123abcd")
	if err != nil || ip != "100.64.0.9" {
		t.Fatalf("%q %v", ip, err)
	}
	if path != "/api/v2/tailnet/-/devices" {
		t.Fatalf("server saw %q", path)
	}
	// The MagicDNS short name matches too (hostname can be renamed on conflict).
	if ip, err := m.ResolveIP(context.Background(), "home"); err != nil || ip != "100.1.1.1" {
		t.Fatalf("%q %v", ip, err)
	}
	if _, err := m.ResolveIP(context.Background(), "mm-burst-ffffffff"); err != ErrDeviceNotFound {
		t.Fatalf("%v", err)
	}
	// A named tailnet is escaped, and the key-minting URL for "-" is unchanged too.
	m.Tailnet = "example.com"
	if got := m.DevicesURL(); got != srv.URL+"/api/v2/tailnet/example.com/devices" {
		t.Fatal(got)
	}
}

func TestTailscale_ResolveIPErrors(t *testing.T) {
	var path string
	srv := devicesServer(t, &path, `not json`)
	defer srv.Close()
	m := &TailscaleMinter{BaseURL: srv.URL, ClientID: "id", ClientSecret: "s", Tailnet: "-"}
	if _, err := m.ResolveIP(context.Background(), "x"); err == nil || err == ErrDeviceNotFound {
		t.Fatalf("%v", err)
	}
}
