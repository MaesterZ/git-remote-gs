package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// GCS is a Store backed by one Google Cloud Storage bucket, authenticated with Application Default Credentials.
type GCS struct {
	client *storage.Client
	bucket *storage.BucketHandle
	name   string
}

var _ Store = (*GCS)(nil)

// NewGCS connects to bucket. STORAGE_EMULATOR_HOST, if set, points the client at an emulator without credentials.
func NewGCS(ctx context.Context, bucket string) (*GCS, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, &CredentialsError{Err: err}
	}

	return NewGCSWithClient(client, bucket), nil
}

// NewGCSWithClient wraps an existing client, e.g. one from a fake server in tests.
func NewGCSWithClient(client *storage.Client, bucket string) *GCS {
	return &GCS{client: client, bucket: client.Bucket(bucket), name: bucket}
}

// Close releases the client.
func (g *GCS) Close() error {
	err := g.client.Close()
	if err != nil {
		return fmt.Errorf("close storage client: %w", err)
	}

	return nil
}

// List implements Store.
func (g *GCS) List(ctx context.Context, prefix string) ([]Object, error) {
	query := &storage.Query{Prefix: prefix}

	err := query.SetAttrSelection([]string{"Name", "Generation", "Created", "Size"})
	if err != nil {
		return nil, fmt.Errorf("list gs://%s/%s: %w", g.name, prefix, err)
	}

	var out []Object

	it := g.bucket.Objects(ctx, query)

	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}

		if err != nil {
			return nil, g.classify(err, "list", prefix, PermList)
		}

		out = append(out, Object{Name: attrs.Name, Generation: attrs.Generation, Created: attrs.Created, Size: attrs.Size})
	}
}

// Read implements Store.
func (g *GCS) Read(ctx context.Context, name string, w io.Writer) (Object, error) {
	reader, err := g.bucket.Object(name).NewReader(ctx)
	if err != nil {
		return Object{}, g.classify(err, "read", name, PermGet)
	}
	defer reader.Close()

	_, err = io.Copy(w, reader)
	if err != nil {
		return Object{}, g.classify(err, "read", name, PermGet)
	}

	// An object is never modified in place, so its last-modified time is its creation time.
	return Object{Name: name, Generation: reader.Attrs.Generation, Created: reader.Attrs.LastModified, Size: reader.Attrs.Size}, nil
}

// ReadRange implements Store. Ranged reads skip the client's CRC32C check; git verifies the pack checksum on unbundle.
func (g *GCS) ReadRange(ctx context.Context, name string, generation, offset, length int64, w io.Writer) error {
	reader, err := g.bucket.Object(name).Generation(generation).NewRangeReader(ctx, offset, length)
	if err != nil {
		return g.classify(err, "read", name, PermGet)
	}
	defer reader.Close()

	copied, err := io.Copy(w, reader)
	if err != nil {
		return g.classify(err, "read", name, PermGet)
	}

	if copied != length {
		return fmt.Errorf("read gs://%s/%s at %d: got %d of %d bytes: %w", g.name, name, offset, copied, length, ErrShortRead)
	}

	return nil
}

// WriteIfAbsent implements Store.
func (g *GCS) WriteIfAbsent(ctx context.Context, name string, r io.Reader, contentType string) (Object, error) {
	return g.write(ctx, name, storage.Conditions{DoesNotExist: true}, r, contentType)
}

// ReplaceIfGeneration implements Store.
func (g *GCS) ReplaceIfGeneration(ctx context.Context, name string, generation int64, r io.Reader, contentType string) (Object, error) {
	if generation <= 0 {
		return Object{}, fmt.Errorf("replace gs://%s/%s: generation %d: %w", g.name, name, generation, ErrPrecondition)
	}

	return g.write(ctx, name, storage.Conditions{GenerationMatch: generation}, r, contentType)
}

// DeleteIfGeneration implements Store.
func (g *GCS) DeleteIfGeneration(ctx context.Context, name string, generation int64) error {
	if generation <= 0 {
		return fmt.Errorf("delete gs://%s/%s: generation %d: %w", g.name, name, generation, ErrPrecondition)
	}

	err := g.bucket.Object(name).If(storage.Conditions{GenerationMatch: generation}).Delete(ctx)
	if err != nil {
		return g.classify(err, "delete", name, PermDelete)
	}

	return nil
}

func (g *GCS) write(ctx context.Context, name string, cond storage.Conditions, r io.Reader, contentType string) (Object, error) {
	// Cancelling the context is the only way to abort a storage.Writer without committing the object.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	writer := g.bucket.Object(name).If(cond).NewWriter(ctx)
	writer.ContentType = contentType
	writer.ChunkSize = chunkSize(r)

	_, err := io.Copy(writer, r)
	if err != nil {
		cancel()

		_ = writer.Close()

		return Object{}, g.classify(err, "write", name, PermCreate)
	}

	err = writer.Close()
	if err != nil {
		return Object{}, g.classify(err, "write", name, PermCreate)
	}

	attrs := writer.Attrs()

	return Object{Name: attrs.Name, Generation: attrs.Generation, Created: attrs.Created, Size: attrs.Size}, nil
}

// chunkSize fits the upload buffer to inputs of known size: the 16 MiB default is allocated even for a 60-byte lock.
func chunkSize(r io.Reader) int {
	size := int64(-1)

	switch v := r.(type) {
	case interface{ Len() int }:
		size = int64(v.Len())
	case interface{ Stat() (fs.FileInfo, error) }:
		info, err := v.Stat()
		if err == nil {
			size = info.Size()
		}
	}

	if size < 0 || size >= googleapi.DefaultUploadChunkSize {
		return googleapi.DefaultUploadChunkSize
	}

	// One byte more keeps the upload a single request; the client rounds it up to a multiple of 256 KiB.
	return int(size) + 1
}

// classify maps client errors onto the package's sentinel errors and types.
func (g *GCS) classify(err error, action, name, perm string) error {
	var (
		apiErr  *googleapi.Error
		authErr *auth.Error
	)

	switch {
	case errors.Is(err, storage.ErrBucketNotExist):
		return fmt.Errorf("%w: gs://%s", ErrBucketNotFound, g.name)
	case errors.Is(err, storage.ErrObjectNotExist):
		return fmt.Errorf("%s gs://%s/%s: %w", action, g.name, name, ErrNotFound)
	case errors.As(err, &authErr):
		return &CredentialsError{Err: err}
	case errors.As(err, &apiErr):
		switch apiErr.Code {
		case http.StatusPreconditionFailed:
			return fmt.Errorf("%s gs://%s/%s: %w", action, g.name, name, ErrPrecondition)
		case http.StatusNotFound:
			return fmt.Errorf("%s gs://%s/%s: %w", action, g.name, name, ErrNotFound)
		case http.StatusUnauthorized:
			return &CredentialsError{Err: err}
		case http.StatusForbidden:
			return &PermissionError{Bucket: g.name, Permission: perm, Err: err}
		}
	}

	return fmt.Errorf("%s gs://%s/%s: %w", action, g.name, name, err)
}
