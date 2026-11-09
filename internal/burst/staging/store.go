// Package staging moves declared files between a host and the transient
// s3://<bucket>/burst/<runId>/ prefix with sha256 manifests (MM125, design
// §7). The API uses it to stage inputs and fetch outputs; the worker uses the
// mirror image. Nothing here knows about commands or paths on the API host.
package staging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ErrNotFound is returned for a missing key.
var ErrNotFound = errors.New("staging: object not found")

// MultipartThreshold: files over this size use multipart (decision 3).
const MultipartThreshold int64 = 64 << 20

// ObjectStore is the slice of S3 the staging code needs.
type ObjectStore interface {
	PutFile(ctx context.Context, key, path string) error
	PutBytes(ctx context.Context, key string, b []byte) error
	GetBytes(ctx context.Context, key string) ([]byte, error)
	// Exists reports whether key exists.
	Exists(ctx context.Context, key string) (bool, error)
	// GetTo streams the object into w.
	GetTo(ctx context.Context, key string, w io.Writer) error
	// DeletePrefix removes every object under prefix.
	DeletePrefix(ctx context.Context, prefix string) error
}

// S3API is the slice of *s3.Client the S3Store uses.
type S3API interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	CreateMultipartUpload(ctx context.Context, in *s3.CreateMultipartUploadInput, opts ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(ctx context.Context, in *s3.UploadPartInput, opts ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, opts ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput, opts ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, opts ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// S3Store implements ObjectStore on S3 (SSE-S3 on every write).
type S3Store struct {
	API       S3API
	Bucket    string
	Threshold int64 // default MultipartThreshold
	PartSize  int64 // default 16 MiB (min 5 MiB for real S3)
}

func (s *S3Store) threshold() int64 {
	if s.Threshold > 0 {
		return s.Threshold
	}
	return MultipartThreshold
}

func (s *S3Store) partSize() int64 {
	if s.PartSize > 0 {
		return s.PartSize
	}
	return 16 << 20
}

func (s *S3Store) PutBytes(ctx context.Context, key string, b []byte) error {
	_, err := s.API.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(key),
		Body: bytes.NewReader(b), ServerSideEncryption: s3types.ServerSideEncryptionAes256})
	return err
}

func (s *S3Store) PutFile(ctx context.Context, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() <= s.threshold() {
		_, err = s.API.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(key),
			Body: f, ContentLength: aws.Int64(st.Size()), ServerSideEncryption: s3types.ServerSideEncryptionAes256})
		return err
	}
	return s.multipart(ctx, key, f, st.Size())
}

func (s *S3Store) multipart(ctx context.Context, key string, f *os.File, size int64) error {
	cm, err := s.API.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(s.Bucket), Key: aws.String(key),
		ServerSideEncryption: s3types.ServerSideEncryptionAes256})
	if err != nil {
		return err
	}
	abort := func() {
		_, _ = s.API.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{Bucket: aws.String(s.Bucket), Key: aws.String(key), UploadId: cm.UploadId})
	}
	var parts []s3types.CompletedPart
	ps := s.partSize()
	for off, n := int64(0), int32(1); off < size; off, n = off+ps, n+1 {
		l := ps
		if off+l > size {
			l = size - off
		}
		out, err := s.API.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(s.Bucket), Key: aws.String(key), UploadId: cm.UploadId,
			PartNumber: aws.Int32(n), Body: io.NewSectionReader(f, off, l), ContentLength: aws.Int64(l)})
		if err != nil {
			abort()
			return err
		}
		parts = append(parts, s3types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(n)})
	}
	if _, err := s.API.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(s.Bucket), Key: aws.String(key),
		UploadId: cm.UploadId, MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts}}); err != nil {
		abort()
		return err
	}
	return nil
}

func (s *S3Store) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.API.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(key)})
	if err == nil {
		return true, nil
	}
	var nf *s3types.NotFound
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nf) || errors.As(err, &nsk) {
		return false, nil
	}
	return false, err
}

func (s *S3Store) GetTo(ctx context.Context, key string, w io.Writer) error {
	out, err := s.API.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.Bucket), Key: aws.String(key)})
	if err != nil {
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) {
			return ErrNotFound
		}
		return err
	}
	defer out.Body.Close()
	_, err = io.Copy(w, out.Body)
	return err
}

func (s *S3Store) GetBytes(ctx context.Context, key string) ([]byte, error) {
	var b bytes.Buffer
	if err := s.GetTo(ctx, key, &b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func (s *S3Store) DeletePrefix(ctx context.Context, prefix string) error {
	if prefix == "" || !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("staging: refusing to delete prefix %q", prefix)
	}
	var token *string
	for {
		out, err := s.API.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(s.Bucket), Prefix: aws.String(prefix), ContinuationToken: token})
		if err != nil {
			return err
		}
		if len(out.Contents) > 0 {
			ids := make([]s3types.ObjectIdentifier, 0, len(out.Contents))
			for _, o := range out.Contents {
				ids = append(ids, s3types.ObjectIdentifier{Key: o.Key})
			}
			if _, err := s.API.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(s.Bucket),
				Delete: &s3types.Delete{Objects: ids, Quiet: aws.Bool(true)}}); err != nil {
				return err
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			return nil
		}
		token = out.NextContinuationToken
	}
}

// MemStore is the in-memory fake S3 (tests, and the worker's e2e harness).
type MemStore struct {
	mu   sync.Mutex
	objs map[string][]byte
	// Hook, when set, may mutate bytes as they are read (corruption tests).
	CorruptOnGet func(key string, b []byte) []byte
	PutCount     int
}

func NewMemStore() *MemStore { return &MemStore{objs: map[string][]byte{}} }

func (m *MemStore) PutBytes(_ context.Context, key string, b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = append([]byte(nil), b...)
	m.PutCount++
	return nil
}

func (m *MemStore) PutFile(ctx context.Context, key, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return m.PutBytes(ctx, key, b)
}

func (m *MemStore) GetBytes(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	b = append([]byte(nil), b...)
	if m.CorruptOnGet != nil {
		b = m.CorruptOnGet(key, b)
	}
	return b, nil
}

func (m *MemStore) Exists(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objs[key]
	return ok, nil
}

func (m *MemStore) GetTo(ctx context.Context, key string, w io.Writer) error {
	b, err := m.GetBytes(ctx, key)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func (m *MemStore) DeletePrefix(_ context.Context, prefix string) error {
	if prefix == "" || !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("staging: refusing to delete prefix %q", prefix)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			delete(m.objs, k)
		}
	}
	return nil
}

// Keys returns the stored keys, sorted.
func (m *MemStore) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Set stores bytes directly (simulates the worker's upload).
func (m *MemStore) Set(key string, b []byte) { _ = m.PutBytes(context.Background(), key, b) }
