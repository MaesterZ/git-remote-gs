package store

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	scheme = "gs://"
	// maxBucketLabel is the longest bucket name, or dot-separated part of one.
	maxBucketLabel = 63
)

// ErrInvalidURL is returned for anything that isn't gs://BUCKET/PREFIX.
var ErrInvalidURL = errors.New("invalid remote URL")

// Bucket name rules from https://cloud.google.com/storage/docs/buckets#naming, minus the domain-verification cases.
var bucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,220}[a-z0-9]$`)

// Location is where a repo lives: a bucket and an object name prefix.
type Location struct {
	Bucket string
	Prefix string
}

// ParseURL parses gs://BUCKET/PREFIX[/...]; a trailing slash is ignored. The bucket root isn't a valid repo.
func ParseURL(raw string) (Location, error) {
	invalid := func(why string) error {
		return fmt.Errorf("%w %q: %s, expected gs://BUCKET/PREFIX", ErrInvalidURL, raw, why)
	}

	rest, ok := strings.CutPrefix(raw, scheme)
	if !ok {
		return Location{}, invalid("scheme must be gs://")
	}

	rest = strings.TrimRight(rest, "/")
	bucket, prefix, _ := strings.Cut(rest, "/")

	if !validBucket(bucket) {
		return Location{}, invalid(fmt.Sprintf("bad bucket name %q", bucket))
	}

	if prefix == "" {
		return Location{}, invalid("missing repository prefix: the bucket root can't hold a repo")
	}

	for seg := range strings.SplitSeq(prefix, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return Location{}, invalid(fmt.Sprintf("bad prefix %q", prefix))
		}
	}

	if strings.ContainsFunc(prefix, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return Location{}, invalid("prefix contains control characters")
	}

	return Location{Bucket: bucket, Prefix: prefix}, nil
}

func validBucket(name string) bool {
	if !bucketRe.MatchString(name) || strings.Contains(name, "..") {
		return false
	}

	if !strings.Contains(name, ".") {
		return len(name) <= maxBucketLabel
	}

	for part := range strings.SplitSeq(name, ".") {
		if len(part) > maxBucketLabel {
			return false
		}
	}

	return true
}

// Key joins the prefix and parts into an object name.
func (l Location) Key(parts ...string) string {
	return l.Prefix + "/" + strings.Join(parts, "/")
}

// Rel strips the prefix from an object name, reporting whether the name was under it.
func (l Location) Rel(name string) (string, bool) {
	return strings.CutPrefix(name, l.Prefix+"/")
}

// URL returns the gs:// URL of an object name, for messages and gcloud commands.
func (l Location) URL(name string) string {
	return scheme + l.Bucket + "/" + name
}

// String returns the normalised remote URL.
func (l Location) String() string {
	return scheme + l.Bucket + "/" + l.Prefix
}
