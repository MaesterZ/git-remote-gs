package git_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MaesterZ/git-remote-gs/internal/git"
)

// setupRepo isolates git from the user's config, then creates and enters a repo with two commits on main.
func setupRepo(t *testing.T) (string, string) {
	t.Helper()

	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")

	dir := t.TempDir()
	t.Chdir(dir)

	sh(t, "init", "-q", "-b", "main")
	sh(t, "commit", "-q", "--allow-empty", "-m", "one")
	first := sh(t, "rev-parse", "HEAD")
	sh(t, "commit", "-q", "--allow-empty", "-m", "two")
	second := sh(t, "rev-parse", "HEAD")

	return first, second
}

func sh(t *testing.T, args ...string) string {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return strings.TrimSpace(string(out))
}

//nolint:paralleltest // setupRepo uses t.Setenv and t.Chdir
func TestResolveAndAncestry(t *testing.T) {
	first, second := setupRepo(t)
	sh(t, "tag", "-a", "-m", "annotated", "v1", first)
	tagObj := sh(t, "rev-parse", "v1")

	g := &git.Git{}
	ctx := t.Context()

	for rev, want := range map[string]string{
		"refs/heads/main": second,
		"main":            second,
		first:             first,
		"refs/tags/v1":    tagObj, // not peeled: annotated tags push their tag object
	} {
		got, err := g.Resolve(ctx, rev)
		if err != nil || got != want {
			t.Errorf("Resolve(%s) = %q, %v; want %q", rev, got, err, want)
		}
	}

	_, err := g.Resolve(ctx, "refs/heads/nope")
	if err == nil {
		t.Error("Resolve(missing) succeeded")
	}

	ok, err := g.IsAncestor(ctx, first, second)
	if !ok || err != nil {
		t.Errorf("IsAncestor(first, second) = %v, %v", ok, err)
	}

	ok, err = g.IsAncestor(ctx, second, first)
	if ok || err != nil {
		t.Errorf("IsAncestor(second, first) = %v, %v", ok, err)
	}

	_, err = g.IsAncestor(ctx, strings.Repeat("1", 40), second)
	if err == nil {
		t.Error("IsAncestor with an unknown object succeeded")
	}

	ok, err = g.ObjectExists(ctx, second)
	if !ok || err != nil {
		t.Errorf("ObjectExists(second) = %v, %v", ok, err)
	}

	ok, err = g.ObjectExists(ctx, strings.Repeat("1", 40))
	if ok || err != nil {
		t.Errorf("ObjectExists(unknown) = %v, %v", ok, err)
	}

	unknown := strings.Repeat("2", 40)

	missing, err := g.MissingObjects(ctx, []string{first, unknown, second})
	if err != nil || len(missing) != 1 || !missing[unknown] {
		t.Errorf("MissingObjects = %v, %v; want only %s", missing, err, unknown)
	}

	missing, err = g.MissingObjects(ctx, nil)
	if err != nil || len(missing) != 0 {
		t.Errorf("MissingObjects(nil) = %v, %v", missing, err)
	}
}

func TestBundleRoundTrip(t *testing.T) {
	_, second := setupRepo(t)
	sh(t, "tag", "-a", "-m", "annotated", "v2", second)
	tagObj := sh(t, "rev-parse", "v2")

	g := &git.Git{}
	ctx := t.Context()
	bundles := t.TempDir()

	for _, sha := range []string{second, tagObj} {
		path := filepath.Join(bundles, sha+".bundle")

		err := g.CreateBundle(ctx, sha, path)
		if err != nil {
			t.Fatalf("CreateBundle(%s): %v", sha, err)
		}
	}

	if refs := sh(t, "for-each-ref", git.TmpRefPrefix); refs != "" {
		t.Fatalf("temporary refs left behind: %s", refs)
	}

	// Unbundle into a fresh repo and check the objects arrived.
	t.Chdir(t.TempDir())
	sh(t, "init", "-q")

	for _, sha := range []string{second, tagObj} {
		path := filepath.Join(bundles, sha+".bundle")

		err := g.VerifyBundle(ctx, path)
		if err != nil {
			t.Fatalf("VerifyBundle: %v", err)
		}

		err = g.Unbundle(ctx, path)
		if err != nil {
			t.Fatalf("Unbundle: %v", err)
		}

		if ok, _ := g.ObjectExists(ctx, sha); !ok {
			t.Fatalf("%s missing after unbundle", sha)
		}
	}

	if typ := sh(t, "cat-file", "-t", tagObj); typ != "tag" {
		t.Errorf("tag object type %q", typ)
	}

	bad := filepath.Join(bundles, "bad.bundle")

	err := os.WriteFile(bad, []byte("not a bundle\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = g.VerifyBundle(ctx, bad)
	if err == nil {
		t.Fatal("VerifyBundle accepted garbage")
	}

	if !strings.Contains(err.Error(), "bundle verify") {
		t.Errorf("error %q doesn't name the command", err)
	}
}
