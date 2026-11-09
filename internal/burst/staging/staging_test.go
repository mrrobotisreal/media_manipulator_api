package staging

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestManifest_RoundTripAndUploadDownload(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "a.wav"), "AAAA")
	write(t, filepath.Join(src, "d", "x", "b.bin"), "BBBBBB")
	write(t, filepath.Join(src, "d", "c.txt"), "")
	ctx := context.Background()
	mem := NewMemStore()
	m, err := Upload(ctx, mem, "burst/r1", "in", []Source{{N: 0, Local: filepath.Join(src, "a.wav")}, {N: 1, Local: filepath.Join(src, "d"), Dir: true}}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 3 || m.TotalBytes() != 10 {
		t.Fatalf("%+v", m)
	}
	got, err := FetchManifest(ctx, mem, "burst/r1", "in")
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("manifest round trip: %+v %v", got, err)
	}
	want := []string{"burst/r1/in/0", "burst/r1/in/1/c.txt", "burst/r1/in/1/x/b.bin", "burst/r1/in/manifest.json"}
	if k := mem.Keys(); !reflect.DeepEqual(k, want) {
		t.Fatalf("keys %v", k)
	}
	dst := t.TempDir()
	roots := map[int]string{0: filepath.Join(dst, "a.wav"), 1: filepath.Join(dst, "d")}
	if err := Download(ctx, mem, "burst/r1", "in", got, roots, 4); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dst, "d", "x", "b.bin"))
	if string(b) != "BBBBBB" {
		t.Fatal("content")
	}
	if left, _ := filepath.Glob(filepath.Join(dst, "*", "*.tmp")); len(left) != 0 {
		t.Fatal(left)
	}
}

func TestDownload_ChecksumMismatchErrorsAndPlacesNothing(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "o", "1.bin"), "one-one")
	write(t, filepath.Join(src, "o", "2.bin"), "two-two")
	ctx := context.Background()
	mem := NewMemStore()
	m, _ := Upload(ctx, mem, "burst/r", "out", []Source{{N: 0, Local: filepath.Join(src, "o"), Dir: true}}, 2)
	// Existing output must survive a failed fetch.
	write(t, filepath.Join(dst, "o", "1.bin"), "OLD")
	mem.CorruptOnGet = func(key string, b []byte) []byte {
		if strings.HasSuffix(key, "2.bin") {
			b[0] ^= 0xff // flip one byte, same size
		}
		return b
	}
	err := Download(ctx, mem, "burst/r", "out", m, map[int]string{0: filepath.Join(dst, "o")}, 2)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("want ErrChecksum, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "o", "1.bin")); string(b) != "OLD" {
		t.Fatal("output placed despite a failed batch")
	}
	var names []string
	ents, _ := os.ReadDir(filepath.Join(dst, "o"))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"1.bin"}) {
		t.Fatalf("temp files left: %v", names)
	}
	// Size mismatch too.
	mem.CorruptOnGet = func(key string, b []byte) []byte { return b[:len(b)-1] }
	if err := Download(ctx, mem, "burst/r", "out", m, map[int]string{0: filepath.Join(dst, "o")}, 2); !errors.Is(err, ErrChecksum) {
		t.Fatalf("size: %v", err)
	}
}

func TestParseManifest_RejectsTraversal(t *testing.T) {
	sum := strings.Repeat("a", 64)
	for _, rel := range []string{"../x", "/abs", "a//b", "a/../b", `a\b`} {
		b := []byte(`{"files":[{"n":0,"relPath":"` + strings.ReplaceAll(rel, `\`, `\\`) + `","size":1,"sha256":"` + sum + `"}]}`)
		if _, err := ParseManifest(b); err == nil {
			t.Errorf("%q accepted", rel)
		}
	}
	if _, err := ParseManifest([]byte(`{"files":[{"n":0,"relPath":"ok/f","size":1,"sha256":"short"}]}`)); err == nil {
		t.Error("short hash accepted")
	}
}

func TestUpload_BoundedConcurrency(t *testing.T) {
	src := t.TempDir()
	for i := 0; i < 20; i++ {
		write(t, filepath.Join(src, "d", string(rune('a'+i))), "x")
	}
	var cur, peak int32
	st := &slowStore{MemStore: NewMemStore(), cur: &cur, peak: &peak}
	if _, err := Upload(context.Background(), st, "p", "in", []Source{{N: 0, Local: filepath.Join(src, "d"), Dir: true}}, 3); err != nil {
		t.Fatal(err)
	}
	if peak > 3 {
		t.Fatalf("peak concurrency %d", peak)
	}
}

type slowStore struct {
	*MemStore
	cur, peak *int32
}

func (s *slowStore) PutFile(ctx context.Context, key, p string) error {
	n := atomic.AddInt32(s.cur, 1)
	for {
		o := atomic.LoadInt32(s.peak)
		if n <= o || atomic.CompareAndSwapInt32(s.peak, o, n) {
			break
		}
	}
	defer atomic.AddInt32(s.cur, -1)
	for i := 0; i < 2000; i++ { // give other workers time to overlap
		_ = i
	}
	return s.MemStore.PutFile(ctx, key, p)
}

func TestMemStore_DeletePrefixRefusesUnsafe(t *testing.T) {
	m := NewMemStore()
	_ = m.PutBytes(context.Background(), "burst/a/x", []byte("1"))
	_ = m.PutBytes(context.Background(), "burst/ab/x", []byte("1"))
	if err := m.DeletePrefix(context.Background(), "burst/a"); err == nil {
		t.Fatal("prefix without slash accepted")
	}
	if err := m.DeletePrefix(context.Background(), "burst/a/"); err != nil {
		t.Fatal(err)
	}
	if k := m.Keys(); len(k) != 1 || k[0] != "burst/ab/x" {
		t.Fatal(k)
	}
}

// fakeS3 is a tiny S3API for the S3Store (multipart path and prefix delete).
type fakeS3 struct {
	mu        sync.Mutex
	objs      map[string][]byte
	uploads   map[string][][]byte
	completed []string
	aborted   int
	failPart  int
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objs: map[string][]byte{}, uploads: map[string][][]byte{}}
}
func readAll(r interface{ Read([]byte) (int, error) }) []byte {
	var b bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.Bytes()
		}
	}
}
func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if in.ServerSideEncryption != s3types.ServerSideEncryptionAes256 {
		return nil, errors.New("no SSE")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[*in.Key] = readAll(in.Body)
	return &s3.PutObjectOutput{}, nil
}
func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objs[*in.Key]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: nopCloser{bytes.NewReader(b)}}, nil
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }
func (f *fakeS3) CreateMultipartUpload(_ context.Context, in *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads[*in.Key] = nil
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String("u-" + *in.Key)}, nil
}
func (f *fakeS3) UploadPart(_ context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPart > 0 && int(*in.PartNumber) == f.failPart {
		return nil, errors.New("part boom")
	}
	f.uploads[*in.Key] = append(f.uploads[*in.Key], readAll(in.Body))
	return &s3.UploadPartOutput{ETag: aws.String("e")}, nil
}
func (f *fakeS3) CompleteMultipartUpload(_ context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []byte
	for _, p := range f.uploads[*in.Key] {
		all = append(all, p...)
	}
	f.objs[*in.Key] = all
	f.completed = append(f.completed, *in.Key)
	return &s3.CompleteMultipartUploadOutput{}, nil
}
func (f *fakeS3) AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.mu.Lock()
	f.aborted++
	f.mu.Unlock()
	return &s3.AbortMultipartUploadOutput{}, nil
}
func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &s3.ListObjectsV2Output{}
	for k := range f.objs {
		if strings.HasPrefix(k, *in.Prefix) {
			out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k)})
		}
	}
	return out, nil
}
func (f *fakeS3) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range in.Delete.Objects {
		delete(f.objs, *o.Key)
	}
	return &s3.DeleteObjectsOutput{}, nil
}

func TestS3Store_MultipartOverThresholdAndDelete(t *testing.T) {
	api := newFakeS3()
	st := &S3Store{API: api, Bucket: "b", Threshold: 10, PartSize: 4}
	dir := t.TempDir()
	big, small := filepath.Join(dir, "big"), filepath.Join(dir, "small")
	write(t, big, "0123456789ABCDEF") // 16 bytes > 10: 4 parts
	write(t, small, "tiny")
	ctx := context.Background()
	if err := st.PutFile(ctx, "burst/r/in/0", big); err != nil {
		t.Fatal(err)
	}
	if err := st.PutFile(ctx, "burst/r/in/1", small); err != nil {
		t.Fatal(err)
	}
	if len(api.completed) != 1 || api.completed[0] != "burst/r/in/0" || len(api.uploads["burst/r/in/0"]) != 4 {
		t.Fatalf("multipart not used for the big file only: %+v", api.completed)
	}
	if b, _ := st.GetBytes(ctx, "burst/r/in/0"); string(b) != "0123456789ABCDEF" {
		t.Fatal("reassembly")
	}
	if _, err := st.GetBytes(ctx, "burst/r/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	api.failPart = 3
	if err := st.PutFile(ctx, "burst/r/in/2", big); err == nil || api.aborted != 1 {
		t.Fatalf("failed part must abort the upload: %v %d", err, api.aborted)
	}
	_ = st.PutBytes(ctx, "burst/other/x", []byte("1"))
	if err := st.DeletePrefix(ctx, "burst/r/"); err != nil {
		t.Fatal(err)
	}
	if len(api.objs) != 1 {
		t.Fatalf("%v", api.objs)
	}
}
