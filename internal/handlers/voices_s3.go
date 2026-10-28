package handlers

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3VoiceObjects stores voice audio under voices/ in the shared bucket (MM77).
type S3VoiceObjects struct {
	Client  *s3.Client
	Presign *s3.PresignClient
	Bucket  string
}

var errVoiceS3Unconfigured = errors.New("voices: s3 is not configured")

// Put uploads a local file and returns its size.
func (o S3VoiceObjects) Put(ctx context.Context, key, localPath, contentType string) (int64, error) {
	if o.Client == nil || o.Bucket == "" {
		return 0, errVoiceS3Unconfigured
	}
	f, err := os.Open(localPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	_, err = o.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(o.Bucket), Key: aws.String(key), Body: f,
		ContentType: aws.String(contentType), ContentLength: aws.Int64(st.Size()),
	})
	return st.Size(), err
}

// Fetch downloads an object to localPath.
func (o S3VoiceObjects) Fetch(ctx context.Context, key, localPath string) error {
	if o.Client == nil || o.Bucket == "" {
		return errVoiceS3Unconfigured
	}
	out, err := o.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(o.Bucket), Key: aws.String(key)})
	if err != nil {
		return err
	}
	defer out.Body.Close()
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, out.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Delete removes one object (idempotent in S3).
func (o S3VoiceObjects) Delete(ctx context.Context, key string) error {
	if o.Client == nil || o.Bucket == "" {
		return errVoiceS3Unconfigured
	}
	_, err := o.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(o.Bucket), Key: aws.String(key)})
	return err
}

// PresignGet signs a short-lived GET (the voice preview player).
func (o S3VoiceObjects) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if o.Presign == nil || o.Bucket == "" {
		return "", errVoiceS3Unconfigured
	}
	req, err := o.Presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(o.Bucket), Key: aws.String(key)},
		func(p *s3.PresignOptions) { p.Expires = ttl })
	if err != nil {
		return "", err
	}
	return req.URL, nil
}
