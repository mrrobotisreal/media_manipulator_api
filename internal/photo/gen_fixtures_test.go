package photo

import (
	"os"
	"testing"
)

// TestPhotoFixturesGenerate rewrites the canonical fixtures from inputs/ and
// docs/ when PHOTO_FIXTURES_UPDATE=1. Never runs in normal test runs; a fixture
// change must land in both repos (see testdata/README.md).
func TestPhotoFixturesGenerate(t *testing.T) {
	if os.Getenv("PHOTO_FIXTURES_UPDATE") != "1" {
		t.Skip("set PHOTO_FIXTURES_UPDATE=1 to regenerate canonical fixtures")
	}
	for in, out := range inputPairs {
		c, err := Sanitize(read(t, "inputs/"+in+".json"), Limits{})
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile("testdata/docs/"+out+".json", append(c, '\n'), 0o644)
	}
	for _, n := range canonicalDocs {
		c, err := Sanitize(read(t, "docs/"+n+".json"), Limits{})
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile("testdata/docs/"+n+".json", append(c, '\n'), 0o644)
	}
}
