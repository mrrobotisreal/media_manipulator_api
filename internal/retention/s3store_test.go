package retention

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type fakeS3API struct {
	in  *s3.DeleteObjectsInput
	out *s3.DeleteObjectsOutput
	err error
}

func (f *fakeS3API) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.in = in
	return f.out, f.err
}

func TestS3StoreReportsPerKeyErrorsAndTreatsNoSuchKeyAsSuccess(t *testing.T) {
	f := &fakeS3API{out: &s3.DeleteObjectsOutput{Errors: []s3types.Error{
		{Key: aws.String("a"), Code: aws.String("NoSuchKey")},
		{Key: aws.String("b"), Code: aws.String("AccessDenied"), Message: aws.String("nope")},
	}}}
	failed, err := NewS3Store(f).DeleteObjects(t.Context(), "bkt", []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed["b"] == nil || failed["a"] != nil {
		t.Errorf("failed = %v", failed)
	}
	if aws.ToString(f.in.Bucket) != "bkt" || len(f.in.Delete.Objects) != 3 || !aws.ToBool(f.in.Delete.Quiet) {
		t.Errorf("request wrong: %+v", f.in)
	}
}

func TestS3StoreWholeCallError(t *testing.T) {
	f := &fakeS3API{err: errors.New("down")}
	if _, err := NewS3Store(f).DeleteObjects(t.Context(), "b", []string{"a"}); err == nil {
		t.Error("whole-call error must surface")
	}
}

func TestNewS3StoreNilClient(t *testing.T) {
	if NewS3Store(nil) != nil {
		t.Error("nil client must give a nil ObjectStore")
	}
}
