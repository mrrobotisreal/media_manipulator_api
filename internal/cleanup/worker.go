// Package cleanup runs a background sweeper over upload/output/temp dirs.
//
// We avoid deleting jobs that the in-memory JobManager still considers
// active (pending/processing) and we never traverse symlinks out of the
// configured roots.
package cleanup

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/metrics"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// ActiveJobs reports the set of in-flight job ids that should not be
// touched by the sweeper. Implementations should return a snapshot of
// currently pending/processing jobs.
type ActiveJobs interface {
	ActiveJobIDs() map[string]struct{}
}

// Worker periodically scans the configured directories.
type Worker struct {
	Cfg     *config.Config
	Store   *telemetry.Store
	Metrics *metrics.Registry
	Logger  *slog.Logger
	Active  ActiveJobs

	stop atomic.Bool
}

// NewWorker constructs a Worker. Active may be nil — in that case nothing is
// considered active.
func NewWorker(cfg *config.Config, store *telemetry.Store, m *metrics.Registry, logger *slog.Logger, active ActiveJobs) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{Cfg: cfg, Store: store, Metrics: m, Logger: logger, Active: active}
}

// Run blocks until ctx is cancelled, ticking at CleanupInterval.
func (w *Worker) Run(ctx context.Context) {
	if !w.Cfg.CleanupEnabled {
		w.Logger.Info("cleanup worker disabled via config")
		return
	}
	interval := w.Cfg.CleanupInterval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// Run once immediately.
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// tick runs a single sweep.
func (w *Worker) tick(ctx context.Context) {
	if w.stop.Load() {
		return
	}
	started := time.Now().UTC()
	run := telemetry.CleanupRun{
		StartedAt: started,
		Status:    "ok",
		UploadDir: w.Cfg.UploadDir,
		OutputDir: w.Cfg.OutputDir,
		TempDir:   w.Cfg.TempDir,
	}
	logger := w.Logger.With("stage", "cleanup", "dryRun", w.Cfg.CleanupDryRun)
	logger.Info("cleanup tick started")

	var active map[string]struct{}
	if w.Active != nil {
		active = w.Active.ActiveJobIDs()
	}

	type sweepSpec struct {
		root      string
		retention time.Duration
		jobAware  bool
	}
	sweeps := []sweepSpec{
		{root: w.Cfg.UploadDir, retention: w.Cfg.UploadRetention, jobAware: true},
		{root: w.Cfg.OutputDir, retention: w.Cfg.OutputRetention, jobAware: true},
		{root: w.Cfg.TempDir, retention: w.Cfg.TempRetention, jobAware: false},
	}

	var (
		files    int64
		dirs     int64
		bytes    int64
		paths    []telemetry.CleanupPath
		maxPaths = w.Cfg.CleanupAuditMaxPathsPerRun
		errMsg   string
	)
	if maxPaths <= 0 {
		maxPaths = 1000
	}

	for _, sp := range sweeps {
		if strings.TrimSpace(sp.root) == "" || sp.retention <= 0 {
			continue
		}
		f, d, b, p, err := w.sweepRoot(ctx, sp.root, sp.retention, sp.jobAware, active, maxPaths-len(paths))
		files += f
		dirs += d
		bytes += b
		paths = append(paths, p...)
		if err != nil {
			run.Status = "error"
			if errMsg == "" {
				errMsg = err.Error()
			} else {
				errMsg += "; " + err.Error()
			}
			if w.Metrics != nil {
				w.Metrics.CleanupError()
			}
		}
		if len(paths) >= maxPaths {
			break
		}
	}

	run.DeletedFiles = files
	run.DeletedDirs = dirs
	run.DeletedBytes = bytes
	run.ErrorMessage = errMsg
	completed := time.Now().UTC()
	run.CompletedAt = &completed
	logger.Info("cleanup tick complete",
		"deletedFiles", files, "deletedDirs", dirs,
		"deletedBytes", bytes, "errors", errMsg)
	if w.Metrics != nil {
		w.Metrics.CleanupDeleted(files, bytes)
	}
	if w.Store != nil {
		runID := w.Store.InsertCleanupRun(ctx, run)
		if runID != "" && len(paths) > 0 {
			for i := range paths {
				paths[i].CleanupRunID = runID
			}
			w.Store.InsertCleanupPaths(ctx, paths)
		}
	}
}

// sweepRoot walks root and deletes anything older than retention, with
// safety checks for the configured directories.
func (w *Worker) sweepRoot(ctx context.Context, root string, retention time.Duration, jobAware bool, active map[string]struct{}, capPaths int) (int64, int64, int64, []telemetry.CleanupPath, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	stat, err := os.Stat(absRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, 0, nil, nil
		}
		return 0, 0, 0, nil, err
	}
	if !stat.IsDir() {
		return 0, 0, 0, nil, errors.New("cleanup root is not a directory: " + absRoot)
	}
	now := time.Now()
	var (
		files int64
		dirs  int64
		bytes int64
		paths []telemetry.CleanupPath
	)

	// Top-level entries: typically job-id-named subdirs for upload/output,
	// or arbitrary temp files for temp.
	entries, err := os.ReadDir(absRoot)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return files, dirs, bytes, paths, ctx.Err()
		}
		name := e.Name()
		// Skip dot files (hidden ops artifacts).
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(absRoot, name)
		// Guard against symlink escapes.
		if !isWithin(full, absRoot) {
			w.Logger.Warn("cleanup skipping path outside root", "path", "<root>/"+name)
			continue
		}
		if jobAware && e.IsDir() {
			if _, busy := active[name]; busy {
				continue
			}
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		age := now.Sub(info.ModTime())
		if age < retention {
			continue
		}
		size := int64(0)
		pathType := "file"
		if e.IsDir() {
			pathType = "dir"
			sub, subBytes, subErr := dirSize(full)
			size = subBytes
			dirs++
			files += sub
			if subErr != nil {
				w.Logger.Warn("cleanup walk error", "err", subErr.Error())
			}
		} else {
			size = info.Size()
			files++
		}
		bytes += size
		if w.Cfg.CleanupDryRun {
			if len(paths) < capPaths {
				paths = append(paths, telemetry.CleanupPath{
					PathRedacted: filepath.Base(absRoot) + "/" + name,
					PathType:     pathType,
					AgeSeconds:   int(age / time.Second),
					SizeBytes:    size,
					DeletedAt:    time.Now().UTC(),
					ErrorMessage: "dry_run",
				})
			}
			continue
		}
		var removeErr error
		if e.IsDir() {
			removeErr = os.RemoveAll(full)
		} else {
			removeErr = os.Remove(full)
		}
		if removeErr != nil {
			w.Logger.Warn("cleanup remove failed", "path", filepath.Base(absRoot)+"/"+name, "err", removeErr.Error())
			if w.Metrics != nil {
				w.Metrics.CleanupError()
			}
			if len(paths) < capPaths {
				paths = append(paths, telemetry.CleanupPath{
					PathRedacted: filepath.Base(absRoot) + "/" + name,
					PathType:     pathType,
					AgeSeconds:   int(age / time.Second),
					SizeBytes:    size,
					DeletedAt:    time.Now().UTC(),
					ErrorMessage: removeErr.Error(),
				})
			}
			continue
		}
		if len(paths) < capPaths {
			paths = append(paths, telemetry.CleanupPath{
				PathRedacted: filepath.Base(absRoot) + "/" + name,
				PathType:     pathType,
				AgeSeconds:   int(age / time.Second),
				SizeBytes:    size,
				DeletedAt:    time.Now().UTC(),
			})
		}
	}
	return files, dirs, bytes, paths, nil
}

// isWithin returns true if candidate is inside root (no symlink-escape).
//
// filepath.Rel returns "../…" when candidate lies outside root, and an error
// when the paths span different volumes — both treated as escape.
func isWithin(candidate, root string) bool {
	cand, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	r := filepath.Clean(root)
	rel, err := filepath.Rel(r, cand)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if strings.HasPrefix(rel, "..") {
		return false
	}
	return true
}

// dirSize totals up files and bytes in a directory tree.
func dirSize(root string) (int64, int64, error) {
	var (
		files int64
		bytes int64
	)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		files++
		bytes += info.Size()
		return nil
	})
	return files, bytes, err
}
