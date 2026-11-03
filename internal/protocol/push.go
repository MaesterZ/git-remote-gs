package protocol

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/MaesterZ/git-remote-gs/internal/store"
)

// cleanupTimeout bounds work that must finish even after Ctrl-C: releasing a lock, deleting a replaced bundle.
const cleanupTimeout = 30 * time.Second

// rejectError is a refused push for one ref; its message goes after `error <dst> `.
type rejectError struct{ msg string }

func (e *rejectError) Error() string { return e.msg }

func reject(format string, args ...any) error {
	return &rejectError{msg: fmt.Sprintf(format, args...)}
}

// pushCmd is one `push [+]<src>:<dst>` line; an empty src deletes dst.
type pushCmd struct {
	src, dst string
	force    bool
}

// errNotRun is the result of a push the batch was interrupted before.
var errNotRun = reject("interrupted")

// push runs the commands of a batch in parallel but independently, and returns one result line per command, in order.
func (h *Helper) push(ctx context.Context, batch []string) []string {
	cmds := make([]pushCmd, len(batch))
	errs := make([]error, len(batch))

	for i, line := range batch {
		cmds[i], errs[i] = parsePush(line)
	}

	_ = parallel(ctx, len(batch), h.cfg.PushJobs, func(ctx context.Context, i int) error {
		if errors.Is(errs[i], errNotRun) {
			errs[i] = h.pushOne(ctx, cmds[i])
		}

		return nil // one ref's failure must not cancel the others
	})

	results := make([]string, len(batch))

	for i, err := range errs {
		if err != nil {
			h.cfg.Log.Logf("push %s: %v", cmds[i].dst, err)
			results[i] = fmt.Sprintf("error %s %s", cmds[i].dst, oneLine(err.Error()))
		} else {
			results[i] = "ok " + cmds[i].dst
		}
	}

	// Done after the batch, so the first branch in batch order becomes HEAD whatever order the parallel pushes end in.
	for i, cmd := range cmds {
		if errs[i] == nil && cmd.src != "" && strings.HasPrefix(cmd.dst, headsPrefix) {
			h.initHead(ctx, cmd.dst)

			break
		}
	}

	return results
}

// parsePush parses a push line; a valid command comes back with errNotRun, to be replaced by its outcome.
func parsePush(line string) (pushCmd, error) {
	spec := strings.TrimPrefix(line, "push ")
	force := strings.HasPrefix(spec, "+")
	spec = strings.TrimPrefix(spec, "+")

	src, dst, ok := strings.CutLast(spec, ":")
	if !ok {
		return pushCmd{dst: spec}, reject("invalid refspec")
	}

	if !validRef(dst) {
		return pushCmd{dst: dst}, reject("invalid remote ref name, expected refs/...")
	}

	return pushCmd{src: src, dst: dst, force: force}, errNotRun
}

func (h *Helper) pushOne(ctx context.Context, cmd pushCmd) error {
	if cmd.src == "" {
		return h.deleteRef(ctx, cmd.dst)
	}

	return h.pushRef(ctx, cmd)
}

func (h *Helper) pushRef(ctx context.Context, cmd pushCmd) error {
	before, err := h.readRef(ctx, cmd.dst)
	if err != nil {
		return err
	}

	if len(before.bundles) > 1 {
		return h.multipleBundles(before.bundles)
	}

	sha, err := h.cfg.Git.Resolve(ctx, cmd.src)
	if err != nil {
		return reject("cannot resolve %s: %v", cmd.src, err)
	}

	// A 64-hex bundle name would be invisible to list, so the ref would never appear on the remote.
	if !shaRe.MatchString(sha) {
		return reject("SHA-256 repositories aren't supported: bundles are named by 40-hex SHA-1")
	}

	remoteSHA := before.sha()
	if sha == remoteSHA {
		h.cfg.Log.Logf("push %s: already at %s", cmd.dst, sha)

		return nil
	}

	// A protected ref accepts only fast-forwards, even when forced, like git-remote-s3 and git-remote-gcs.
	if remoteSHA != "" && (!cmd.force || before.protected) {
		err = h.checkFastForward(ctx, remoteSHA, sha, cmd.force)
		if err != nil {
			return err
		}
	}

	return h.bundleAndUpload(ctx, cmd, sha, before)
}

func (h *Helper) checkFastForward(ctx context.Context, remoteSHA, sha string, forced bool) error {
	exists, err := h.cfg.Git.ObjectExists(ctx, remoteSHA)
	if err != nil {
		return err
	}

	if !exists {
		return reject("fetch first")
	}

	ancestor, err := h.cfg.Git.IsAncestor(ctx, remoteSHA, sha)
	if err != nil {
		return err
	}

	switch {
	case ancestor:
		return nil
	case forced:
		return reject("protected branch, force push not allowed")
	default:
		return reject("non-fast-forward")
	}
}

// bundleAndUpload creates the bundle before taking the lock, so the lock is held only for the upload.
func (h *Helper) bundleAndUpload(ctx context.Context, cmd pushCmd, sha string, before refState) error {
	dir, err := os.MkdirTemp(h.cfg.TempDir, "git-remote-gs-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, sha+bundleSuffix)

	err = h.cfg.Git.CreateBundle(ctx, sha, path)
	if err != nil {
		return fmt.Errorf("create bundle: %w", err)
	}

	return h.withLock(ctx, cmd.dst, func(ctx context.Context) error {
		return h.uploadLocked(ctx, cmd, sha, path, before)
	})
}

// uploadLocked runs with the ref's lock held: re-check, upload, then drop the old bundle.
func (h *Helper) uploadLocked(ctx context.Context, cmd pushCmd, sha, path string, before refState) error {
	err := h.recheckLocked(ctx, cmd, sha, before)
	if err != nil {
		return err
	}

	err = h.upload(ctx, h.cfg.Location.Key(cmd.dst, sha+bundleSuffix), path)
	if err != nil {
		return err
	}

	if len(before.bundles) == 1 {
		return h.deleteOldBundle(ctx, before.bundles[0].obj, sha)
	}

	return nil
}

// recheckLocked lists the ref again under the lock: it must be unchanged since the push decided what to do.
func (h *Helper) recheckLocked(ctx context.Context, cmd pushCmd, sha string, before refState) error {
	now, err := h.readRef(ctx, cmd.dst)
	if err != nil {
		return err
	}

	if len(now.bundles) > 1 {
		return h.multipleBundles(now.bundles)
	}

	if !sameBundles(before.bundles, now.bundles) {
		return reject("stale remote, fetch and retry")
	}

	// The ref may have been protected since the first check, which skipped ancestry for a force push.
	if now.protected && !before.protected && cmd.force && before.sha() != "" {
		return h.checkFastForward(ctx, before.sha(), sha, true)
	}

	return nil
}

func (h *Helper) upload(ctx context.Context, name, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open bundle: %w", err)
	}
	defer file.Close()

	h.cfg.Log.Logf("upload %s", h.cfg.Location.URL(name))

	_, err = h.cfg.Store.WriteIfAbsent(ctx, name, file, "application/octet-stream")
	if errors.Is(err, store.ErrPrecondition) {
		return reject("stale remote, fetch and retry: %s appeared during the push", h.cfg.Location.URL(name))
	}

	return err
}

// deleteOldBundle runs only after the new bundle is uploaded, so the bucket always holds a complete copy of the ref.
func (h *Helper) deleteOldBundle(ctx context.Context, old store.Object, sha string) error {
	// The new bundle is in place: finish even if the lock was just lost or Ctrl-C was hit, or the ref keeps two bundles.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	url := h.cfg.Location.URL(old.Name)

	err := h.cfg.Store.DeleteIfGeneration(ctx, old.Name, old.Generation)

	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrPrecondition):
		// Only a push that took our lock over, or a tool that doesn't lock, changes the ref while we hold the lock.
		return reject("pushed %s, but another push changed the ref at the same time (%s was already replaced): "+
			"fetch and check the ref, then push again", sha, url)
	default:
		return reject("pushed %s, but could not delete the old bundle %s (%v): remove it with gcloud storage rm %s",
			sha, url, err, shellQuote(url))
	}
}

// initHead creates HEAD on the first branch push; losing the race to another pusher is fine.
func (h *Helper) initHead(ctx context.Context, ref string) {
	_, err := h.cfg.Store.WriteIfAbsent(ctx, h.cfg.Location.Key(headObject), strings.NewReader(ref), "text/plain")

	switch {
	case err == nil:
		h.cfg.Log.Logf("HEAD set to %s", ref)
	case !errors.Is(err, store.ErrPrecondition):
		h.cfg.Log.Warnf("could not set the remote HEAD to %s: %v", ref, err)
	}
}

func (h *Helper) deleteRef(ctx context.Context, dst string) error {
	head, err := h.readHead(ctx)
	if err != nil {
		return err
	}

	if head == dst {
		return reject("refusing to delete the remote HEAD branch")
	}

	before, err := h.readRef(ctx, dst)
	if err != nil {
		return err
	}

	if before.protected {
		return reject("refusing to delete a protected branch")
	}

	if len(before.bundles) == 0 {
		return reject("remote ref does not exist")
	}

	return h.withLock(ctx, dst, func(ctx context.Context) error {
		return h.deleteLocked(ctx, dst, before)
	})
}

// deleteLocked runs with the ref's lock held: re-check, then delete the ref's bundles.
func (h *Helper) deleteLocked(ctx context.Context, dst string, before refState) error {
	now, err := h.readRef(ctx, dst)
	if err != nil {
		return err
	}

	if now.protected {
		return reject("refusing to delete a protected branch")
	}

	if !sameBundles(before.bundles, now.bundles) {
		return reject("stale remote, fetch and retry")
	}

	for _, found := range now.bundles {
		h.cfg.Log.Logf("delete %s", h.cfg.Location.URL(found.obj.Name))

		err = h.cfg.Store.DeleteIfGeneration(ctx, found.obj.Name, found.obj.Generation)
		if err != nil {
			return err
		}
	}

	return nil
}

func (h *Helper) multipleBundles(bundles []bundle) error {
	urls := make([]string, len(bundles))
	for i, found := range bundles {
		urls[i] = shellQuote(h.cfg.Location.URL(found.obj.Name))
	}

	return reject("several bundles for one ref, keep the right one and remove the others with gcloud storage rm: %s",
		strings.Join(urls, " "))
}

// sameBundles reports whether two listings of one ref hold the same objects at the same generations.
func sameBundles(a, b []bundle) bool {
	return slices.EqualFunc(a, b, func(x, y bundle) bool {
		return x.obj.Name == y.obj.Name && x.obj.Generation == y.obj.Generation
	})
}
