// Package store holds the object storage the remote lives in: a Store interface, a GCS implementation and an in-memory fake.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

var (
	// ErrNotFound means the object doesn't exist.
	ErrNotFound = errors.New("object not found")
	// ErrPrecondition means a write-if-absent found the object, or a conditional replace or delete found another generation.
	ErrPrecondition = errors.New("precondition failed")
	// ErrNoCredentials means Application Default Credentials are missing, invalid or expired.
	ErrNoCredentials = errors.New("no Google credentials: run gcloud auth application-default login")
	// ErrBucketNotFound means the bucket itself doesn't exist.
	ErrBucketNotFound = errors.New("bucket not found")
)

// Object describes one stored object.
type Object struct {
	Name       string
	Generation int64
	Created    time.Time
	Size       int64
}

// Store is the minimal object storage API the remote helper needs. Names are full object names within one bucket.
type Store interface {
	// List returns every object whose name starts with prefix, sorted by name.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Read copies the object's content to w and returns its attributes, or ErrNotFound.
	Read(ctx context.Context, name string, w io.Writer) (Object, error)
	// ReadRange copies length bytes from offset of one generation of the object to w, or returns ErrNotFound.
	ReadRange(ctx context.Context, name string, generation, offset, length int64, w io.Writer) error
	// WriteIfAbsent creates the object from r, or returns ErrPrecondition if it already exists.
	WriteIfAbsent(ctx context.Context, name string, r io.Reader, contentType string) (Object, error)
	// ReplaceIfGeneration overwrites the object only if its generation matches, else returns ErrPrecondition.
	ReplaceIfGeneration(ctx context.Context, name string, generation int64, r io.Reader, contentType string) (Object, error)
	// DeleteIfGeneration deletes the object only if its generation matches, else returns ErrPrecondition or ErrNotFound.
	DeleteIfGeneration(ctx context.Context, name string, generation int64) error
}

// CredentialsError wraps a credential failure; its message is the one line users need, the cause stays reachable via Unwrap.
type CredentialsError struct {
	Err error
}

func (e *CredentialsError) Error() string { return ErrNoCredentials.Error() }

// Unwrap returns the underlying cause.
func (e *CredentialsError) Unwrap() error { return e.Err }

// Is makes errors.Is(err, ErrNoCredentials) true.
func (e *CredentialsError) Is(target error) bool { return target == ErrNoCredentials }

// PermissionError is a 403 on a bucket: it names the IAM permission the caller lacks.
type PermissionError struct {
	Bucket     string
	Permission string
	Err        error
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("permission denied on bucket gs://%s: %s is required (e.g. roles/storage.objectUser)",
		e.Bucket, e.Permission)
}

// Unwrap returns the underlying cause.
func (e *PermissionError) Unwrap() error { return e.Err }

// ErrShortRead means a ranged read returned fewer bytes than asked for.
var ErrShortRead = errors.New("short read")

// IAM permissions for each Store operation.
const (
	PermList   = "storage.objects.list"
	PermGet    = "storage.objects.get"
	PermCreate = "storage.objects.create"
	PermDelete = "storage.objects.delete"
)
