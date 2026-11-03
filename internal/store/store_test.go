package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MaesterZ/git-remote-gs/internal/store"
	"github.com/fsouza/fake-gcs-server/fakestorage"
)

// fidelity lists the preconditions a fake honours; the real GCS and Memory honour all of them.
type fidelity struct {
	strictDelete  bool // DeleteIfGeneration with a wrong generation fails
	strictReplace bool // ReplaceIfGeneration with a wrong generation fails
	generations   bool // ReadRange of a replaced generation fails
}

// contract checks the behaviour the helper relies on, on objects under base.
func contract(t *testing.T, s store.Store, base string, fid fidelity) {
	t.Helper()

	ctx := t.Context()

	_, err := s.Read(ctx, base+"missing", &bytes.Buffer{})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Read(missing) = %v, want ErrNotFound", err)
	}

	first, err := s.WriteIfAbsent(ctx, base+"p/a", strings.NewReader("one"), "text/plain")
	if err != nil {
		t.Fatalf("WriteIfAbsent: %v", err)
	}

	if first.Generation <= 0 {
		t.Fatalf("generation %d, want > 0", first.Generation)
	}

	_, err = s.WriteIfAbsent(ctx, base+"p/a", strings.NewReader("two"), "text/plain")
	if !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("second WriteIfAbsent = %v, want ErrPrecondition", err)
	}

	var buf bytes.Buffer

	got, err := s.Read(ctx, base+"p/a", &buf)
	if err != nil || buf.String() != "one" || got.Generation != first.Generation {
		t.Fatalf("Read = %q gen %d, %v; want %q gen %d", buf.String(), got.Generation, err, "one", first.Generation)
	}

	if got.Size != 3 || got.Created.IsZero() {
		t.Fatalf("Read attrs = %+v, want size 3 and a creation time", got)
	}

	first = contractReplace(t, s, base+"p/a", first, fid)

	for _, name := range []string{"p/b/c", "q/d"} {
		_, err := s.WriteIfAbsent(ctx, base+name, strings.NewReader(name), "")
		if err != nil {
			t.Fatalf("WriteIfAbsent(%s): %v", name, err)
		}
	}

	objs, err := s.List(ctx, base+"p/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var names []string
	for _, o := range objs {
		names = append(names, strings.TrimPrefix(o.Name, base))
	}

	if strings.Join(names, ",") != "p/a,p/b/c" {
		t.Fatalf("List(p/) = %v", names)
	}

	if fid.strictDelete {
		err := s.DeleteIfGeneration(ctx, base+"p/a", first.Generation+1000)
		if !errors.Is(err, store.ErrPrecondition) {
			t.Fatalf("DeleteIfGeneration(wrong gen) = %v, want ErrPrecondition", err)
		}
	}

	err = s.DeleteIfGeneration(ctx, base+"p/a", first.Generation)
	if err != nil {
		t.Fatalf("DeleteIfGeneration: %v", err)
	}

	err = s.DeleteIfGeneration(ctx, base+"p/a", first.Generation)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteIfGeneration(deleted) = %v, want ErrNotFound", err)
	}

	_, err = s.WriteIfAbsent(ctx, base+"p/a", strings.NewReader("three"), "")
	if err != nil {
		t.Fatalf("WriteIfAbsent after delete: %v", err)
	}
}

// contractReplace checks conditional replaces and ranged reads of name, and returns its new attributes.
func contractReplace(t *testing.T, s store.Store, name string, first store.Object, fid fidelity) store.Object {
	t.Helper()

	ctx := t.Context()

	if fid.strictReplace {
		_, err := s.ReplaceIfGeneration(ctx, name, first.Generation+1000, strings.NewReader("x"), "")
		if !errors.Is(err, store.ErrPrecondition) {
			t.Fatalf("ReplaceIfGeneration(wrong gen) = %v, want ErrPrecondition", err)
		}

		_, err = s.ReplaceIfGeneration(ctx, name+"-missing", 1, strings.NewReader("x"), "")
		if !errors.Is(err, store.ErrPrecondition) {
			t.Fatalf("ReplaceIfGeneration(missing) = %v, want ErrPrecondition", err)
		}
	}

	replaced, err := s.ReplaceIfGeneration(ctx, name, first.Generation, strings.NewReader("ONE"), "text/plain")
	if err != nil || replaced.Generation == first.Generation {
		t.Fatalf("ReplaceIfGeneration = gen %d, %v; want a new generation", replaced.Generation, err)
	}

	var part bytes.Buffer

	err = s.ReadRange(ctx, name, replaced.Generation, 1, 2, &part)
	if err != nil || part.String() != "NE" {
		t.Fatalf("ReadRange = %q, %v; want %q", part.String(), err, "NE")
	}

	err = s.ReadRange(ctx, name, replaced.Generation, 2, 5, &bytes.Buffer{})
	if !errors.Is(err, store.ErrShortRead) {
		t.Fatalf("ReadRange past the end = %v, want ErrShortRead", err)
	}

	if fid.generations {
		err = s.ReadRange(ctx, name, first.Generation, 0, 1, &bytes.Buffer{})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("ReadRange(old generation) = %v, want ErrNotFound", err)
		}
	}

	return replaced
}

func TestMemoryContract(t *testing.T) {
	t.Parallel()
	contract(t, store.NewMemory(), "", fidelity{strictDelete: true, strictReplace: true, generations: true})
}

// fake-gcs-server doesn't honour every precondition; the strict checks it skips run on Memory.
func TestGCSContractOnFakeServer(t *testing.T) {
	t.Parallel()

	server, err := fakestorage.NewServerWithOptions(fakestorage.Options{NoListener: true})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(server.Stop)
	server.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: "contract"})

	contract(t, store.NewGCSWithClient(server.Client(), "contract"), "", fidelity{strictReplace: true, generations: true})
}

// TestGCSContractOnRealBucket runs the contract on GIT_REMOTE_GS_TEST_BUCKET under a unique prefix, then cleans up.
func TestGCSContractOnRealBucket(t *testing.T) {
	t.Parallel()

	target := os.Getenv("GIT_REMOTE_GS_TEST_BUCKET")
	if target == "" {
		t.Skip("set GIT_REMOTE_GS_TEST_BUCKET=gs://BUCKET[/PREFIX] to run against a real bucket")
	}

	run := fmt.Sprintf("git-remote-gs-contract-%d-%04d", time.Now().Unix(), rand.IntN(10000)) //nolint:gosec // not security

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

	contract(t, gcs, loc.Prefix+"/", fidelity{strictDelete: true, strictReplace: true, generations: true})
}

func TestGCSMissingBucket(t *testing.T) {
	t.Parallel()

	server, err := fakestorage.NewServerWithOptions(fakestorage.Options{NoListener: true})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(server.Stop)

	_, err = store.NewGCSWithClient(server.Client(), "nope").List(t.Context(), "")
	if !errors.Is(err, store.ErrBucketNotFound) {
		t.Fatalf("List on missing bucket = %v, want ErrBucketNotFound", err)
	}
}

func TestErrorMessages(t *testing.T) {
	t.Parallel()

	cred := &store.CredentialsError{Err: errors.ErrUnsupported}
	if !errors.Is(cred, store.ErrNoCredentials) {
		t.Error("CredentialsError doesn't match ErrNoCredentials")
	}

	if got, want := cred.Error(), "no Google credentials: run gcloud auth application-default login"; got != want {
		t.Errorf("CredentialsError = %q, want %q", got, want)
	}

	perm := &store.PermissionError{Bucket: "b", Permission: store.PermCreate}
	if got := perm.Error(); !strings.Contains(got, "gs://b") || !strings.Contains(got, "storage.objects.create") {
		t.Errorf("PermissionError = %q", got)
	}
}
