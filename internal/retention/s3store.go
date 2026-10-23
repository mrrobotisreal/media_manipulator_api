package retention

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3DeleteAPI is the one S3 call the sweeper needs (*s3.Client satisfies it).
type S3DeleteAPI interface {
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// S3Store adapts an S3 client to ObjectStore. A nil client yields a nil
// ObjectStore via NewS3Store so "S3 not configured" stays one check.
type S3Store struct{ api S3DeleteAPI }

// NewS3Store returns nil when api is nil.
func NewS3Store(api S3DeleteAPI) ObjectStore {
	if api == nil {
		return nil
	}
	return &S3Store{api: api}
}

// DeleteObjects deletes up to 1000 keys in quiet mode and reports per-key errors.
// NoSuchKey counts as success (S3 itself reports a missing key as deleted).
func (s *S3Store) DeleteObjects(ctx context.Context, bucket string, keys []string) (map[string]error, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	ids := make([]s3types.ObjectIdentifier, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, s3types.ObjectIdentifier{Key: aws.String(k)})
	}
	out, err := s.api.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(bucket),
		Delete: &s3types.Delete{Objects: ids, Quiet: aws.Bool(true)},
	})
	if err != nil {
		return nil, err
	}
	var failed map[string]error
	for _, e := range out.Errors {
		if aws.ToString(e.Code) == "NoSuchKey" {
			continue
		}
		if failed == nil {
			failed = map[string]error{}
		}
		failed[aws.ToString(e.Key)] = errors.New(aws.ToString(e.Code) + ": " + aws.ToString(e.Message))
	}
	return failed, nil
}
