package store

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"
)

func TestChunkSize(t *testing.T) {
	t.Parallel()

	file, err := os.Create(filepath.Join(t.TempDir(), "bundle"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = file.Close() })

	_, err = file.Write(make([]byte, 1000))
	if err != nil {
		t.Fatal(err)
	}

	big := bytes.NewReader(make([]byte, googleapi.DefaultUploadChunkSize))

	for name, tt := range map[string]struct {
		r    io.Reader
		want int
	}{
		"small string": {strings.NewReader("refs/heads/main"), 16},
		"small bytes":  {bytes.NewReader(make([]byte, 99)), 100},
		"file":         {file, 1001},
		"large":        {big, googleapi.DefaultUploadChunkSize},
		"unknown size": {io.MultiReader(strings.NewReader("x")), googleapi.DefaultUploadChunkSize},
	} {
		if got := chunkSize(tt.r); got != tt.want {
			t.Errorf("%s: chunkSize = %d, want %d", name, got, tt.want)
		}
	}
}
