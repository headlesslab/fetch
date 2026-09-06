package fetch_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

// entry is one file inside a test archive.
type entry struct {
	name    string
	content string
	mode    fs.FileMode // zero means a regular 0o644 file
	stored  bool        // written uncompressed, to control the archive's size
}

// zipOf builds an archive holding the entries in the order given. Names ending
// in "/" become directory entries.
func zipOf(t *testing.T, entries ...entry) []byte {
	t.Helper()

	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)

	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.stored {
			h.Method = zip.Store
		}

		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}

		h.SetMode(mode)

		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := w.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// host is an ephemeral HTTP server that counts the requests it received.
type host struct {
	*httptest.Server

	hits atomic.Int32
}

// serve starts a host running handler for every request.
func serve(t *testing.T, handler http.HandlerFunc) *host {
	t.Helper()

	h := &host{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(h.Close)

	return h
}

// serveZip serves body with status 200 after an optional delay before the
// headers.
func serveZip(t *testing.T, body []byte, delay time.Duration) *host {
	t.Helper()

	return serve(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}

		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(body)
	})
}

// serveStatus answers every request with the status code and no body.
func serveStatus(t *testing.T, code int) *host {
	t.Helper()

	return serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	})
}

// tree reads every regular file under dir as a map of slash-separated
// relative path to content.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()

	out, err := readTree(dir)
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func readTree(dir string) (map[string]string, error) {
	out := map[string]string{}

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}

		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}

		out[filepath.ToSlash(rel)] = string(b)

		return nil
	})

	return out, err
}

func assertTree(t *testing.T, dir string, want map[string]string) {
	t.Helper()

	got := tree(t, dir)

	if len(got) != len(want) {
		t.Fatalf("tree under %s: got %v, want %v", dir, keys(got), keys(want))
	}

	for name, content := range want {
		if got[name] != content {
			t.Fatalf("%s: got %q, want %q", name, got[name], content)
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

func exists(p string) bool {
	_, err := os.Stat(p)

	return err == nil
}

// assertNoLeftovers fails when parent holds anything but the destination dir
// (when present) and its lock file.
func assertNoLeftovers(t *testing.T, parent, dir string) {
	t.Helper()

	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		name := e.Name()
		if name == filepath.Base(dir) || name == filepath.Base(dir)+".lock" {
			continue
		}

		t.Fatalf("leftover %q in %s", name, parent)
	}
}
