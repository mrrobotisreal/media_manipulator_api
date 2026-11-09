package burst

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/staging"
)

// WorkerKey is the S3 key of a worker build: burst/bin/<sha256>/burst-worker.
func WorkerKey(prefix, hash string) string {
	return RunPrefix(prefix, "bin") + "/" + hash + "/burst-worker"
}

// S3WorkerBinary hashes the deploy build of cmd/burst-worker and uploads it by
// hash when absent. The API builds nothing: the deploy step builds it next to
// the API binary.
type S3WorkerBinary struct {
	Store  staging.ObjectStore
	Path   string // the built binary
	Prefix string // "burst/"
	Log    *slog.Logger

	mu       sync.Mutex
	hash     string
	size     int64
	modified int64
}

// DefaultWorkerBinaryPaths are tried in order: next to the running API, then
// the working directory.
func DefaultWorkerBinaryPaths() []string {
	var out []string
	if exe, err := os.Executable(); err == nil {
		out = append(out, filepath.Join(filepath.Dir(exe), "burst-worker"))
	}
	return append(out, "burst-worker")
}

// FindWorkerBinary returns the first existing candidate.
func FindWorkerBinary(candidates []string) (string, bool) {
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() {
			return c, true
		}
	}
	return "", false
}

// EnsureUploaded re-hashes only when the file changed, then uploads when the
// object is missing.
func (w *S3WorkerBinary) EnsureUploaded(ctx context.Context) (string, string, error) {
	st, err := os.Stat(w.Path)
	if err != nil {
		return "", "", err
	}
	w.mu.Lock()
	if w.hash == "" || st.Size() != w.size || st.ModTime().UnixNano() != w.modified {
		_, sum, herr := staging.HashFile(w.Path)
		if herr != nil {
			w.mu.Unlock()
			return "", "", herr
		}
		w.hash, w.size, w.modified = sum, st.Size(), st.ModTime().UnixNano()
	}
	hash := w.hash
	w.mu.Unlock()
	key := WorkerKey(w.Prefix, hash)
	ok, err := w.Store.Exists(ctx, key)
	if err != nil {
		return "", "", err
	}
	if !ok {
		if err := w.Store.PutFile(ctx, key, w.Path); err != nil {
			return "", "", err
		}
		if w.Log != nil {
			w.Log.Info("burst: worker binary uploaded", "sha256", hash, "bytes", st.Size())
		}
	}
	return hash, key, nil
}

// PublishWorkerBinary uploads the deploy-built worker next to the API binary by
// hash. It logs and returns when there is no build: nothing launches before
// MM127, and the user-data builder fails closed without a binary.
func PublishWorkerBinary(ctx context.Context, store staging.ObjectStore, prefix string, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	path, ok := FindWorkerBinary(DefaultWorkerBinaryPaths())
	if !ok {
		log.Warn("burst: no burst-worker binary next to the API; build it in the deploy step (go build -o burst-worker ./cmd/burst-worker)")
		return
	}
	wb := &S3WorkerBinary{Store: store, Path: path, Prefix: prefix, Log: log}
	if hash, _, err := wb.EnsureUploaded(ctx); err != nil {
		log.Error("burst: worker binary upload failed", "error", err.Error())
	} else {
		log.Info("burst: worker binary ready", "sha256", hash)
	}
}
