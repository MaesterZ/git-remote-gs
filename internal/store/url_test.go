package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/MaesterZ/git-remote-gs/internal/store"
)

func TestParseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in     string
		bucket string
		prefix string
		url    string
	}{
		{"gs://bucket/repo", "bucket", "repo", "gs://bucket/repo"},
		{"gs://bucket/team/repo.git/", "bucket", "team/repo.git", "gs://bucket/team/repo.git"},
		{"gs://my.bucket-1_x/a///", "my.bucket-1_x", "a", "gs://my.bucket-1_x/a"},
		{"gs://abc/with space", "abc", "with space", "gs://abc/with space"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			loc, err := store.ParseURL(tt.in)
			if err != nil {
				t.Fatalf("store.ParseURL(%q): %v", tt.in, err)
			}

			if loc.Bucket != tt.bucket || loc.Prefix != tt.prefix || loc.String() != tt.url {
				t.Errorf("store.ParseURL(%q) = %+v (%s), want bucket %q prefix %q (%s)",
					tt.in, loc, loc, tt.bucket, tt.prefix, tt.url)
			}
		})
	}
}

func TestParseURLInvalid(t *testing.T) {
	t.Parallel()

	for _, in := range []string{
		"",
		"gs://",
		"gs:///repo",
		"gs://bucket",
		"gs://bucket/",
		"gs://bucket//",
		"gcs://bucket/repo",
		"s3://bucket/repo",
		"https://storage.googleapis.com/bucket",
		"bucket/repo",
		"gs://Bucket/repo",
		"gs://ab/repo",
		"gs://-bucket/repo",
		"gs://bucket-/repo",
		"gs://bu..cket/repo",
		"gs://" + strings.Repeat("a", 64) + "/repo",
		"gs://bucket/a//b",
		"gs://bucket/./b",
		"gs://bucket/a/../b",
		"gs://bucket/a\nb",
	} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()

			_, err := store.ParseURL(in)
			if !errors.Is(err, store.ErrInvalidURL) {
				t.Fatalf("store.ParseURL(%q) error = %v, want store.ErrInvalidURL", in, err)
			}

			if !strings.Contains(err.Error(), "gs://BUCKET/PREFIX") {
				t.Errorf("error %q doesn't show the expected format", err)
			}
		})
	}
}

func TestLongDottedBucket(t *testing.T) {
	t.Parallel()

	name := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63)

	_, err := store.ParseURL("gs://" + name + "/repo")
	if err != nil {
		t.Fatalf("dotted bucket name: %v", err)
	}
}

func TestLocationKeys(t *testing.T) {
	t.Parallel()

	nested := store.Location{Bucket: "b", Prefix: "x/y"}

	checks := []struct{ got, want string }{
		{nested.Key("refs/heads/main", "LOCK#.lock"), "x/y/refs/heads/main/LOCK#.lock"},
		{nested.Key("HEAD"), "x/y/HEAD"},
		{nested.Key("refs"), "x/y/refs"},
		{nested.URL(nested.Key("HEAD")), "gs://b/x/y/HEAD"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}

	if rel, ok := nested.Rel("x/y/refs/heads/main"); !ok || rel != "refs/heads/main" {
		t.Errorf("Rel = %q, %v", rel, ok)
	}

	if _, ok := nested.Rel("x/yz/refs"); ok {
		t.Error("Rel accepted a name outside the prefix")
	}
}
