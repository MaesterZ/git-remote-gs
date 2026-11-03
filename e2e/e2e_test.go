// Package e2e_test builds git-remote-gs and drives real git against it, using an in-process fake GCS server.
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/MaesterZ/git-remote-gs/internal/store"
	"github.com/fsouza/fake-gcs-server/fakestorage"
)

var (
	binDir       string
	gitConfig    string
	fake         *fakestorage.Server
	emulatorHost string
	fakeClient   *storage.Client
	bucketSeq    atomic.Int64
)

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	tmp, err := os.MkdirTemp("", "git-remote-gs-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}
	defer os.RemoveAll(tmp)

	binDir = filepath.Join(tmp, "bin")

	bin := filepath.Join(binDir, "git-remote-gs")
	// Same tag as the Makefile and releases, so the tests exercise the binary that ships
	build := exec.CommandContext(context.Background(), "go", "build", "-tags", "disable_grpc_modules", "-o", bin, "..")

	out, err := build.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build git-remote-gs: %v\n%s", err, out)

		return 1
	}

	gitConfig = filepath.Join(tmp, "gitconfig")

	cfg := "[user]\n\tname = E2E\n\temail = e2e@example.com\n[init]\n\tdefaultBranch = main\n[advice]\n\tdetachedHead = false\n"

	err = os.WriteFile(gitConfig, []byte(cfg), 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}

	// PublicHost makes the fake route XML-API reads (http://127.0.0.1:port/bucket/object) by path, not by Host header.
	opts := fakestorage.Options{Scheme: "http", Host: "127.0.0.1", PublicHost: "127.0.0.1"}

	fake, err = fakestorage.NewServerWithOptions(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start fake GCS: %v\n", err)

		return 1
	}
	defer fake.Stop()

	fakeURL, err := url.Parse(fake.URL())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}

	emulatorHost = fakeURL.Host

	// Inspect the bucket with a client built like the binary's; the env var is read only by NewClient.
	_ = os.Setenv("STORAGE_EMULATOR_HOST", emulatorHost)
	fakeClient, err = storage.NewClient(context.Background())

	_ = os.Unsetenv("STORAGE_EMULATOR_HOST")

	if err != nil {
		fmt.Fprintf(os.Stderr, "fake GCS client: %v\n", err)

		return 1
	}

	defer fakeClient.Close()

	return m.Run()
}

// remote is one repo location plus a store client to inspect and tamper with it.
type remote struct {
	t     *testing.T
	url   string
	loc   store.Location
	store store.Store
	env   []string
}

// baseEnv isolates git from the user's configuration and puts the freshly built helper first on PATH.
func baseEnv() []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "GIT_") || strings.HasPrefix(kv, "STORAGE_EMULATOR_HOST=")
	})

	return append(env,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL="+gitConfig,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	)
}

// newFakeRemote gives each test its own bucket on the shared fake server.
func newFakeRemote(t *testing.T) *remote {
	t.Helper()

	name := strings.ToLower(regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(t.Name(), "-"))
	bucket := fmt.Sprintf("e2e-%d-%.40s", bucketSeq.Add(1), name)
	bucket = strings.TrimRight(bucket, "-")
	fake.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: bucket})

	loc := store.Location{Bucket: bucket, Prefix: "team/repo"}

	return &remote{
		t:     t,
		url:   loc.String(),
		loc:   loc,
		store: store.NewGCSWithClient(fakeClient, bucket),
		env:   append(baseEnv(), "STORAGE_EMULATOR_HOST="+emulatorHost),
	}
}

func (r *remote) cmd(dir string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(r.t.Context(), "git", args...)
	cmd.Dir = dir

	cmd.Env = append(slices.Clone(r.env), extraEnv...)

	out, err := cmd.CombinedOutput()

	return strings.TrimSpace(string(out)), err
}

// git runs git in dir and fails the test on error.
func (r *remote) git(dir string, args ...string) string {
	r.t.Helper()

	out, err := r.cmd(dir, nil, args...)
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return out
}

// gitFails runs git in dir and fails the test unless it fails.
func (r *remote) gitFails(dir string, args ...string) string {
	r.t.Helper()

	out, err := r.cmd(dir, nil, args...)
	if err == nil {
		r.t.Fatalf("git %s succeeded, want failure\n%s", strings.Join(args, " "), out)
	}

	return out
}

// newRepo creates a repo with one commit and origin pointing at the remote.
func (r *remote) newRepo() string {
	r.t.Helper()

	dir := r.t.TempDir()
	r.git(dir, "init", "-q")
	r.commit(dir, "README", "one\n")
	r.git(dir, "remote", "add", "origin", r.url)

	return dir
}

func (r *remote) clone() string {
	r.t.Helper()

	dir := filepath.Join(r.t.TempDir(), "clone")
	r.git("", "clone", "-q", r.url, dir)

	return dir
}

func (r *remote) commit(dir, file, content string) string {
	r.t.Helper()

	err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600)
	if err != nil {
		r.t.Fatal(err)
	}

	r.git(dir, "add", file)
	r.git(dir, "commit", "-q", "-m", "update "+file)

	return r.git(dir, "rev-parse", "HEAD")
}

func (r *remote) fsck(dirs ...string) {
	r.t.Helper()

	for _, dir := range dirs {
		r.git(dir, "fsck", "--full", "--strict")
	}
}

// objects lists object names under the repo prefix, relative to it.
func (r *remote) objects() []string {
	r.t.Helper()

	objs, err := r.store.List(r.t.Context(), r.loc.Key(""))
	if err != nil {
		r.t.Fatalf("list bucket: %v", err)
	}

	names := make([]string, 0, len(objs))
	for _, o := range objs {
		rel, _ := r.loc.Rel(o.Name)
		names = append(names, rel)
	}

	return names
}

func (r *remote) read(rel string) (string, bool) {
	r.t.Helper()

	var buf bytes.Buffer

	_, err := r.store.Read(r.t.Context(), r.loc.Key(rel), &buf)

	return buf.String(), err == nil
}

// bundles returns the SHAs of the bundles stored for ref.
func (r *remote) bundles(ref string) []string {
	r.t.Helper()

	var shas []string

	for _, name := range r.objects() {
		if rest, ok := strings.CutPrefix(name, ref+"/"); ok && strings.HasSuffix(rest, ".bundle") {
			shas = append(shas, strings.TrimSuffix(rest, ".bundle"))
		}
	}

	return shas
}

func (r *remote) assertRef(ref, sha string) {
	r.t.Helper()

	if got := r.bundles(ref); !slices.Equal(got, []string{sha}) {
		r.t.Fatalf("bundles of %s = %v, want exactly [%s]\nbucket: %v", ref, got, sha, r.objects())
	}

	if _, locked := r.read(ref + "/LOCK#.lock"); locked {
		r.t.Fatalf("%s is still locked", ref)
	}
}

func (r *remote) putLock(ref string) {
	r.t.Helper()

	body, err := json.Marshal(map[string]any{"timestamp": time.Now().Unix(), "host": "elsewhere", "pid": 1})
	if err != nil {
		r.t.Fatal(err)
	}

	_, err = r.store.WriteIfAbsent(r.t.Context(), r.loc.Key(ref, "LOCK#.lock"), bytes.NewReader(body), "application/json")
	if err != nil {
		r.t.Fatalf("plant lock: %v", err)
	}
}

// roundTrip covers init, push, clone, push from the clone, pull, tags, raw SHAs and deletes; it also runs on a real bucket.
func roundTrip(t *testing.T, r *remote) {
	t.Helper()

	a := r.newRepo()
	first := r.git(a, "rev-parse", "HEAD")
	r.git(a, "push", "-q", "-u", "origin", "main")

	if head, _ := r.read("HEAD"); head != "refs/heads/main" {
		t.Fatalf("HEAD object = %q", head)
	}

	r.assertRef("refs/heads/main", first)

	if got := r.git(a, "ls-remote", "--symref", "origin", "HEAD"); !strings.HasPrefix(got, "ref: refs/heads/main\tHEAD") {
		t.Errorf("ls-remote --symref = %q", got)
	}

	b := r.clone()
	if branch := r.git(b, "symbolic-ref", "--short", "HEAD"); branch != "main" {
		t.Errorf("clone checked out %q", branch)
	}

	if data, _ := os.ReadFile(filepath.Join(b, "README")); string(data) != "one\n" {
		t.Errorf("clone README = %q", data)
	}

	second := r.commit(b, "README", "two\n")
	r.git(b, "push", "-q", "origin", "main")
	r.assertRef("refs/heads/main", second)

	r.git(a, "pull", "-q", "--ff-only")

	if got := r.git(a, "rev-parse", "HEAD"); got != second {
		t.Errorf("after pull HEAD = %s, want %s", got, second)
	}

	// Tags: lightweight on the first commit, annotated on the second.
	r.git(a, "tag", "light", first)
	r.git(a, "tag", "-a", "-m", "release", "v1")
	tagObj := r.git(a, "rev-parse", "v1")
	r.git(a, "push", "-q", "origin", "--tags")
	r.assertRef("refs/tags/light", first)
	r.assertRef("refs/tags/v1", tagObj)

	c := r.clone()
	if tags := r.git(c, "tag", "-l"); tags != "light\nv1" {
		t.Errorf("cloned tags = %q", tags)
	}

	if typ := r.git(c, "cat-file", "-t", "v1"); typ != "tag" {
		t.Errorf("v1 type %q, want an annotated tag", typ)
	}

	if peeled := r.git(c, "rev-parse", "v1^{commit}"); peeled != second {
		t.Errorf("v1 peels to %s, want %s", peeled, second)
	}

	// A raw SHA source needs a temporary ref to bundle.
	r.git(a, "push", "-q", "origin", first+":refs/heads/from-sha")
	r.assertRef("refs/heads/from-sha", first)

	if got := r.git(c, "ls-remote", "origin", "refs/heads/from-sha"); got != first+"\trefs/heads/from-sha" {
		t.Errorf("ls-remote from-sha = %q", got)
	}

	if refs := r.git(a, "for-each-ref", "refs/git-remote-gs/"); refs != "" {
		t.Errorf("temporary refs left behind: %s", refs)
	}

	// Branch delete, and refusal to delete the HEAD branch.
	r.git(a, "push", "-q", "origin", "--delete", "from-sha")

	if got := r.bundles("refs/heads/from-sha"); len(got) != 0 {
		t.Errorf("deleted branch still has bundles %v", got)
	}

	out := r.gitFails(a, "push", "origin", "--delete", "main")
	if !strings.Contains(out, "refusing to delete the remote HEAD branch") {
		t.Errorf("deleting main: %s", out)
	}

	r.assertRef("refs/heads/main", second)
	r.fsck(a, b, c)
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	roundTrip(t, newFakeRemote(t))
}

func TestProtectedBranch(t *testing.T) {
	t.Parallel()

	r := newFakeRemote(t)
	a := r.newRepo()
	r.git(a, "push", "-q", "-u", "origin", "main")
	r.git(a, "push", "-q", "origin", "main:refs/heads/release")

	// As `git-s3 protect` or `gcloud storage cp /dev/null 'gs://.../refs/heads/release/PROTECTED#'` would.
	_, err := r.store.WriteIfAbsent(r.t.Context(), r.loc.Key("refs/heads/release", "PROTECTED#"), strings.NewReader(""), "")
	if err != nil {
		t.Fatal(err)
	}

	base := r.git(a, "rev-parse", "HEAD")
	ahead := r.commit(a, "x", "1\n")

	// Fast-forwards still go through, even with --force.
	r.git(a, "push", "-q", "--force", "origin", "main:release")
	r.assertRef("refs/heads/release", ahead)

	r.git(a, "reset", "-q", "--hard", base)
	r.commit(a, "y", "rewritten\n")

	out := r.gitFails(a, "push", "--force", "origin", "HEAD:release")
	if !strings.Contains(out, "protected branch, force push not allowed") {
		t.Errorf("forced rewrite of a protected branch: %s", out)
	}

	out = r.gitFails(a, "push", "origin", "--delete", "release")
	if !strings.Contains(out, "refusing to delete a protected branch") {
		t.Errorf("deleting a protected branch: %s", out)
	}

	r.assertRef("refs/heads/release", ahead)
}

func TestNonFastForwardAndForce(t *testing.T) {
	t.Parallel()

	r := newFakeRemote(t)
	a := r.newRepo()
	r.git(a, "push", "-q", "-u", "origin", "main")
	b := r.clone()

	theirs := r.commit(a, "a.txt", "a\n")
	r.git(a, "push", "-q", "origin", "main")

	mine := r.commit(b, "b.txt", "b\n")
	r.git(b, "fetch", "-q")

	out := r.gitFails(b, "push", "origin", "main")
	if !strings.Contains(out, "rejected") {
		t.Errorf("non-fast-forward push: %s", out)
	}

	r.assertRef("refs/heads/main", theirs)

	r.git(b, "push", "-q", "--force", "origin", "main")
	r.assertRef("refs/heads/main", mine)

	r.git(a, "fetch", "-q")

	if got := r.git(a, "rev-parse", "origin/main"); got != mine {
		t.Errorf("origin/main = %s after force push, want %s", got, mine)
	}

	r.fsck(a, b)
}

func TestLocks(t *testing.T) {
	t.Parallel()

	r := newFakeRemote(t)
	a := r.newRepo()
	r.git(a, "push", "-q", "-u", "origin", "main")

	// A lock older than the TTL is taken over. Its age is its object's time, which the fake can't backdate: wait instead.
	r.putLock("refs/heads/main")
	time.Sleep(1500 * time.Millisecond)

	sha := r.commit(a, "x", "1\n")

	out, err := r.cmd(a, []string{"GIT_REMOTE_GS_LOCK_TTL=1"}, "push", "-q", "origin", "main")
	if err != nil {
		t.Fatalf("push over a stale lock: %v\n%s", err, out)
	}

	r.assertRef("refs/heads/main", sha)

	// A fresh lock blocks the push and is left alone.
	r.putLock("refs/heads/main")
	r.commit(a, "x", "2\n")

	out = r.gitFails(a, "push", "origin", "main")
	lockURL := r.loc.URL(r.loc.Key("refs/heads/main", "LOCK#.lock"))

	for _, want := range []string{"could not lock " + lockURL, "held by elsewhere", "gcloud storage rm '" + lockURL + "'"} {
		if !strings.Contains(out, want) {
			t.Errorf("blocked push output lacks %q:\n%s", want, out)
		}
	}

	if body, ok := r.read("refs/heads/main/LOCK#.lock"); !ok || !strings.Contains(body, "elsewhere") {
		t.Errorf("the other client's lock was touched: %q, %v", body, ok)
	}

	if got := r.bundles("refs/heads/main"); !slices.Equal(got, []string{sha}) {
		t.Errorf("bundles = %v, want unchanged [%s]", got, sha)
	}
}

func TestConcurrentPushes(t *testing.T) {
	t.Parallel()

	r := newFakeRemote(t)
	a := r.newRepo()
	r.git(a, "push", "-q", "origin", "main")

	for round := range 3 {
		clones := []string{r.clone(), r.clone()}
		heads := make([]string, len(clones))

		for i, dir := range clones {
			heads[i] = r.commit(dir, fmt.Sprintf("f%d", i), fmt.Sprintf("round %d clone %d\n", round, i))
		}

		var (
			wg   sync.WaitGroup
			errs = make([]error, len(clones))
			outs = make([]string, len(clones))
		)

		for i, dir := range clones {
			wg.Go(func() { outs[i], errs[i] = r.cmd(dir, nil, "push", "origin", "main") })
		}

		wg.Wait()

		var winners []int

		for i, err := range errs {
			if err == nil {
				winners = append(winners, i)
			}
		}

		if len(winners) != 1 {
			t.Fatalf("round %d: %d pushes succeeded, want 1\n%s\n---\n%s", round, len(winners), outs[0], outs[1])
		}

		t.Logf("round %d: loser said: %s", round, outs[1-winners[0]])
		r.assertRef("refs/heads/main", heads[winners[0]])
		r.fsck(clones...)
	}
}

func TestEmptyClone(t *testing.T) {
	t.Parallel()

	r := newFakeRemote(t)
	dir := filepath.Join(t.TempDir(), "empty")

	out := r.git("", "clone", r.url, dir)
	if !strings.Contains(out, "empty repository") {
		t.Errorf("clone of an empty remote: %s", out)
	}
}

func TestFatalErrors(t *testing.T) {
	t.Parallel()

	r := newFakeRemote(t)

	tests := map[string]struct {
		url  string
		want string
	}{
		"invalid URL":    {"gs://Not_A_Bucket/repo", "fatal: invalid remote URL \"gs://Not_A_Bucket/repo\": bad bucket name"},
		"bucket root":    {"gs://" + r.loc.Bucket + "/", "missing repository prefix"},
		"missing bucket": {"gs://no-such-bucket-e2e/repo", "fatal: bucket not found: gs://no-such-bucket-e2e"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			out := r.gitFails("", "ls-remote", tt.url)
			if !strings.Contains(out, tt.want) {
				t.Errorf("output lacks %q:\n%s", tt.want, out)
			}
		})
	}
}

// TestPartialClone: git asks for --filter anyway; the helper's own git commands must not lazy-fetch through it again.
func TestPartialClone(t *testing.T) {
	t.Parallel()

	r := newFakeRemote(t)
	a := r.newRepo()
	r.commit(a, "x", "1\n")
	r.git(a, "push", "-q", "origin", "main")

	dir := filepath.Join(t.TempDir(), "partial")

	trace := filepath.Join(t.TempDir(), "trace") // a file collects traces from nested processes too, unlike stderr

	out, err := r.cmd("", []string{"GIT_TRACE=" + trace}, "clone", "-q", "--filter=blob:none", r.url, dir)
	if err != nil {
		t.Fatalf("partial clone: %v\n%s", err, out)
	}

	traced, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}

	if starts := strings.Count(string(traced), "trace: exec: git-remote-gs "); starts != 1 {
		t.Errorf("the helper started %d times, want 1: its git commands lazy-fetched through it", starts)
	}

	if missing := r.git(dir, "rev-list", "--objects", "--missing=print", "HEAD"); strings.Contains(missing, "?") {
		t.Errorf("the filter should be ignored, but objects are missing:\n%s", missing)
	}

	sha := r.commit(dir, "x", "2\n")
	r.git(dir, "push", "-q", "origin", "main")
	r.assertRef("refs/heads/main", sha)
	r.fsck(dir)
}

func TestSHA256Rejected(t *testing.T) {
	t.Parallel()

	r := newFakeRemote(t)
	dir := filepath.Join(t.TempDir(), "sha256")
	r.git("", "init", "-q", "-b", "main", "--object-format=sha256", dir)
	r.commit(dir, "x", "1\n")

	out := r.gitFails(dir, "push", r.url, "main")
	if !strings.Contains(out, "SHA-256 repositories aren't supported") {
		t.Errorf("push of a SHA-256 repo: %s", out)
	}

	if names := r.objects(); len(names) != 0 {
		t.Errorf("rejected push wrote %v", names)
	}
}

func TestVerboseLogging(t *testing.T) {
	t.Parallel()

	r := newFakeRemote(t)
	a := r.newRepo()

	quiet, err := r.cmd(a, nil, "push", "origin", "main")
	if err != nil || strings.Contains(quiet, "git-remote-gs:") {
		t.Errorf("quiet push logged (err=%v):\n%s", err, quiet)
	}

	r.commit(a, "x", "1\n")

	verbose, err := r.cmd(a, []string{"GIT_REMOTE_GS_VERBOSE=anything"}, "push", "origin", "main")
	if err != nil || !strings.Contains(verbose, "git-remote-gs: upload ") {
		t.Errorf("verbose push didn't log uploads (err=%v):\n%s", err, verbose)
	}

	r.commit(a, "x", "2\n")

	dashV, err := r.cmd(a, nil, "push", "-v", "origin", "main")
	if err != nil || !strings.Contains(dashV, "git-remote-gs: upload ") {
		t.Errorf("git push -v didn't turn on logging (err=%v):\n%s", err, dashV)
	}
}

// TestNoCredentials runs without an emulator or ADC: the helper must fail with the one-line hint, before any network call.
func TestNoCredentials(t *testing.T) {
	t.Parallel()

	empty := t.TempDir()
	r := &remote{t: t, env: append(baseEnv(),
		"HOME="+empty,
		"CLOUDSDK_CONFIG="+empty,
		"GOOGLE_APPLICATION_CREDENTIALS=",
	)}

	out := r.gitFails("", "ls-remote", "gs://some-bucket/repo")
	if !strings.Contains(out, "fatal: no Google credentials: run gcloud auth application-default login\n") &&
		!strings.HasSuffix(out, "fatal: no Google credentials: run gcloud auth application-default login") {
		t.Fatalf("output lacks the credentials line:\n%s", out)
	}

	// git adds its own "fatal: remote helper 'gs' aborted session"; the helper's part must be the one short line.
	if strings.Count(out, "no Google credentials") != 1 || strings.Contains(out, "could not find default credentials") {
		t.Errorf("want exactly one short credentials line:\n%s", out)
	}
}

// TestRealBucket runs the round trip against GIT_REMOTE_GS_TEST_BUCKET under a unique prefix, then deletes what it wrote.
func TestRealBucket(t *testing.T) {
	t.Parallel()

	target := os.Getenv("GIT_REMOTE_GS_TEST_BUCKET")
	if target == "" {
		t.Skip("set GIT_REMOTE_GS_TEST_BUCKET=gs://BUCKET[/PREFIX] to run against a real bucket")
	}

	run := fmt.Sprintf("git-remote-gs-e2e-%d-%04d", time.Now().Unix(), rand.IntN(10000)) //nolint:gosec // not security

	loc, err := store.ParseURL(strings.TrimRight(target, "/") + "/" + run)
	if err != nil {
		t.Fatal(err)
	}

	gcs, err := store.NewGCS(t.Context(), loc.Bucket)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())

		objs, err := gcs.List(ctx, loc.Prefix+"/")
		if err != nil {
			t.Errorf("cleanup: %v", err)
		}

		for _, o := range objs {
			err := gcs.DeleteIfGeneration(ctx, o.Name, o.Generation)
			if err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}

		_ = gcs.Close()
	})

	r := &remote{t: t, url: loc.String(), loc: loc, store: gcs, env: baseEnv()}
	t.Logf("testing against %s", r.url)
	roundTrip(t, r)
}
