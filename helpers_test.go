package fetch_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
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
	method  uint16      // a compression method of its own, for ones Go cannot read
}

// nopCloser lets a test archive be written with a compression method the
// reader does not know: the bytes go in as they are.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

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

		if e.method != 0 {
			h.Method = e.method
			zw.RegisterCompressor(e.method, func(w io.Writer) (io.WriteCloser, error) { return nopCloser{w}, nil })
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

// requireSymlinks skips the test where this machine cannot create symbolic
// links, which on Windows needs a privilege or Developer Mode.
func requireSymlinks(t *testing.T) {
	t.Helper()

	dir := t.TempDir()

	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Skipf("cannot create symbolic links here: %v", err)
	}
}

// hook is a Logger that runs fn once, the first time a line contains
// trigger. Zip logs from the goroutine doing the work, right before the step
// the line announces, so fn runs at a known point of the fetch.
type hook struct {
	trigger string
	fn      func()
	fired   bool
}

func (h *hook) Println(v ...any) {
	if !h.fired && strings.Contains(fmt.Sprintln(v...), h.trigger) {
		h.fired = true
		h.fn()
	}
}

// requireFaultInjection skips where file permissions cannot make the
// operating system refuse a write: Windows, and root, which ignores them.
func requireFaultInjection(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("Windows permissions are not Unix modes")
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
}

// workDir finds the temporary directory a fetch of dir is working in.
func workDir(t *testing.T, dir string) string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "."+filepath.Base(dir)+".fetch-") {
			return filepath.Join(filepath.Dir(dir), e.Name())
		}
	}

	t.Fatalf("no work directory next to %s", dir)

	return ""
}

// readOnly makes p unwritable for the rest of the test and restores it
// afterwards, so the temporary directory can be cleaned up.
func readOnly(t *testing.T, p string, mode fs.FileMode) {
	t.Helper()

	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(p, 0o700) })
}
