package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/manifest"
)

type imgAPI struct{ tags map[string]string }

func (a imgAPI) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	var out ec2.DescribeImagesOutput
	for _, id := range in.ImageIds {
		out.Images = append(out.Images, types.Image{ImageId: aws.String(id), Tags: []types.Tag{
			{Key: aws.String("runtime-manifest"), Value: aws.String(a.tags[id])}}})
	}
	return &out, nil
}

func TestStatusShowsImagesWithStaleFlag(t *testing.T) {
	var buf bytes.Buffer
	e, _ := testEnv(&buf)
	e.cfg.AMIs = map[string]string{"image:x86_64": "ami-1", "restore:x86_64": "ami-2"}
	e.images = imgAPI{tags: map[string]string{"ami-1": "aaa", "ami-2": "old"}}
	e.host = func(f string) (manifest.Manifest, error) {
		return manifest.Manifest{Family: f, SHA256: map[string]string{"image": "aaa", "restore": "new"}[f]}, nil
	}
	if err := e.status(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"images: 2", "image", "ami-1", "stale: false", "restore", "ami-2", "stale: true"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestManifestPathsNeedsNoFilesystem(t *testing.T) {
	var buf bytes.Buffer
	e := &env{out: &buf}
	if err := e.manifest([]string{"restore", "--paths"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "/opt/media-manipulator-ai/venvs/restore-vsr-mm\t/opt/media-manipulator-ai/venvs/restore-vsr-mm") {
		t.Fatalf("paths output:\n%s", buf.String())
	}
	if err := e.manifest([]string{"nope", "--paths"}); err == nil {
		t.Fatal("unknown family must error")
	}
}
