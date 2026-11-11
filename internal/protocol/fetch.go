package protocol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/MaesterZ/git-remote-gs/internal/store"
)

type fetchJob struct {
	sha string
	ref string
}

// fetch downloads, verifies and unbundles the bundle of every requested ref whose object is missing locally.
func (h *Helper) fetch(ctx context.Context, batch []string) error {
	jobs, err := h.parseFetch(batch)
	if err != nil {
		return err
	}

	meter := h.cfg.Log.Meter("Downloading")
	h.down = meter

	defer meter.Finish()

	// Branches go first: their history often holds other refs' objects (merged branches, tags), which then need no download.
	branches, rest := partition(jobs, func(job fetchJob) bool { return strings.HasPrefix(job.ref, headsPrefix) })

	for _, wave := range [][]fetchJob{branches, rest} {
		wave, err = h.stillMissing(ctx, wave)
		if err != nil {
			return err
		}

		err = parallel(ctx, len(wave), h.cfg.FetchJobs, func(ctx context.Context, i int) error {
			return h.fetchOne(ctx, wave[i])
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// parseFetch reads a fetch batch into jobs, one per distinct SHA, with HEAD mapped to the ref it names.
func (h *Helper) parseFetch(batch []string) ([]fetchJob, error) {
	var jobs []fetchJob

	seen := map[string]bool{}

	for _, line := range batch {
		fields := strings.Fields(strings.TrimPrefix(line, "fetch "))
		if len(fields) != 2 || !shaRe.MatchString(fields[0]) {
			return nil, fmt.Errorf("%w: bad fetch command %q", ErrProtocol, line)
		}

		sha, ref := fields[0], fields[1]
		if seen[sha] {
			continue
		}

		seen[sha] = true

		if ref == headObject && h.head != "" {
			ref = h.head
		}

		jobs = append(jobs, fetchJob{sha: sha, ref: ref})
	}

	return jobs, nil
}

// stillMissing drops the jobs whose object is already in the local repository, with a single git process.
func (h *Helper) stillMissing(ctx context.Context, jobs []fetchJob) ([]fetchJob, error) {
	if len(jobs) == 0 {
		return nil, nil
	}

	shas := make([]string, len(jobs))
	for i, job := range jobs {
		shas[i] = job.sha
	}

	missing, err := h.cfg.Git.MissingObjects(ctx, shas)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}

	var out []fetchJob

	for _, job := range jobs {
		if !missing[job.sha] {
			h.cfg.Log.Logf("fetch %s: %s already present", job.ref, job.sha)

			continue
		}

		if !validRef(job.ref) {
			return nil, fmt.Errorf("%w: cannot fetch %q: not a ref under refs/", ErrProtocol, job.ref)
		}

		out = append(out, job)
	}

	return out, nil
}

func partition[T any](items []T, first func(T) bool) ([]T, []T) {
	var matched, others []T

	for _, item := range items {
		if first(item) {
			matched = append(matched, item)
		} else {
			others = append(others, item)
		}
	}

	return matched, others
}

func (h *Helper) fetchOne(ctx context.Context, job fetchJob) error {
	name := h.cfg.Location.Key(job.ref, job.sha+bundleSuffix)
	url := h.cfg.Location.URL(name)

	dir, err := os.MkdirTemp(h.cfg.TempDir, "git-remote-gs-")
	if err != nil {
		return fmt.Errorf("fetch %s: %w", job.ref, err)
	}
	defer os.RemoveAll(dir)

	// MkdirTemp makes the directory 0700 and CreateTemp the file 0600.
	file, err := os.CreateTemp(dir, "*"+bundleSuffix)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", job.ref, err)
	}

	err = h.download(ctx, job, name, file)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}

	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("fetch %s: bundle %s is gone: %w", job.ref, url, errBundleGone)
	case err != nil:
		return fmt.Errorf("fetch %s: download %s: %w", job.ref, url, err)
	}

	err = h.cfg.Git.VerifyBundle(ctx, file.Name())
	if err != nil {
		return fmt.Errorf("fetch %s: bundle %s is invalid: %w", job.ref, url, err)
	}

	err = h.cfg.Git.Unbundle(ctx, file.Name())
	if err != nil {
		return fmt.Errorf("fetch %s: unbundle %s: %w", job.ref, url, err)
	}

	return nil
}

// download reads the bundle into file: as parallel ranged reads when list saw it and it is large, else in one stream.
func (h *Helper) download(ctx context.Context, job fetchJob, name string, file *os.File) error {
	known, ok := h.listed[job.ref]
	if !ok || known.sha != job.sha || known.obj.Size < minParts*h.cfg.PartSize {
		h.cfg.Log.Logf("download %s", h.cfg.Location.URL(name))

		_, err := h.cfg.Store.Read(ctx, name, h.down.Writer(file))

		return err
	}

	obj := known.obj
	parts := int((obj.Size + h.cfg.PartSize - 1) / h.cfg.PartSize)

	h.cfg.Log.Logf("download %s (%d bytes) in %d parts", h.cfg.Location.URL(name), obj.Size, parts)

	// Every part reads the listed generation, so a bundle replaced mid-download fails as gone instead of mixing content.
	return parallel(ctx, parts, h.cfg.FetchJobs, func(ctx context.Context, i int) error {
		offset := int64(i) * h.cfg.PartSize
		length := min(h.cfg.PartSize, obj.Size-offset)

		part := h.down.Writer(io.NewOffsetWriter(file, offset))

		return h.cfg.Store.ReadRange(ctx, obj.Name, obj.Generation, offset, length, part)
	})
}

// sortedKeys returns a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	return keys
}
