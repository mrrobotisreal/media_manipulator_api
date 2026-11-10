package burst

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// BinDir is the burst/bin/<sha256>/burst-worker directory (MM125); the reaper
// must never treat it as a run's staging prefix.
const BinDir = "bin"

// BuildDir is burst/build/<date>/ (MM126): image-builder bundles and smoke
// results written by the builder instance. Also never a run prefix.
const BuildDir = "build"

// PrefixStore deletes and lists the transient staging prefixes
// (s3://<bucket>/burst/<runId>/).
type PrefixStore interface {
	// DeleteRun removes every object under <prefix><runID>/.
	DeleteRun(ctx context.Context, runID string) error
	// ListRunIDs returns the run ids that currently have a staging prefix.
	ListRunIDs(ctx context.Context) ([]string, error)
}

// S3API is the slice of *s3.Client the staging store uses.
type S3API interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, opts ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// S3Staging implements PrefixStore on the existing S3 client.
type S3Staging struct {
	API    S3API
	Bucket string
	Prefix string // "burst/"
}

func (s *S3Staging) prefix() string {
	p := strings.Trim(s.Prefix, "/")
	if p == "" {
		p = "burst"
	}
	return p + "/"
}

func (s *S3Staging) DeleteRun(ctx context.Context, runID string) error {
	if runID == "" || runID == BinDir || runID == BuildDir || strings.ContainsAny(runID, "/.") {
		return nil // never build a prefix that could reach beyond one run
	}
	pfx := s.prefix() + runID + "/"
	var token *string
	for {
		out, err := s.API.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(s.Bucket), Prefix: aws.String(pfx), ContinuationToken: token})
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

func (s *S3Staging) ListRunIDs(ctx context.Context) ([]string, error) {
	var ids []string
	var token *string
	for {
		out, err := s.API.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(s.Bucket), Prefix: aws.String(s.prefix()),
			Delimiter: aws.String("/"), ContinuationToken: token})
		if err != nil {
			return nil, err
		}
		for _, cp := range out.CommonPrefixes {
			id := strings.Trim(strings.TrimPrefix(aws.ToString(cp.Prefix), s.prefix()), "/")
			if id != "" && id != BinDir && id != BuildDir { // burst/bin/ holds worker builds, not a run
				ids = append(ids, id)
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			return ids, nil
		}
		token = out.NextContinuationToken
	}
}
