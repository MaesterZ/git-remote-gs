package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/MaesterZ/git-remote-gs/internal/git"
)

func TestRunUsageAndVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{args: nil, code: exitUsage, stderr: "usage: git-remote-gs <remote-name> gs://BUCKET/PREFIX"},
		{args: []string{"origin"}, code: exitUsage, stderr: "usage:"},
		{args: []string{"--version"}, code: 0, stdout: "git-remote-gs "},
		{args: []string{"origin", "gcs://bucket/repo"}, code: exitFatal, stderr: "fatal: invalid remote URL \"gcs://bucket/repo\""},
		{args: []string{"origin", "gs://B"}, code: exitFatal, stderr: "expected gs://BUCKET/PREFIX"},
		{args: []string{"origin", "gs://bucket"}, code: exitFatal, stderr: "missing repository prefix"},
	}

	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer

			code := run(tt.args, strings.NewReader(""), &stdout, &stderr)
			if code != tt.code {
				t.Errorf("exit code %d, want %d", code, tt.code)
			}

			if !strings.Contains(stdout.String(), tt.stdout) || (tt.stdout == "" && stdout.Len() > 0) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.stdout)
			}

			if !strings.Contains(stderr.String(), tt.stderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.stderr)
			}
		})
	}
}

func TestLockTTL(t *testing.T) {
	t.Parallel()

	for value, want := range map[string]time.Duration{"": time.Minute, "1": time.Second, "300": 5 * time.Minute} {
		got, err := lockTTL(value)
		if err != nil || got != want {
			t.Errorf("lockTTL(%q) = %v, %v; want %v", value, got, err, want)
		}
	}

	for _, value := range []string{"0", "-5", "1m", "abc", "1.5"} {
		_, err := lockTTL(value)
		if !errors.Is(err, errBadTTL) {
			t.Errorf("lockTTL(%q) error = %v, want errBadTTL", value, err)
		}
	}
}

// TestNestedStartRefused isn't parallel: it sets an environment variable.
func TestNestedStartRefused(t *testing.T) {
	t.Setenv(git.NestedEnv, "1")

	var stderr bytes.Buffer

	code := run([]string{"origin", "gs://bucket/repo"}, strings.NewReader(""), io.Discard, &stderr)
	if code != exitFatal || !strings.Contains(stderr.String(), "partial clone: --filter isn't supported") {
		t.Fatalf("run = %d, stderr %q", code, stderr.String())
	}
}
