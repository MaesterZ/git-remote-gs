// Package git wraps the git commands the remote helper runs, in the repository git started it in.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
)

// TmpRefPrefix is where CreateBundle points a temporary ref, since git bundle create needs a ref name, not a bare SHA.
const TmpRefPrefix = "refs/git-remote-gs/tmp/"

// NestedEnv is set in the environment of every git command the helper runs, so the helper can tell if one started it.
const NestedEnv = "GIT_REMOTE_GS_NESTED"

// exitFalse is the exit code git uses for a "no" answer, e.g. merge-base --is-ancestor.
const exitFalse = 1

// Error is a failed git command, with its stderr.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("git %s: %v", strings.Join(e.Args, " "), e.Err)
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}

	return msg
}

// Unwrap returns the exec error.
func (e *Error) Unwrap() error { return e.Err }

// Git runs git commands. Logf, if set, receives one line per command.
type Git struct {
	Logf    func(format string, args ...any)
	counter atomic.Int64
}

// ObjectExists reports whether the object is in the local repository.
func (g *Git) ObjectExists(ctx context.Context, sha string) (bool, error) {
	_, err := g.run(ctx, "cat-file", "-e", sha)
	if err == nil {
		return true, nil
	}

	if exitCode(err) > 0 {
		return false, nil
	}

	return false, err
}

// MissingObjects returns which of shas aren't in the local repository, using a single git process.
func (g *Git) MissingObjects(ctx context.Context, shas []string) (map[string]bool, error) {
	missing := map[string]bool{}
	if len(shas) == 0 {
		return missing, nil
	}

	out, err := g.runInput(ctx, strings.Join(shas, "\n")+"\n", "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}

	// One line per input: "<sha> <type> <size>", or "<sha> missing".
	for line := range strings.Lines(out) {
		name, info, _ := strings.Cut(strings.TrimSpace(line), " ")
		if info == "missing" {
			missing[name] = true
		}
	}

	return missing, nil
}

// Resolve returns the object name rev points to, without peeling tags.
func (g *Git) Resolve(ctx context.Context, rev string) (string, error) {
	out, err := g.run(ctx, "rev-parse", "--verify", "--end-of-options", rev+"^{object}")
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(out), nil
}

// IsAncestor reports whether ancestor is an ancestor of (or equal to) descendant.
func (g *Git) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	_, err := g.run(ctx, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}

	if exitCode(err) == exitFalse {
		return false, nil
	}

	return false, err
}

// CreateBundle writes a full bundle of sha to path via a temporary ref, deleted afterwards.
func (g *Git) CreateBundle(ctx context.Context, sha, path string) (err error) {
	ref := fmt.Sprintf("%s%d-%d", TmpRefPrefix, os.Getpid(), g.counter.Add(1))

	// The empty old value makes update-ref fail if the ref already exists.
	_, err = g.run(ctx, "update-ref", "--no-deref", ref, sha, "")
	if err != nil {
		return err
	}

	defer func() {
		_, delErr := g.run(context.WithoutCancel(ctx), "update-ref", "--no-deref", "-d", ref, sha)
		err = errors.Join(err, delErr)
	}()

	_, err = g.run(ctx, "bundle", "create", "--quiet", path, ref)

	return err
}

// VerifyBundle checks the bundle is valid and its prerequisites, if any, are present.
func (g *Git) VerifyBundle(ctx context.Context, path string) error {
	_, err := g.run(ctx, "bundle", "verify", "--quiet", path)

	return err
}

// Unbundle stores the bundle's objects in the repository; it doesn't update any ref.
func (g *Git) Unbundle(ctx context.Context, path string) error {
	_, err := g.run(ctx, "bundle", "unbundle", path)

	return err
}

func (g *Git) run(ctx context.Context, args ...string) (string, error) {
	return g.runInput(ctx, "", args...)
}

func (g *Git) runInput(ctx context.Context, stdin string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdin = strings.NewReader(stdin)
	// In a partial clone, a missing object would make git fetch it through this helper again, recursively.
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1", NestedEnv+"=1")
	cmd.Stdout = &stdout // never inherit: the helper's stdout carries only protocol output
	cmd.Stderr = &stderr

	err := cmd.Run()
	if g.Logf != nil {
		g.Logf("git %s (err=%v)", strings.Join(args, " "), err)
	}

	if err != nil {
		return stdout.String(), &Error{Args: args, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}

	return stdout.String(), nil
}

// exitCode returns the exit code of a git command that ran and failed, or -1.
func exitCode(err error) int {
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}

	return -1
}
