// Package protocol implements the git remote helper protocol (gitremote-helpers(7)) on top of a store.Store.
package protocol

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MaesterZ/git-remote-gs/internal/store"
)

const (
	headObject   = "HEAD"
	lockObject   = "LOCK#.lock" // git-remote-s3's lock name
	bundleSuffix = ".bundle"
	refsPrefix   = "refs/"
	headsPrefix  = "refs/heads/"

	// protectedObject marks a protected ref, as `git-s3 protect` writes it.
	protectedObject = "PROTECTED#"

	// DefaultLockTTL is how old a push lock must be before another push may take it over.
	DefaultLockTTL = 60 * time.Second
	// DefaultFetchJobs bounds parallel bundle downloads, and parallel parts of one large bundle.
	DefaultFetchJobs = 4
	// DefaultPushJobs bounds refs pushed in parallel.
	DefaultPushJobs = 4
	// DefaultPartSize is the size of each ranged read of a large bundle.
	DefaultPartSize = 16 << 20
	// minParts is how many parts a bundle must span to be downloaded as parallel ranged reads.
	minParts = 4

	// maxSmallObject caps reads of HEAD and lock objects, which hold a few bytes.
	maxSmallObject = 64 << 10
)

var (
	// ErrProtocol is a malformed or unexpected command from git.
	ErrProtocol = errors.New("protocol error")

	errTooLarge   = errors.New("object too large")
	errBundleGone = errors.New("the ref was updated since it was listed: fetch again")

	shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Git is the subset of git the helper needs; internal/git implements it with the git binary.
type Git interface {
	ObjectExists(ctx context.Context, sha string) (bool, error)
	MissingObjects(ctx context.Context, shas []string) (map[string]bool, error)
	Resolve(ctx context.Context, rev string) (string, error)
	IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error)
	CreateBundle(ctx context.Context, sha, path string) error
	VerifyBundle(ctx context.Context, path string) error
	Unbundle(ctx context.Context, path string) error
}

// Config wires a Helper. Zero values of the optional fields get defaults.
type Config struct {
	Store    store.Store
	Location store.Location
	Git      Git
	Log      *Logger

	LockTTL     time.Duration    // default DefaultLockTTL
	LockRefresh time.Duration    // how often a held lock is rewritten, default LockTTL/3
	FetchJobs   int              // default DefaultFetchJobs
	PushJobs    int              // default DefaultPushJobs
	PartSize    int64            // default DefaultPartSize
	TempDir     string           // default os.TempDir()
	Now         func() time.Time // default time.Now
	Host        string           // lock owner, default os.Hostname()
	PID         int              // lock owner, default os.Getpid()
}

// Helper serves one git remote helper session.
type Helper struct {
	cfg Config
	// head is the HEAD target seen by the last list, used to map `fetch <sha> HEAD`.
	head string
	// listed holds the bundles seen by the last list, whose sizes and generations fetch uses for ranged reads.
	listed map[string]bundle
}

// New returns a Helper, filling in defaults.
func New(cfg Config) *Helper {
	if cfg.LockTTL == 0 {
		cfg.LockTTL = DefaultLockTTL
	}

	if cfg.LockRefresh <= 0 {
		cfg.LockRefresh = cfg.LockTTL / 3 //nolint:mnd // three refreshes per TTL survive one or two failed ones
	}

	if cfg.FetchJobs <= 0 {
		cfg.FetchJobs = DefaultFetchJobs
	}

	if cfg.PushJobs <= 0 {
		cfg.PushJobs = DefaultPushJobs
	}

	if cfg.PartSize <= 0 {
		cfg.PartSize = DefaultPartSize
	}

	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	if cfg.Host == "" {
		cfg.Host, _ = os.Hostname()
	}

	if cfg.PID == 0 {
		cfg.PID = os.Getpid()
	}

	if cfg.Log == nil {
		cfg.Log = NewLogger(io.Discard, false)
	}

	return &Helper{cfg: cfg}
}

// Run reads commands from in and answers on out until a blank line or EOF outside a batch.
func (h *Helper) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	writer := bufio.NewWriter(out)

	for {
		line, err := readLine(reader)
		if errors.Is(err, io.EOF) || (err == nil && line == "") {
			return nil
		}

		if err != nil {
			return fmt.Errorf("read command: %w", err)
		}

		h.cfg.Log.Logf("< %s", line)

		err = h.dispatch(ctx, reader, writer, line)
		if err != nil {
			return err
		}

		err = writer.Flush()
		if err != nil {
			return fmt.Errorf("write reply: %w", err)
		}
	}
}

func (h *Helper) dispatch(ctx context.Context, reader *bufio.Reader, out *bufio.Writer, line string) error {
	switch {
	case line == "capabilities":
		_, _ = out.WriteString("*push\n*fetch\noption\n\n")
	case strings.HasPrefix(line, "option "):
		_, _ = out.WriteString(h.option(line) + "\n")
	case line == "list" || line == "list for-push":
		return h.list(ctx, out, line == "list for-push")
	case strings.HasPrefix(line, "fetch "):
		return h.fetchBatch(ctx, reader, out, line)
	case strings.HasPrefix(line, "push "):
		return h.pushBatch(ctx, reader, out, line)
	default:
		return fmt.Errorf("%w: unknown command %q", ErrProtocol, line)
	}

	return nil
}

func (h *Helper) fetchBatch(ctx context.Context, reader *bufio.Reader, out *bufio.Writer, first string) error {
	batch, err := readBatch(reader, first, "fetch ")
	if err != nil {
		return err
	}

	err = h.fetch(ctx, batch)
	if err != nil {
		return err
	}

	_, _ = out.WriteString("\n")

	return nil
}

func (h *Helper) pushBatch(ctx context.Context, reader *bufio.Reader, out *bufio.Writer, first string) error {
	batch, err := readBatch(reader, first, "push ")
	if err != nil {
		return err
	}

	for _, result := range h.push(ctx, batch) {
		h.cfg.Log.Logf("> %s", result)
		_, _ = out.WriteString(result + "\n")
	}

	_, _ = out.WriteString("\n")

	return nil
}

// readLine returns one line without its terminator; a final unterminated line is returned as is.
func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if errors.Is(err, io.EOF) && line != "" {
		err = nil
	}

	return strings.TrimRight(line, "\r\n"), err
}

// readBatch reads the rest of a fetch or push batch, up to a blank line or EOF.
func readBatch(reader *bufio.Reader, first, prefix string) ([]string, error) {
	batch := []string{first}

	for {
		line, err := readLine(reader)
		if errors.Is(err, io.EOF) || (err == nil && line == "") {
			return batch, nil
		}

		if err != nil {
			return nil, fmt.Errorf("read batch: %w", err)
		}

		if !strings.HasPrefix(line, prefix) {
			return nil, fmt.Errorf("%w: %q inside a %q batch", ErrProtocol, line, strings.TrimSpace(prefix))
		}

		batch = append(batch, line)
	}
}

func (h *Helper) option(line string) string {
	name, value, _ := strings.Cut(strings.TrimPrefix(line, "option "), " ")
	if name != "verbosity" {
		return "unsupported"
	}

	level, err := strconv.Atoi(value)
	if err != nil {
		return "error invalid verbosity " + strconv.Quote(value)
	}

	h.cfg.Log.SetVerbosity(level)

	return "ok"
}

// bundle is one <ref>/<sha>.bundle object.
type bundle struct {
	ref string
	sha string
	obj store.Object
}

// parseBundle splits an object name into ref and SHA, if it is a <prefix>/refs/.../<40-hex>.bundle object.
func (h *Helper) parseBundle(obj store.Object) (bundle, bool) {
	rel, under := h.cfg.Location.Rel(obj.Name)
	if !under {
		return bundle{}, false
	}

	ref, file, nested := strings.CutLast(rel, "/")
	if !nested {
		return bundle{}, false
	}

	sha, isBundle := strings.CutSuffix(file, bundleSuffix)
	if !isBundle || !shaRe.MatchString(sha) || !validRef(ref) {
		return bundle{}, false
	}

	return bundle{ref: ref, sha: sha, obj: obj}, true
}

// remoteRefs lists every ref; with several bundles for a ref (a push in flight), the newest wins.
func (h *Helper) remoteRefs(ctx context.Context) (map[string]bundle, error) {
	objs, err := h.cfg.Store.List(ctx, h.cfg.Location.Key("refs")+"/")
	if err != nil {
		return nil, err
	}

	refs := map[string]bundle{}

	for _, obj := range objs {
		found, ok := h.parseBundle(obj)
		if !ok {
			continue
		}

		if cur, dup := refs[found.ref]; dup {
			h.cfg.Log.Logf("several bundles for %s: %s and %s", found.ref, cur.sha, found.sha)

			if !newer(found.obj, cur.obj) {
				continue
			}
		}

		refs[found.ref] = found
	}

	return refs, nil
}

func newer(a, b store.Object) bool {
	if !a.Created.Equal(b.Created) {
		return a.Created.After(b.Created)
	}

	return a.Generation > b.Generation
}

// refState is what is stored directly under one ref: its bundles, and whether a protection marker exists.
type refState struct {
	bundles   []bundle
	protected bool
}

// sha returns the ref's remote SHA, or "" when it has no single bundle.
func (r refState) sha() string {
	if len(r.bundles) != 1 {
		return ""
	}

	return r.bundles[0].sha
}

// readRef lists the objects stored directly under one ref.
func (h *Helper) readRef(ctx context.Context, ref string) (refState, error) {
	dir := h.cfg.Location.Key(ref) + "/"

	objs, err := h.cfg.Store.List(ctx, dir)
	if err != nil {
		return refState{}, err
	}

	var state refState

	for _, obj := range objs {
		file := strings.TrimPrefix(obj.Name, dir)
		if file == protectedObject {
			state.protected = true
		}

		if found, ok := h.parseBundle(obj); ok && found.ref == ref {
			state.bundles = append(state.bundles, found)
		}
	}

	return state, nil
}

// readHead returns the ref HEAD names, or "" when there is no HEAD object.
func (h *Helper) readHead(ctx context.Context) (string, error) {
	data, err := h.readSmall(ctx, h.cfg.Location.Key(headObject))
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}

	if err != nil {
		return "", err
	}

	head := strings.TrimPrefix(strings.TrimSpace(string(data)), "ref: ")
	if !validRef(head) {
		h.cfg.Log.Logf("ignoring HEAD %q: not a ref name", head)

		return "", nil
	}

	return head, nil
}

// readSmall reads an object that is expected to be a few bytes, refusing anything over maxSmallObject.
func (h *Helper) readSmall(ctx context.Context, name string) ([]byte, error) {
	buf := &limitedBuffer{max: maxSmallObject}

	_, err := h.cfg.Store.Read(ctx, name, buf)
	if err != nil {
		return nil, err
	}

	return buf.buf.Bytes(), nil
}

type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		return 0, fmt.Errorf("%w: over %d bytes", errTooLarge, b.max)
	}

	return b.buf.Write(p) //nolint:wrapcheck // bytes.Buffer.Write never fails
}

func (h *Helper) list(ctx context.Context, out *bufio.Writer, forPush bool) error {
	var (
		head    string
		headErr error
		wg      sync.WaitGroup
	)

	if !forPush {
		wg.Go(func() { head, headErr = h.readHead(ctx) })
	}

	refs, err := h.remoteRefs(ctx)

	wg.Wait()

	if err != nil {
		return err
	}

	if headErr != nil {
		return headErr
	}

	h.listed = refs

	for _, name := range sortedKeys(refs) {
		_, _ = fmt.Fprintf(out, "%s %s\n", refs[name].sha, name)
	}

	if !forPush {
		if _, ok := refs[head]; ok {
			h.head = head
			_, _ = fmt.Fprintf(out, "@%s HEAD\n", head)
		}
	}

	_, _ = out.WriteString("\n")

	return nil
}

// validRef accepts full ref names that are safe to use in object names: under refs/, no empty, "." or ".." parts.
func validRef(ref string) bool {
	if !strings.HasPrefix(ref, refsPrefix) || strings.ContainsFunc(ref, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return false
	}

	for part := range strings.SplitSeq(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") {
			return false
		}
	}

	return true
}

// parallel calls run for 0..count-1, at most limit at a time; the first error cancels the others and is returned.
func parallel(ctx context.Context, count, limit int, run func(ctx context.Context, index int) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, limit)
	)

	for index := range count {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			err := run(ctx, index)
			if err != nil {
				cancel(err)
			}
		})
	}

	wg.Wait()

	err := context.Cause(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err //nolint:wrapcheck // a run error, already wrapped by run
	}

	return ctx.Err() //nolint:wrapcheck // only set when the caller's context was cancelled
}

// shellQuote quotes a gs:// URL for the gcloud commands in messages: zsh's extended_glob treats '#' as a glob.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// oneLine flattens a message for the single-line protocol replies.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
