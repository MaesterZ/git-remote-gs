package protocol //nolint:testpackage // white-box: exercises unexported parsing, locking and limits

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MaesterZ/git-remote-gs/internal/store"
)

var errFake = errors.New("fake git error")

// sha builds a recognisable 40-hex object name from a short label.
func sha(label string) string {
	return fmt.Sprintf("%040x", []byte(label))
}

// fakeGit models a repository with linear commit ancestry; bundles are files holding "bundle <sha>".
type fakeGit struct {
	mu      sync.Mutex
	objects map[string]bool
	refs    map[string]string
	parent  map[string]string // shared ancestry, as if every repo had the same history
	created []string
	failRun map[string]error
}

func newFakeGit() *fakeGit {
	return &fakeGit{objects: map[string]bool{}, refs: map[string]string{}, parent: map[string]string{}, failRun: map[string]error{}}
}

func (g *fakeGit) ObjectExists(_ context.Context, s string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.objects[s], nil
}

func (g *fakeGit) MissingObjects(_ context.Context, shas []string) (map[string]bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	missing := map[string]bool{}

	for _, s := range shas {
		if !g.objects[s] {
			missing[s] = true
		}
	}

	return missing, nil
}

func (g *fakeGit) Resolve(_ context.Context, rev string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if s, ok := g.refs[rev]; ok {
		return s, nil
	}

	if g.objects[rev] {
		return rev, nil
	}

	return "", fmt.Errorf("%w: unknown revision %s", errFake, rev)
}

func (g *fakeGit) IsAncestor(_ context.Context, ancestor, descendant string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.objects[ancestor] || !g.objects[descendant] {
		return false, fmt.Errorf("%w: bad object", errFake)
	}

	for c := descendant; c != ""; c = g.parent[c] {
		if c == ancestor {
			return true, nil
		}
	}

	return false, nil
}

func (g *fakeGit) CreateBundle(_ context.Context, s, path string) error {
	g.mu.Lock()
	g.created = append(g.created, s)
	g.mu.Unlock()

	return os.WriteFile(path, []byte("bundle "+s), 0o600) //nolint:wrapcheck // test fake
}

func (g *fakeGit) VerifyBundle(_ context.Context, path string) error {
	_, err := g.readBundle(path)

	return err
}

func (g *fakeGit) Unbundle(_ context.Context, path string) error {
	s, err := g.readBundle(path)
	if err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	err = g.failRun["unbundle"]
	if err != nil {
		return err
	}

	for c := s; c != ""; c = g.parent[c] {
		g.objects[c] = true
	}

	return nil
}

// commit records child with the given parent and makes it a local object.
func (g *fakeGit) commit(child, parent string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.objects[child] = true
	if parent != "" {
		g.parent[child] = parent
	}
}

func (g *fakeGit) readBundle(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err //nolint:wrapcheck // test fake
	}

	s, ok := strings.CutPrefix(string(data), "bundle ")
	if !ok {
		return "", fmt.Errorf("%w: %s is not a bundle", errFake, path)
	}

	return s, nil
}

// hookStore lets a test act at precise points, as a concurrent client would, and records reads.
type hookStore struct {
	*store.Memory

	beforeWrite func(name string)

	mu    sync.Mutex
	reads []string
}

func (h *hookStore) Read(ctx context.Context, name string, w io.Writer) (store.Object, error) {
	h.record("read " + name)

	return h.Memory.Read(ctx, name, w) //nolint:wrapcheck // test wrapper
}

func (h *hookStore) ReadRange(ctx context.Context, name string, gen, offset, length int64, w io.Writer) error {
	h.record("range " + name)

	return h.Memory.ReadRange(ctx, name, gen, offset, length, w) //nolint:wrapcheck // test wrapper
}

func (h *hookStore) WriteIfAbsent(ctx context.Context, name string, r io.Reader, ct string) (store.Object, error) {
	if h.beforeWrite != nil {
		h.beforeWrite(name)
	}

	return h.Memory.WriteIfAbsent(ctx, name, r, ct) //nolint:wrapcheck // test wrapper
}

func (h *hookStore) record(entry string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.reads = append(h.reads, entry)
}

// readsOf counts reads of names ending in suffix, by kind ("read" or "range").
func (h *hookStore) readsOf(kind, suffix string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	n := 0

	for _, entry := range h.reads {
		if strings.HasPrefix(entry, kind+" ") && strings.HasSuffix(entry, suffix) {
			n++
		}
	}

	return n
}

type harness struct {
	t        *testing.T
	mem      *store.Memory
	hooks    *hookStore
	git      *fakeGit
	stderr   *bytes.Buffer
	now      time.Time
	loc      store.Location
	refresh  time.Duration // lock refresh interval; zero keeps the default, a third of the TTL
	partSize int64         // ranged read size; zero keeps the default
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	mem := store.NewMemory()
	now := time.Unix(1_800_000_000, 0)

	// Lock ages come from object times, so the store must share the helper's clock.
	mem.SetClock(func() time.Time { return now })

	return &harness{
		t:      t,
		mem:    mem,
		hooks:  &hookStore{Memory: mem},
		git:    newFakeGit(),
		stderr: &bytes.Buffer{},
		now:    now,
		loc:    store.Location{Bucket: "bkt", Prefix: "repo"},
	}
}

func (h *harness) helper() *Helper {
	return New(Config{
		Store:       h.hooks,
		Location:    h.loc,
		Git:         h.git,
		Log:         NewLogger(h.stderr, true),
		LockTTL:     time.Minute,
		LockRefresh: h.refresh,
		FetchJobs:   2,
		PartSize:    h.partSize,
		TempDir:     h.t.TempDir(),
		Now:         func() time.Time { return h.now },
		Host:        "testhost",
		PID:         42,
	})
}

// session runs one helper session over input and returns stdout.
func (h *harness) session(input string) (string, error) {
	var out bytes.Buffer

	err := h.helper().Run(h.t.Context(), strings.NewReader(input), &out)

	return out.String(), err
}

func (h *harness) mustSession(input string) string {
	h.t.Helper()

	out, err := h.session(input)
	if err != nil {
		h.t.Fatalf("session error: %v\nstderr:\n%s", err, h.stderr)
	}

	return out
}

// seed stores a bundle for ref at s, as a previous push would have.
func (h *harness) seed(ref, s string) {
	h.mem.Put(h.loc.Key(ref, s+".bundle"), []byte("bundle "+s), time.Time{})
}

func (h *harness) bundlesOf(ref string) []string {
	var out []string

	prefix := h.loc.Key(ref) + "/"
	for _, name := range h.mem.Names() {
		if rest, ok := strings.CutPrefix(name, prefix); ok && strings.HasSuffix(rest, ".bundle") {
			out = append(out, strings.TrimSuffix(rest, ".bundle"))
		}
	}

	return out
}

func (h *harness) object(name string) (string, bool) {
	data, ok := h.mem.Get(h.loc.Key(name))

	return string(data), ok
}

func TestCapabilitiesAndOptions(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	out := h.mustSession("capabilities\noption verbosity 2\noption progress true\noption progress maybe\noption verbosity x\n\n")

	want := "*push\n*fetch\noption\n\nok\nok\nerror invalid progress \"maybe\"\nerror invalid verbosity \"x\"\n"
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestControlCharsShownAsQuestionMarks(t *testing.T) {
	t.Parallel()

	if got, want := oneLine("a\x1b[31m\nb\x07"), "a?[31m b?"; got != want {
		t.Errorf("oneLine = %q, want %q", got, want)
	}
}

// A lock holder from the bucket must not put escape sequences in the output.
func TestLockHolderControlCharsHidden(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.commit(sha("c"), "")
	h.git.refs["refs/heads/main"] = sha("c")

	body, err := json.Marshal(lockInfo{Timestamp: float64(h.now.Add(-10 * time.Second).Unix()), Host: "evil\x1b[2J\nhost", PID: 7})
	if err != nil {
		t.Fatal(err)
	}

	_ = h.mem.Put(h.loc.Key("refs/heads/main", "LOCK#.lock"), body, h.now.Add(-10*time.Second))

	out := h.mustSession("push refs/heads/main:refs/heads/main\n\n")

	if strings.Contains(out, "\x1b") {
		t.Errorf("output carries an escape character: %q", out)
	}

	if !strings.Contains(out, "held by evil?[2J?host pid 7") {
		t.Errorf("output %q does not show the holder with '?' for control characters", out)
	}
}

func TestExitOnEOFAndBlankLine(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "\n", "capabilities\n", "capabilities\n\nlist\n"} {
		h := newHarness(t)

		out, err := h.session(input)
		if err != nil {
			t.Errorf("input %q: %v", input, err)
		}

		if strings.Contains(out, "@") {
			t.Errorf("input %q: commands after the blank line ran: %q", input, out)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, err := h.session("connect git-upload-pack\n")
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("error = %v, want ErrProtocol", err)
	}
}

func TestListEmpty(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if out := h.mustSession("list\n"); out != "\n" {
		t.Fatalf("list on empty remote = %q", out)
	}
}

func TestList(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seed("refs/heads/main", sha("main"))
	h.seed("refs/tags/v1", sha("v1"))
	h.mem.Put("repo/HEAD", []byte("refs/heads/main\n"), time.Time{})
	h.mem.Put("repo/refs/heads/main/LOCK#.lock", []byte("{}"), time.Time{})
	h.mem.Put("repo/refs/heads/main/notes.txt", nil, time.Time{})
	h.mem.Put("repo/refs/heads/x/"+strings.ToUpper(sha("xyz"))+".bundle", nil, time.Time{})
	h.mem.Put("repo/refs/heads/y/abc.bundle", nil, time.Time{})
	h.mem.Put("repo/other/"+sha("o")+".bundle", nil, time.Time{})
	h.mem.Put("repo2/refs/heads/z/"+sha("z")+".bundle", nil, time.Time{})

	want := sha("main") + " refs/heads/main\n" + sha("v1") + " refs/tags/v1\n"

	if out := h.mustSession("list\n"); out != want+"@refs/heads/main HEAD\n\n" {
		t.Errorf("list = %q", out)
	}

	if out := h.mustSession("list for-push\n"); out != want+"\n" {
		t.Errorf("list for-push = %q", out)
	}
}

func TestListHeadNotInvented(t *testing.T) {
	t.Parallel()

	for name, head := range map[string]string{"missing": "", "dangling": "refs/heads/gone", "garbage": "../../etc"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.seed("refs/heads/main", sha("main"))

			if head != "" {
				h.mem.Put("repo/HEAD", []byte(head), time.Time{})
			}

			if out := h.mustSession("list\n"); strings.Contains(out, "HEAD") {
				t.Errorf("list = %q, want no HEAD line", out)
			}
		})
	}
}

func TestListPrefersNewestBundle(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.mem.Put(h.loc.Key("refs/heads/main", sha("new")+".bundle"), nil, h.now)
	h.mem.Put(h.loc.Key("refs/heads/main", sha("old")+".bundle"), nil, h.now.Add(-time.Hour))

	if out := h.mustSession("list for-push\n"); out != sha("new")+" refs/heads/main\n\n" {
		t.Errorf("list = %q", out)
	}
}

func TestFetch(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.parent[sha("c2")] = sha("c1")
	h.seed("refs/heads/main", sha("c2"))
	h.seed("refs/tags/v1", sha("t1"))
	h.seed("refs/heads/have", sha("have"))
	h.git.commit(sha("have"), "")
	h.mem.Put("repo/HEAD", []byte("refs/heads/main"), time.Time{})

	input := "list\n" +
		"fetch " + sha("c2") + " HEAD\n" +
		"fetch " + sha("c2") + " refs/heads/main\n" +
		"fetch " + sha("t1") + " refs/tags/v1\n" +
		"fetch " + sha("have") + " refs/heads/have\n\n"

	out := h.mustSession(input)
	if !strings.HasSuffix(out, "HEAD\n\n\n") {
		t.Errorf("output %q doesn't end with the fetch batch's blank line", out)
	}

	for _, s := range []string{sha("c1"), sha("c2"), sha("t1")} {
		if !h.git.objects[s] {
			t.Errorf("%s not fetched", s)
		}
	}
}

func TestFetchFailures(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		setup func(h *harness)
		want  string
	}{
		"bundle gone": {
			setup: func(*harness) {},
			want:  "is gone",
		},
		"invalid bundle": {
			setup: func(h *harness) {
				h.mem.Put(h.loc.Key("refs/heads/main", sha("c")+".bundle"), []byte("garbage"), time.Time{})
			},
			want: "is invalid",
		},
		"unbundle fails": {
			setup: func(h *harness) {
				h.seed("refs/heads/main", sha("c"))
				h.git.failRun["unbundle"] = errFake
			},
			want: "unbundle",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			tt.setup(h)

			_, err := h.session("fetch " + sha("c") + " refs/heads/main\n\n")
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "refs/heads/main") {
				t.Fatalf("error = %v, want one containing %q and the ref", err, tt.want)
			}
		})
	}
}

func TestFetchBadCommand(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"fetch nothex refs/heads/main\n\n", "fetch " + sha("a") + "\n\n", "fetch " + sha("a") + " refs/x\npush a:b\n\n"} {
		h := newHarness(t)

		_, err := h.session(input)
		if !errors.Is(err, ErrProtocol) {
			t.Errorf("input %q: error = %v, want ErrProtocol", input, err)
		}
	}
}

func TestPushNewBranchSetsHead(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.commit(sha("c1"), "")
	h.git.refs["refs/heads/main"] = sha("c1")

	if out := h.mustSession("push refs/heads/main:refs/heads/main\n\n"); out != "ok refs/heads/main\n\n" {
		t.Fatalf("push = %q", out)
	}

	if got := h.bundlesOf("refs/heads/main"); !slices.Equal(got, []string{sha("c1")}) {
		t.Errorf("bundles = %v", got)
	}

	if head, _ := h.object("HEAD"); head != "refs/heads/main" {
		t.Errorf("HEAD = %q", head)
	}

	if _, locked := h.object("refs/heads/main/LOCK#.lock"); locked {
		t.Error("lock not released")
	}

	// A second branch must not move HEAD.
	h.git.refs["refs/heads/dev"] = sha("c1")
	h.mustSession("push refs/heads/dev:refs/heads/dev\n\n")

	if head, _ := h.object("HEAD"); head != "refs/heads/main" {
		t.Errorf("HEAD moved to %q", head)
	}
}

func TestPushTagDoesNotSetHead(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.commit(sha("tag"), "")
	h.git.refs["refs/tags/v1"] = sha("tag")

	if out := h.mustSession("push refs/tags/v1:refs/tags/v1\n\n"); out != "ok refs/tags/v1\n\n" {
		t.Fatalf("push = %q", out)
	}

	if _, ok := h.object("HEAD"); ok {
		t.Error("tag push created HEAD")
	}
}

func TestPushFastForwardReplacesBundle(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.commit(sha("c1"), "")
	h.git.commit(sha("c2"), sha("c1"))
	h.git.refs["refs/heads/main"] = sha("c2")
	h.seed("refs/heads/main", sha("c1"))

	if out := h.mustSession("push refs/heads/main:refs/heads/main\n\n"); out != "ok refs/heads/main\n\n" {
		t.Fatalf("push = %q\n%s", out, h.stderr)
	}

	if got := h.bundlesOf("refs/heads/main"); !slices.Equal(got, []string{sha("c2")}) {
		t.Errorf("bundles = %v, want only the new one", got)
	}
}

func TestPushRejections(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		line  string
		setup func(h *harness)
		want  string
	}{
		"non-fast-forward": {
			line: "push refs/heads/main:refs/heads/main",
			setup: func(h *harness) {
				h.git.commit(sha("other"), "")
				h.git.refs["refs/heads/main"] = sha("other")
				h.git.commit(sha("remote"), "")
				h.seed("refs/heads/main", sha("remote"))
			},
			want: "error refs/heads/main non-fast forward",
		},
		"remote commit unknown locally": {
			line: "push refs/heads/main:refs/heads/main",
			setup: func(h *harness) {
				h.git.commit(sha("mine"), "")
				h.git.refs["refs/heads/main"] = sha("mine")
				h.seed("refs/heads/main", sha("theirs"))
			},
			want: "error refs/heads/main fetch first",
		},
		"unresolvable source": {
			line:  "push refs/heads/nope:refs/heads/main",
			setup: func(*harness) {},
			want:  "error refs/heads/main cannot resolve refs/heads/nope",
		},
		"invalid destination": {
			line:  "push refs/heads/main:refs/heads/../../HEAD",
			setup: func(*harness) {},
			want:  "error refs/heads/../../HEAD invalid remote ref name",
		},
		"destination outside refs": {
			line:  "push refs/heads/main:HEAD",
			setup: func(*harness) {},
			want:  "error HEAD invalid remote ref name",
		},
		"several bundles": {
			line: "push refs/heads/main:refs/heads/main",
			setup: func(h *harness) {
				h.git.commit(sha("c"), "")
				h.git.refs["refs/heads/main"] = sha("c")
				h.seed("refs/heads/main", sha("a"))
				h.seed("refs/heads/main", sha("b"))
			},
			want: "error refs/heads/main several bundles for one ref",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			tt.setup(h)
			before := h.mem.Names()

			out := h.mustSession(tt.line + "\n\n")
			if !strings.HasPrefix(out, tt.want) || !strings.HasSuffix(out, "\n\n") || strings.Count(out, "\n") != 2 {
				t.Errorf("push = %q, want one line starting %q", out, tt.want)
			}

			if after := h.mem.Names(); !slices.Equal(before, after) {
				t.Errorf("rejected push changed the bucket: %v -> %v", before, after)
			}
		})
	}
}

func TestLeases(t *testing.T) {
	t.Parallel()

	zero := strings.Repeat("0", 40)

	tests := map[string]struct {
		remote string // remote SHA, "" for no remote ref
		input  string
		want   string
	}{
		"matching lease forces, though git sends the push unforced": {
			remote: sha("theirs"),
			input:  "option cas refs/heads/main:" + sha("theirs") + "\npush refs/heads/main:refs/heads/main\n\n",
			want:   "ok\nok refs/heads/main\n\n",
		},
		"stale lease": {
			remote: sha("newer"),
			input:  "option cas refs/heads/main:" + sha("theirs") + "\npush +refs/heads/main:refs/heads/main\n\n",
			want:   "ok\nerror refs/heads/main stale info\n\n",
		},
		"must not exist, but does": {
			remote: sha("theirs"),
			input:  "option cas refs/heads/main:" + zero + "\npush +refs/heads/main:refs/heads/main\n\n",
			want:   "ok\nerror refs/heads/main stale info\n\n",
		},
		"must not exist, and doesn't": {
			input: "option cas refs/heads/main:" + zero + "\npush refs/heads/main:refs/heads/main\n\n",
			want:  "ok\nok refs/heads/main\n\n",
		},
		"stale lease on delete": {
			remote: sha("newer"),
			input:  "option cas refs/heads/main:" + sha("theirs") + "\npush :refs/heads/main\n\n",
			want:   "ok\nerror refs/heads/main stale info\n\n",
		},
		"lease applies to its batch only": {
			remote: sha("newer"),
			input: "option cas refs/heads/main:" + sha("theirs") + "\npush refs/heads/other:refs/heads/other\n\n" +
				"push +refs/heads/main:refs/heads/main\n\n",
			want: "ok\nok refs/heads/other\n\nok refs/heads/main\n\n",
		},
		"invalid": {
			input: "option cas refs/heads/main:nothex\n",
			want:  "error invalid cas \"refs/heads/main:nothex\"\n",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.git.commit(sha("mine"), "")
			h.git.refs["refs/heads/main"] = sha("mine")
			h.git.refs["refs/heads/other"] = sha("mine")

			if tt.remote != "" {
				h.seed("refs/heads/main", tt.remote)
			}

			if out := h.mustSession(tt.input); out != tt.want {
				t.Fatalf("got %q, want %q\n%s", out, tt.want, h.stderr)
			}
		})
	}
}

// TestLeaseRacedBeforeLock: a push landing after the lease check but before the lock still fails, as a stale remote.
func TestLeaseRacedBeforeLock(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.commit(sha("mine"), "")
	h.git.refs["refs/heads/main"] = sha("mine")
	h.seed("refs/heads/main", sha("theirs"))

	h.hooks.beforeWrite = func(name string) {
		if strings.HasSuffix(name, "LOCK#.lock") {
			h.hooks.beforeWrite = nil
			h.seed("refs/heads/main", sha("newer"))
			h.remove(h.loc.Key("refs/heads/main", sha("theirs")+".bundle"))
		}
	}

	out := h.mustSession("option cas refs/heads/main:" + sha("theirs") + "\npush +refs/heads/main:refs/heads/main\n\n")
	if out != "ok\nerror refs/heads/main stale remote, fetch and retry\n\n" {
		t.Fatalf("push = %q", out)
	}

	if got := h.bundlesOf("refs/heads/main"); !slices.Equal(got, []string{sha("newer")}) {
		t.Errorf("bundles = %v, the racing push's must survive", got)
	}
}

func TestPushForce(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.commit(sha("mine"), "")
	h.git.refs["refs/heads/main"] = sha("mine")
	h.seed("refs/heads/main", sha("theirs"))

	if out := h.mustSession("push +refs/heads/main:refs/heads/main\n\n"); out != "ok refs/heads/main\n\n" {
		t.Fatalf("force push = %q\n%s", out, h.stderr)
	}

	if got := h.bundlesOf("refs/heads/main"); !slices.Equal(got, []string{sha("mine")}) {
		t.Errorf("bundles = %v", got)
	}
}

func TestPushUpToDateUploadsNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.commit(sha("c"), "")
	h.git.refs["refs/heads/main"] = sha("c")
	h.seed("refs/heads/main", sha("c"))

	if out := h.mustSession("push refs/heads/main:refs/heads/main\n\n"); out != "ok refs/heads/main\n\n" {
		t.Fatalf("push = %q", out)
	}

	if len(h.git.created) != 0 {
		t.Errorf("bundled %v for an up-to-date ref", h.git.created)
	}
}

func TestPushRawSHA(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.commit(sha("c"), "")

	if out := h.mustSession("push " + sha("c") + ":refs/heads/x\n\n"); out != "ok refs/heads/x\n\n" {
		t.Fatalf("push = %q\n%s", out, h.stderr)
	}

	if got := h.bundlesOf("refs/heads/x"); !slices.Equal(got, []string{sha("c")}) {
		t.Errorf("bundles = %v", got)
	}
}

func TestPushRejectsSHA256(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	long := strings.Repeat("ab", 32)
	h.git.objects[long] = true
	h.git.refs["refs/heads/main"] = long

	out := h.mustSession("push refs/heads/main:refs/heads/main\n\n")
	if out != "error refs/heads/main SHA-256 repositories aren't supported: bundles are named by 40-hex SHA-1\n\n" {
		t.Fatalf("push = %q", out)
	}

	if names := h.mem.Names(); len(names) != 0 {
		t.Errorf("rejected push wrote %v", names)
	}
}

func TestPushBatchIsIndependent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.commit(sha("a"), "")
	h.git.refs["refs/heads/a"] = sha("a")
	h.git.refs["refs/heads/c"] = sha("a")

	out := h.mustSession("push refs/heads/a:refs/heads/a\npush refs/heads/missing:refs/heads/b\npush refs/heads/c:refs/heads/c\n\n")

	lines := strings.Split(out, "\n")
	if len(lines) != 5 || lines[0] != "ok refs/heads/a" || !strings.HasPrefix(lines[1], "error refs/heads/b ") ||
		lines[2] != "ok refs/heads/c" {
		t.Fatalf("batch = %q", out)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seed("refs/heads/main", sha("m"))
	h.seed("refs/heads/feature", sha("f"))
	h.mem.Put("repo/HEAD", []byte("refs/heads/main"), time.Time{})

	out := h.mustSession("push :refs/heads/feature\npush :refs/heads/main\npush :refs/heads/nope\n\n")

	want := "ok refs/heads/feature\n" +
		"error refs/heads/main refusing to delete the remote HEAD branch\n" +
		"error refs/heads/nope remote ref does not exist\n\n"
	if out != want {
		t.Fatalf("delete = %q, want %q", out, want)
	}

	if got := h.bundlesOf("refs/heads/feature"); len(got) != 0 {
		t.Errorf("feature bundles left: %v", got)
	}

	if got := h.bundlesOf("refs/heads/main"); len(got) != 1 {
		t.Errorf("main bundles = %v", got)
	}

	if names := h.mem.Names(); slices.ContainsFunc(names, func(n string) bool { return strings.HasSuffix(n, "LOCK#.lock") }) {
		t.Errorf("lock left behind: %v", names)
	}
}

// putLock writes another client's lock on refs/heads/main, created age ago.
func (h *harness) putLock(age time.Duration) store.Object {
	body, err := json.Marshal(lockInfo{Timestamp: float64(h.now.Add(-age).Unix()), Host: "otherhost", PID: 7})
	if err != nil {
		h.t.Fatal(err)
	}

	return h.mem.Put(h.loc.Key("refs/heads/main", "LOCK#.lock"), body, h.now.Add(-age))
}

func TestFreshLockBlocksPush(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.commit(sha("c"), "")
	h.git.refs["refs/heads/main"] = sha("c")
	lock := h.putLock(10 * time.Second)

	out := h.mustSession("push refs/heads/main:refs/heads/main\n\n")

	for _, want := range []string{
		"error refs/heads/main could not lock gs://bkt/repo/refs/heads/main/LOCK#.lock",
		"held by otherhost pid 7",
		"after 60s (GIT_REMOTE_GS_LOCK_TTL)",
		"for 10s",
		"gcloud storage rm 'gs://bkt/repo/refs/heads/main/LOCK#.lock'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("push = %q, missing %q", out, want)
		}
	}

	if got := h.bundlesOf("refs/heads/main"); len(got) != 0 {
		t.Errorf("uploaded despite the lock: %v", got)
	}

	if body, _ := h.object("refs/heads/main/LOCK#.lock"); !strings.Contains(body, "otherhost") {
		t.Errorf("another client's lock was touched: %q (generation was %d)", body, lock.Generation)
	}
}

func TestStaleLockTakenOver(t *testing.T) {
	t.Parallel()

	for name, body := range map[string][]byte{"json": nil, "unparseable": []byte("garbage")} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.git.commit(sha("c"), "")
			h.git.refs["refs/heads/main"] = sha("c")

			if body == nil {
				h.putLock(2 * time.Minute)
			} else {
				h.mem.Put(h.loc.Key("refs/heads/main", "LOCK#.lock"), body, h.now.Add(-2*time.Minute))
			}

			if out := h.mustSession("push refs/heads/main:refs/heads/main\n\n"); out != "ok refs/heads/main\n\n" {
				t.Fatalf("push = %q\n%s", out, h.stderr)
			}

			if _, ok := h.object("refs/heads/main/LOCK#.lock"); ok {
				t.Error("lock left behind")
			}
		})
	}
}

func TestLockTakenOverDuringPushIsNotDeleted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.commit(sha("c"), "")
	h.git.refs["refs/heads/main"] = sha("c")

	// While our bundle uploads, another client replaces our lock, as after a TTL expiry.
	h.hooks.beforeWrite = func(name string) {
		if strings.HasSuffix(name, ".bundle") {
			h.putLock(0)
		}
	}

	h.mustSession("push refs/heads/main:refs/heads/main\n\n")

	if body, _ := h.object("refs/heads/main/LOCK#.lock"); !strings.Contains(body, "otherhost") {
		t.Errorf("released another client's lock: %q", body)
	}

	if !strings.Contains(h.stderr.String(), "warning: lock gs://bkt/repo/refs/heads/main/LOCK#.lock was taken over") {
		t.Errorf("no takeover warning in stderr:\n%s", h.stderr)
	}
}

func TestStaleRemoteUnderLock(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.commit(sha("c1"), "")
	h.git.commit(sha("c2"), sha("c1"))
	h.git.refs["refs/heads/main"] = sha("c2")
	h.seed("refs/heads/main", sha("c1"))

	// Between our first listing and taking the lock, another push replaces the bundle.
	h.hooks.beforeWrite = func(name string) {
		if strings.HasSuffix(name, "LOCK#.lock") {
			h.hooks.beforeWrite = nil
			h.mem.Put(h.loc.Key("refs/heads/main", sha("c1")+".bundle"), []byte("bundle "+sha("c1")), time.Time{})
		}
	}

	out := h.mustSession("push refs/heads/main:refs/heads/main\n\n")
	if out != "error refs/heads/main stale remote, fetch and retry\n\n" {
		t.Fatalf("push = %q", out)
	}

	if _, ok := h.object("refs/heads/main/LOCK#.lock"); ok {
		t.Error("lock not released after a failed push")
	}
}

func TestConcurrentPushLosesOnLock(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.commit(sha("a"), "")
	h.git.commit(sha("b"), "")
	h.git.refs["refs/heads/a"] = sha("a")
	h.git.refs["refs/heads/b"] = sha("b")

	var second string

	// While the first push holds the lock and uploads, a second client pushes the same ref.
	h.hooks.beforeWrite = func(name string) {
		if strings.HasSuffix(name, sha("a")+".bundle") {
			h.hooks.beforeWrite = nil
			second = h.mustSession("push refs/heads/b:refs/heads/main\n\n")
		}
	}

	first := h.mustSession("push refs/heads/a:refs/heads/main\n\n")

	if first != "ok refs/heads/main\n\n" || !strings.Contains(second, "could not lock") {
		t.Fatalf("first = %q, second = %q", first, second)
	}

	if got := h.bundlesOf("refs/heads/main"); !slices.Equal(got, []string{sha("a")}) {
		t.Errorf("bundles = %v", got)
	}
}

func TestLimitedBuffer(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seed("refs/heads/main", sha("m"))
	h.mem.Put("repo/HEAD", bytes.Repeat([]byte("x"), maxSmallObject+1), time.Time{})

	_, err := h.session("list\n")
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("error = %v, want errTooLarge", err)
	}
}

func TestValidRef(t *testing.T) {
	t.Parallel()

	for ref, want := range map[string]bool{
		"refs/heads/main":        true,
		"refs/tags/v1.0":         true,
		"refs/heads/feat/x-y_z":  true,
		"HEAD":                   false,
		"refs/":                  false,
		"refs/heads/":            false,
		"refs//x":                false,
		"refs/heads/../x":        false,
		"refs/heads/.hidden":     false,
		"refs/heads/with space":  false,
		"refs/heads/tab\there":   false,
		"heads/main":             false,
		"refs/heads/main/":       false,
		"refs/heads/ok.lock.not": true,
	} {
		if got := validRef(ref); got != want {
			t.Errorf("validRef(%q) = %v, want %v", ref, got, want)
		}
	}
}

// lockGeneration returns the generation of the ref's lock, or 0 when unlocked.
func (h *harness) lockGeneration(ref string) int64 {
	objs, err := h.mem.List(h.t.Context(), h.loc.Key(ref, "LOCK#.lock"))
	if err != nil || len(objs) != 1 {
		return 0
	}

	return objs[0].Generation
}

// remove deletes an object the way another client would.
func (h *harness) remove(name string) {
	objs, err := h.mem.List(h.t.Context(), name)
	if err != nil || len(objs) != 1 {
		h.t.Fatalf("remove %s: %v %v", name, objs, err)
	}

	err = h.mem.DeleteIfGeneration(h.t.Context(), name, objs[0].Generation)
	if err != nil {
		h.t.Fatal(err)
	}
}

func TestLockRefreshedDuringLongUpload(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.refresh = 5 * time.Millisecond
	h.git.commit(sha("c"), "")
	h.git.refs["refs/heads/main"] = sha("c")

	var atStart, atEnd int64

	h.hooks.beforeWrite = func(name string) {
		if strings.HasSuffix(name, ".bundle") {
			atStart = h.lockGeneration("refs/heads/main")

			time.Sleep(60 * time.Millisecond) // a slow upload

			atEnd = h.lockGeneration("refs/heads/main")
		}
	}

	if out := h.mustSession("push refs/heads/main:refs/heads/main\n\n"); out != "ok refs/heads/main\n\n" {
		t.Fatalf("push = %q\n%s", out, h.stderr)
	}

	if atStart == 0 || atEnd == atStart {
		t.Errorf("lock generation %d at upload start, %d at end: not refreshed", atStart, atEnd)
	}

	// The release must find the refreshed generation, not the one first created.
	if _, ok := h.object("refs/heads/main/LOCK#.lock"); ok || strings.Contains(h.stderr.String(), "warning") {
		t.Errorf("refreshed lock not released cleanly:\n%s", h.stderr)
	}
}

func TestLockLostAbortsUpload(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.refresh = 5 * time.Millisecond
	h.git.commit(sha("c"), "")
	h.git.refs["refs/heads/main"] = sha("c")

	// Mid-upload, another client takes the lock over; our next refresh notices and aborts the upload.
	h.hooks.beforeWrite = func(name string) {
		if strings.HasSuffix(name, ".bundle") {
			h.putLock(0)
			time.Sleep(60 * time.Millisecond)
		}
	}

	out := h.mustSession("push refs/heads/main:refs/heads/main\n\n")
	if !strings.HasPrefix(out, "error refs/heads/main lost the lock gs://bkt/repo/refs/heads/main/LOCK#.lock") ||
		!strings.Contains(out, "nothing was pushed") {
		t.Fatalf("push = %q", out)
	}

	if got := h.bundlesOf("refs/heads/main"); len(got) != 0 {
		t.Errorf("upload committed after losing the lock: %v", got)
	}

	if body, _ := h.object("refs/heads/main/LOCK#.lock"); !strings.Contains(body, "otherhost") {
		t.Errorf("the other client's lock was touched: %q", body)
	}
}

func TestOldBundleGoneIsConflict(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.commit(sha("c1"), "")
	h.git.commit(sha("c2"), sha("c1"))
	h.git.refs["refs/heads/main"] = sha("c2")
	h.seed("refs/heads/main", sha("c1"))

	// While we upload, a client that took our lock over replaces the ref's bundle.
	h.hooks.beforeWrite = func(name string) {
		if strings.HasSuffix(name, sha("c2")+".bundle") {
			h.seed("refs/heads/main", sha("other"))
			h.remove(h.loc.Key("refs/heads/main", sha("c1")+".bundle"))
		}
	}

	out := h.mustSession("push refs/heads/main:refs/heads/main\n\n")
	if !strings.HasPrefix(out, "error refs/heads/main pushed "+sha("c2")+", but another push changed the ref") {
		t.Fatalf("push = %q, want a conflict error", out)
	}
}

func TestProtectedRefs(t *testing.T) {
	t.Parallel()

	for _, marker := range []string{"PROTECTED#"} {
		t.Run(marker, func(t *testing.T) {
			t.Parallel()

			setup := func() *harness {
				h := newHarness(t)
				h.git.commit(sha("c1"), "")
				h.git.commit(sha("c2"), sha("c1"))
				h.git.commit(sha("other"), "")
				h.seed("refs/heads/main", sha("c1"))
				h.mem.Put(h.loc.Key("refs/heads/main", marker), nil, time.Time{})

				return h
			}

			for line, want := range map[string]string{
				"push +refs/heads/other:refs/heads/main": "error refs/heads/main protected branch, force push not allowed\n\n",
				"push refs/heads/other:refs/heads/main":  "error refs/heads/main non-fast forward\n\n",
				"push :refs/heads/main":                  "error refs/heads/main refusing to delete a protected branch\n\n",
				"push +refs/heads/c2:refs/heads/main":    "ok refs/heads/main\n\n",
			} {
				h := setup()
				h.git.refs["refs/heads/other"] = sha("other")
				h.git.refs["refs/heads/c2"] = sha("c2")

				if out := h.mustSession(line + "\n\n"); out != want {
					t.Errorf("%s: got %q, want %q", line, out, want)
				}
			}

			// A list ignores the marker.
			h := setup()
			if out := h.mustSession("list for-push\n"); out != sha("c1")+" refs/heads/main\n\n" {
				t.Errorf("list = %q", out)
			}
		})
	}
}

func TestProtectedDuringForcePush(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.commit(sha("theirs"), "")
	h.git.commit(sha("mine"), "")
	h.git.refs["refs/heads/main"] = sha("mine")
	h.seed("refs/heads/main", sha("theirs"))

	// The ref gets protected after our first check, before we take the lock.
	h.hooks.beforeWrite = func(name string) {
		if strings.HasSuffix(name, "LOCK#.lock") {
			h.hooks.beforeWrite = nil
			h.mem.Put(h.loc.Key("refs/heads/main", "PROTECTED#"), nil, time.Time{})
		}
	}

	out := h.mustSession("push +refs/heads/main:refs/heads/main\n\n")
	if out != "error refs/heads/main protected branch, force push not allowed\n\n" {
		t.Fatalf("push = %q", out)
	}

	if got := h.bundlesOf("refs/heads/main"); !slices.Equal(got, []string{sha("theirs")}) {
		t.Errorf("bundles = %v", got)
	}
}

func TestPushHeadIsFirstBranchInBatch(t *testing.T) {
	t.Parallel()

	for range 5 { // the pushes run in parallel: HEAD must not depend on which ends first
		h := newHarness(t)
		h.git.commit(sha("c"), "")

		for _, ref := range []string{"refs/heads/a", "refs/heads/b", "refs/tags/t"} {
			h.git.refs[ref] = sha("c")
		}

		out := h.mustSession("push refs/tags/t:refs/tags/t\npush refs/heads/missing:refs/heads/x\n" +
			"push refs/heads/b:refs/heads/b\npush refs/heads/a:refs/heads/a\n\n")

		lines := strings.Split(out, "\n")
		if len(lines) != 6 || lines[0] != "ok refs/tags/t" || !strings.HasPrefix(lines[1], "error refs/heads/x ") ||
			lines[2] != "ok refs/heads/b" || lines[3] != "ok refs/heads/a" {
			t.Fatalf("batch = %q", out)
		}

		if head, _ := h.object("HEAD"); head != "refs/heads/b" {
			t.Fatalf("HEAD = %q, want the first branch pushed, refs/heads/b", head)
		}
	}
}

func TestFetchSkipsObjectsBroughtByBranches(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.git.parent[sha("c2")] = sha("c1")
	h.seed("refs/heads/main", sha("c2"))
	h.seed("refs/tags/v1", sha("c1")) // a lightweight tag on main's history

	h.mustSession("fetch " + sha("c1") + " refs/tags/v1\nfetch " + sha("c2") + " refs/heads/main\n\n")

	if !h.git.objects[sha("c1")] || !h.git.objects[sha("c2")] {
		t.Fatal("objects not fetched")
	}

	if n := h.hooks.readsOf("read", sha("c1")+".bundle"); n != 0 {
		t.Errorf("downloaded the tag's bundle %d times, though main's history holds it", n)
	}
}

func TestFetchLargeBundleInParts(t *testing.T) {
	t.Parallel()

	for name, listFirst := range map[string]bool{"listed": true, "not listed": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.partSize = 4 // the fake's 47-byte bundles span 12 parts
			h.seed("refs/heads/main", sha("c"))

			input := "fetch " + sha("c") + " refs/heads/main\n\n"
			if listFirst {
				input = "list\n" + input
			}

			h.mustSession(input)

			if !h.git.objects[sha("c")] {
				t.Fatalf("not fetched:\n%s", h.stderr)
			}

			ranges, whole := h.hooks.readsOf("range", ".bundle"), h.hooks.readsOf("read", ".bundle")
			if listFirst && (ranges != 12 || whole != 0) || !listFirst && (ranges != 0 || whole != 1) {
				t.Errorf("%d ranged and %d whole reads", ranges, whole)
			}
		})
	}
}

func TestFetchBundleReplacedDuringPartsIsGone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.partSize = 4
	h.seed("refs/heads/main", sha("c"))

	helper := h.helper()

	err := helper.Run(t.Context(), strings.NewReader("list\n"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	h.seed("refs/heads/main", sha("c")) // re-pushed after the list: same name, new generation

	err = helper.Run(t.Context(), strings.NewReader("fetch "+sha("c")+" refs/heads/main\n\n"), io.Discard)
	if !errors.Is(err, errBundleGone) {
		t.Fatalf("error = %v, want errBundleGone", err)
	}
}
