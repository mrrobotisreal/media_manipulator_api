// Command burst-worker is the GPU burst worker (MM125, design §10). It runs on
// a burst instance, listens on the tailnet IP only and executes one declared
// spec for the API. It is built statically (CGO_ENABLED=0) and fetched by hash
// from s3://<bucket>/burst/bin/<sha256>/burst-worker by the run's user-data.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/staging"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/wire"
	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/worker"
)

// version is stamped by the build (-ldflags "-X main.version=...").
var version = "dev"

// runFile is /etc/mm-burst/run.json, written by the user-data (mode 0600).
type runFile struct {
	RunID    string   `json:"runId"`
	RunToken string   `json:"runToken"`
	Bucket   string   `json:"bucket"`
	Prefix   string   `json:"prefix"` // "burst/<runId>"
	Port     int      `json:"port"`
	Region   string   `json:"region"`
	Family   string   `json:"family"`
	Arch     string   `json:"arch"`
	Manifest string   `json:"manifestHash"`
	Orphan   int      `json:"orphanMinutes"`
	Prewarm  []string `json:"prewarm"`
	Session  bool     `json:"session"`
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	path := "/etc/mm-burst/run.json"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Error("worker: cannot read run file")
		os.Exit(2)
	}
	var rf runFile
	if err := json.Unmarshal(b, &rf); err != nil || rf.RunID == "" || rf.RunToken == "" || rf.Bucket == "" || rf.Prefix == "" {
		log.Error("worker: run file is incomplete")
		os.Exit(2)
	}
	if rf.Port == 0 {
		rf.Port = 7781
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(rf.Region)) // instance role
	if err != nil {
		log.Error("worker: aws config failed")
		os.Exit(2)
	}
	store := &staging.S3Store{API: s3.NewFromConfig(awsCfg), Bucket: rf.Bucket}

	prewarm := startPrewarm(rf.Prewarm)
	w := worker.New(worker.Config{
		RunID: rf.RunID, Token: rf.RunToken, Prefix: rf.Prefix, Version: version,
		Image:     wire.ImageInfo{Family: rf.Family, Arch: rf.Arch, ManifestHash: rf.Manifest},
		OrphanMin: rf.Orphan, Session: rf.Session, Store: store, Log: log, Workers: staging.DefaultConcurrency,
		Shutdown:    func() { _ = exec.Command("shutdown", "-h", "now").Run() },
		FreeGiB:     func() float64 { return freeGiB("/opt/dlami/nvme") },
		GPU:         gpuInfo,
		PrewarmDone: prewarm,
	})
	go w.WatchOrphan(ctx, 15*time.Second)

	ip, err := tailnetIP()
	if err != nil {
		log.Error("worker: no tailnet address; refusing to listen elsewhere")
		os.Exit(2)
	}
	srv := &http.Server{Addr: net.JoinHostPort(ip, strconv.Itoa(rf.Port)), Handler: w.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	log.Info("worker: listening", "run", rf.RunID, "version", version)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("worker: server stopped", "error", err.Error())
		os.Exit(1)
	}
}

// tailnetIP finds the interface address in the Tailscale CGNAT range.
func tailnetIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && cgnat.Contains(ipn.IP) {
			return ipn.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no tailnet address")
}

func freeGiB(dir string) float64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0
	}
	return float64(st.Bavail) * float64(st.Bsize) / (1 << 30)
}

func gpuInfo() wire.GPUInfo {
	out, err := exec.Command("nvidia-smi", "--query-gpu=name,memory.total,driver_version", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return wire.GPUInfo{}
	}
	f := strings.Split(strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), ",")
	if len(f) < 3 {
		return wire.GPUInfo{}
	}
	mem, _ := strconv.Atoi(strings.TrimSpace(f[1]))
	g := wire.GPUInfo{Name: strings.TrimSpace(f[0]), MemMiB: mem, Driver: strings.TrimSpace(f[2])}
	if v, err := exec.Command("nvcc", "--version").Output(); err == nil {
		if i := strings.Index(string(v), "release "); i >= 0 {
			g.CUDA = strings.TrimRight(strings.Fields(string(v)[i+8:])[0], ",")
		}
	}
	return g
}

// startPrewarm reads the profile's prewarm paths in the background and returns
// a done func (true once finished; nil list = already done).
func startPrewarm(paths []string) func() bool {
	if len(paths) == 0 {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, p := range paths {
			_ = exec.Command("sh", "-c", `find "$1" -type f -exec cat {} + >/dev/null 2>&1`, "sh", p).Run()
		}
	}()
	return func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
}
