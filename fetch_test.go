package fetch_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/headlesslab/fetch"
)

var sample = []entry{
	{name: "a.txt", content: "alpha"},
	{name: "sub/", content: ""},
	{name: "sub/b.txt", content: "beta"},
}

var sampleTree = map[string]string{"a.txt": "alpha", "sub/b.txt": "beta"}

func TestZipExtractsTheArchiveIntoDir(t *testing.T) {
	archive := zipOf(t, sample...)
	h := serveZip(t, archive, 0)
	dir := filepath.Join(t.TempDir(), "tool")

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/tool.zip"})
	if err != nil {
		t.Fatal(err)
	}

	assertTree(t, dir, sampleTree)
	assertNoLeftovers(t, filepath.Dir(dir), dir)
}

// marked builds an archive whose single file names the host that served it.
func marked(t *testing.T, mark string) []byte {
	t.Helper()

	return zipOf(t, entry{name: "origin.txt", content: mark})
}

func TestFirstResponderWins(t *testing.T) {
	slow := serveZip(t, marked(t, "slow"), 500*time.Millisecond)
	fast := serveZip(t, marked(t, "fast"), 0)
	dir := filepath.Join(t.TempDir(), "tool")

	err := fetch.Zip(context.Background(), dir, []string{slow.URL + "/a.zip", fast.URL + "/a.zip"})
	if err != nil {
		t.Fatal(err)
	}

	assertTree(t, dir, map[string]string{"origin.txt": "fast"})
}

func TestFailingHostsFallBack(t *testing.T) {
	notFound := serveStatus(t, http.StatusNotFound)
	slow := serveZip(t, marked(t, "slow"), 300*time.Millisecond)
	dir := filepath.Join(t.TempDir(), "tool")

	err := fetch.Zip(context.Background(), dir, []string{notFound.URL + "/a.zip", slow.URL + "/a.zip"})
	if err != nil {
		t.Fatal(err)
	}

	assertTree(t, dir, map[string]string{"origin.txt": "slow"})
}

// bigMarked builds an archive larger than a probe, so a host can pass the
// probe and still fail the download.
func bigMarked(t *testing.T, mark string) []byte {
	t.Helper()

	return zipOf(t,
		entry{name: "origin.txt", content: mark},
		entry{name: "pad.bin", content: strings.Repeat(mark+"\n", 200<<10/(len(mark)+1)), stored: true},
	)
}

func TestBrokenDownloadFallsBackToTheNextResponder(t *testing.T) {
	brokenBody := bigMarked(t, "broken")
	broken := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(brokenBody)))
		_, _ = w.Write(brokenBody[:len(brokenBody)/2])
	})
	good := serveZip(t, bigMarked(t, "good"), 500*time.Millisecond)
	dir := filepath.Join(t.TempDir(), "tool")

	err := fetch.Zip(context.Background(), dir, []string{broken.URL + "/a.zip", good.URL + "/a.zip"})
	if err != nil {
		t.Fatal(err)
	}

	if got := tree(t, dir)["origin.txt"]; got != "good" {
		t.Fatalf("origin.txt: got %q, want %q", got, "good")
	}

	// The broken host passed the probe and was downloaded from before the
	// fetch moved on: one probe request, one download request.
	if hits := broken.hits.Load(); hits != 2 {
		t.Fatalf("broken host got %d requests, want 2 (probe and download)", hits)
	}
}

func TestHangingHostDoesNotDelayTheFetch(t *testing.T) {
	hang := serveZip(t, marked(t, "hang"), time.Minute)
	good := serveZip(t, marked(t, "good"), 0)
	dir := filepath.Join(t.TempDir(), "tool")

	start := time.Now()

	err := fetch.Zip(context.Background(), dir, []string{hang.URL + "/a.zip", good.URL + "/a.zip"})
	if err != nil {
		t.Fatal(err)
	}

	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("fetch took %s waiting for the hanging host", took)
	}

	assertTree(t, dir, map[string]string{"origin.txt": "good"})
}

func TestEveryHostFailingNamesEachURL(t *testing.T) {
	notFound := serveStatus(t, http.StatusNotFound)
	refused := serveStatus(t, http.StatusOK)
	refused.Close() // the port is released, so the URL refuses connections
	dir := filepath.Join(t.TempDir(), "tool")
	urls := []string{notFound.URL + "/a.zip", refused.URL + "/a.zip"}

	err := fetch.Zip(context.Background(), dir, urls)
	if err == nil {
		t.Fatal("expected an error")
	}

	for _, u := range urls {
		if !strings.Contains(err.Error(), u) {
			t.Errorf("error %q does not name %s", err, u)
		}
	}

	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not carry the HTTP status", err)
	}

	if exists(dir) {
		t.Fatalf("%s exists after a failed fetch", dir)
	}

	assertNoLeftovers(t, filepath.Dir(dir), dir)
}

func TestHashMismatchFailsBeforeTheArchiveIsOpened(t *testing.T) {
	// Not a zip at all: had extraction been attempted, its error would show.
	garbage := []byte("this is not an archive")
	h := serveZip(t, garbage, 0)
	dir := filepath.Join(t.TempDir(), "tool")
	wrong := sha256Hex([]byte("something else"))

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"}, fetch.WithSHA256(wrong))

	var mismatch *fetch.HashMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("got %v, want a *fetch.HashMismatchError", err)
	}

	if mismatch.Expected != wrong || mismatch.Actual != sha256Hex(garbage) {
		t.Fatalf("got expected %q actual %q, want %q and %q", mismatch.Expected, mismatch.Actual, wrong, sha256Hex(garbage))
	}

	for _, want := range []string{wrong, sha256Hex(garbage), h.URL} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}

	if exists(dir) {
		t.Fatalf("%s exists after a hash mismatch", dir)
	}

	assertNoLeftovers(t, filepath.Dir(dir), dir)
}

func TestMatchingHashExtracts(t *testing.T) {
	archive := zipOf(t, sample...)
	h := serveZip(t, archive, 0)
	dir := filepath.Join(t.TempDir(), "tool")

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"}, fetch.WithSHA256(strings.ToUpper(sha256Hex(archive))))
	if err != nil {
		t.Fatal(err)
	}

	assertTree(t, dir, sampleTree)
}

func TestMalformedExpectedHashIsRejectedBeforeAnyRequest(t *testing.T) {
	h := serveZip(t, zipOf(t, sample...), 0)
	dir := filepath.Join(t.TempDir(), "tool")

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"}, fetch.WithSHA256("not-a-digest"))
	if err == nil {
		t.Fatal("expected an error")
	}

	if hits := h.hits.Load(); hits != 0 {
		t.Fatalf("host got %d requests, want none", hits)
	}
}

func TestHashMismatchFallsBackToTheNextResponder(t *testing.T) {
	good := bigMarked(t, "good")
	tampered := serveZip(t, bigMarked(t, "tampered"), 0)
	origin := serveZip(t, good, 200*time.Millisecond)
	dir := filepath.Join(t.TempDir(), "tool")

	err := fetch.Zip(context.Background(), dir, []string{tampered.URL + "/a.zip", origin.URL + "/a.zip"}, fetch.WithSHA256(sha256Hex(good)))
	if err != nil {
		t.Fatal(err)
	}

	if got := tree(t, dir)["origin.txt"]; got != "good" {
		t.Fatalf("origin.txt: got %q, want %q", got, "good")
	}
}

func TestConcurrentFetchersSerializeAndBothSucceed(t *testing.T) {
	h := serveZip(t, zipOf(t, sample...), 300*time.Millisecond)
	dir := filepath.Join(t.TempDir(), "tool")
	urls := []string{h.URL + "/a.zip"}

	first := make(chan error, 1)

	go func() { first <- fetch.Zip(context.Background(), dir, urls) }()

	time.Sleep(50 * time.Millisecond) // let the first fetcher take the lock

	if err := fetch.Zip(context.Background(), dir, urls); err != nil {
		t.Fatalf("second fetcher: %v", err)
	}

	if err := <-first; err != nil {
		t.Fatalf("first fetcher: %v", err)
	}

	assertTree(t, dir, sampleTree)

	// One probe and one download: the fetcher that waited on the lock found
	// the directory in place and made no request of its own.
	if hits := h.hits.Load(); hits != 2 {
		t.Fatalf("host got %d requests, want 2", hits)
	}
}

func TestExistingDestinationIsKeptWithoutARequest(t *testing.T) {
	h := serveZip(t, zipOf(t, sample...), 0)
	dir := filepath.Join(t.TempDir(), "tool")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "mine.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"}); err != nil {
		t.Fatal(err)
	}

	assertTree(t, dir, map[string]string{"mine.txt": "kept"})

	if hits := h.hits.Load(); hits != 0 {
		t.Fatalf("host got %d requests, want none", hits)
	}
}

func TestLockIsReleasedAfterAFailedFetch(t *testing.T) {
	notFound := serveStatus(t, http.StatusNotFound)
	good := serveZip(t, zipOf(t, sample...), 0)
	dir := filepath.Join(t.TempDir(), "tool")

	if err := fetch.Zip(context.Background(), dir, []string{notFound.URL + "/a.zip"}); err == nil {
		t.Fatal("expected the first fetch to fail")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := fetch.Zip(ctx, dir, []string{good.URL + "/a.zip"}); err != nil {
		t.Fatalf("second fetch: %v", err)
	}

	assertTree(t, dir, sampleTree)
}

// serveTrickle serves body in small chunks with a pause between them, so a
// download stays in flight long enough to be interrupted or observed.
func serveTrickle(t *testing.T, body []byte, chunk int, pause time.Duration, withLength bool) *host {
	t.Helper()

	return serve(t, func(w http.ResponseWriter, r *http.Request) {
		if withLength {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		}

		flusher, _ := w.(http.Flusher)

		for off := 0; off < len(body); off += chunk {
			end := min(off+chunk, len(body))

			if _, err := w.Write(body[off:end]); err != nil {
				return
			}

			if flusher != nil {
				flusher.Flush()
			}

			select {
			case <-time.After(pause):
			case <-r.Context().Done():
				return
			}
		}
	})
}

func TestCancelledContextAbortsAndLeavesNothing(t *testing.T) {
	archive := bigMarked(t, "slow")
	h := serveTrickle(t, archive, 1<<10, 20*time.Millisecond, true) // ~4 s for the whole body
	dir := filepath.Join(t.TempDir(), "tool")

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)

	err := fetch.Zip(ctx, dir, []string{h.URL + "/a.zip"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}

	if exists(dir) {
		t.Fatalf("%s exists after a cancelled fetch", dir)
	}

	assertNoLeftovers(t, filepath.Dir(dir), dir)
}

func TestAlreadyCancelledContextFailsWithoutFetching(t *testing.T) {
	h := serveZip(t, zipOf(t, sample...), 0)
	dir := filepath.Join(t.TempDir(), "tool")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := fetch.Zip(ctx, dir, []string{h.URL + "/a.zip"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}

	if exists(dir) {
		t.Fatalf("%s exists after a cancelled fetch", dir)
	}
}

func TestDestinationAppearsCompleteOrNotAtAll(t *testing.T) {
	// Many files, trickled in, so an extraction in place would be caught
	// half-written by the watcher below.
	entries := make([]entry, 0, 300)
	want := map[string]string{}

	for i := 0; i < 300; i++ {
		name := fmt.Sprintf("d%d/f%d.txt", i%10, i)
		content := strings.Repeat(fmt.Sprintf("%03d", i), 700)
		entries = append(entries, entry{name: name, content: content, stored: true})
		want[name] = content
	}

	h := serveTrickle(t, zipOf(t, entries...), 64<<10, 5*time.Millisecond, true)
	dir := filepath.Join(t.TempDir(), "tool")

	stop := make(chan struct{})
	partial := make(chan string, 1)

	go func() {
		defer close(partial)

		for {
			select {
			case <-stop:
				return
			default:
			}

			// A read that fails is not an observation; a read that succeeds
			// with fewer files than the archive holds is a half-written dir.
			if got, err := readTree(dir); err == nil && len(got) != len(want) {
				select {
				case partial <- fmt.Sprintf("saw %d of %d files", len(got), len(want)):
				default:
				}
			}

			time.Sleep(time.Millisecond)
		}
	}()

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"})
	close(stop)

	if err != nil {
		t.Fatal(err)
	}

	if msg, ok := <-partial; ok {
		t.Fatalf("destination observed half-written: %s", msg)
	}

	assertTree(t, dir, want)
}

func TestStripFirstDirMakesTheNestedFolderTheDestination(t *testing.T) {
	nested := []entry{
		{name: "tool-1.0/", content: ""},
		{name: "tool-1.0/a.txt", content: "alpha"},
		{name: "tool-1.0/sub/b.txt", content: "beta"},
	}
	implied := []entry{ // no directory entries at all, as some archivers write
		{name: "tool-1.0/a.txt", content: "alpha"},
		{name: "tool-1.0/sub/b.txt", content: "beta"},
	}

	for name, entries := range map[string][]entry{"explicit": nested, "implied": implied} {
		t.Run(name, func(t *testing.T) {
			h := serveZip(t, zipOf(t, entries...), 0)
			dir := filepath.Join(t.TempDir(), "tool")

			err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"}, fetch.WithStripFirstDir())
			if err != nil {
				t.Fatal(err)
			}

			assertTree(t, dir, sampleTree)
			assertNoLeftovers(t, filepath.Dir(dir), dir)
		})
	}
}

func TestStripFirstDirRejectsAnArchiveWithoutOneTopLevelFolder(t *testing.T) {
	cases := map[string][]entry{
		"two folders":   {{name: "a/x.txt", content: "x"}, {name: "b/y.txt", content: "y"}},
		"file at top":   {{name: "a/x.txt", content: "x"}, {name: "README", content: "r"}},
		"only a file":   {{name: "x.txt", content: "x"}},
		"empty archive": {},
	}

	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			h := serveZip(t, zipOf(t, entries...), 0)
			dir := filepath.Join(t.TempDir(), "tool")

			err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"}, fetch.WithStripFirstDir())
			if err == nil {
				t.Fatal("expected an error")
			}

			if exists(dir) {
				t.Fatalf("%s exists after a failed fetch", dir)
			}

			assertNoLeftovers(t, filepath.Dir(dir), dir)
		})
	}
}

func TestEntriesEscapingTheDestinationAreRejected(t *testing.T) {
	cases := map[string]string{
		"parent":        "../evil.txt",
		"nested parent": "sub/../../evil.txt",
		"absolute":      "/evil.txt",
		"backslash":     `..\evil.txt`,
	}

	for name, entryName := range cases {
		t.Run(name, func(t *testing.T) {
			h := serveZip(t, zipOf(t, entry{name: "ok.txt", content: "ok"}, entry{name: entryName, content: "evil"}), 0)
			parent := t.TempDir()
			dir := filepath.Join(parent, "tool")

			err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"})
			if err == nil || !strings.Contains(err.Error(), "escapes") {
				t.Fatalf("got %v, want an error naming the escape", err)
			}

			if exists(filepath.Join(parent, "evil.txt")) {
				t.Fatalf("%q was written outside the destination", entryName)
			}

			if exists(dir) {
				t.Fatalf("%s exists after a failed fetch", dir)
			}

			assertNoLeftovers(t, parent, dir)
		})
	}
}

func TestSymlinkEscapingTheDestinationIsRejected(t *testing.T) {
	cases := map[string]string{
		"relative": "../../outside.txt",
		"absolute": "/etc/passwd",
	}

	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			h := serveZip(t, zipOf(t,
				entry{name: "sub/link", content: target, mode: fs.ModeSymlink | 0o777},
			), 0)
			dir := filepath.Join(t.TempDir(), "tool")

			err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"})
			if err == nil || !strings.Contains(err.Error(), "escapes") && !strings.Contains(err.Error(), "absolute") {
				t.Fatalf("got %v, want an error naming the escape", err)
			}

			if exists(dir) {
				t.Fatalf("%s exists after a failed fetch", dir)
			}
		})
	}
}

func TestSymlinkInsideTheArchiveIsRecreated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs a privilege on Windows")
	}

	h := serveZip(t, zipOf(t,
		entry{name: "a.txt", content: "alpha"},
		entry{name: "sub/link", content: "../a.txt", mode: fs.ModeSymlink | 0o777},
	), 0)
	dir := filepath.Join(t.TempDir(), "tool")

	if err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"}); err != nil {
		t.Fatal(err)
	}

	target, err := os.Readlink(filepath.Join(dir, "sub", "link"))
	if err != nil {
		t.Fatal(err)
	}

	if target != filepath.FromSlash("../a.txt") {
		t.Fatalf("link points to %q, want %q", target, "../a.txt")
	}

	assertTree(t, dir, map[string]string{"a.txt": "alpha", "sub/link": "alpha"})
}

func TestFileModeIsPreserved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}

	h := serveZip(t, zipOf(t, entry{name: "bin/run", content: "#!/bin/sh", mode: 0o755}), 0)
	dir := filepath.Join(t.TempDir(), "tool")

	if err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "bin", "run"))
	if err != nil {
		t.Fatal(err)
	}

	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Fatalf("mode %o, want 755", perm)
	}
}

// lines collects what a fetch logs.
type lines struct {
	mu  sync.Mutex
	out []string
}

func (l *lines) Println(v ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.out = append(l.out, fmt.Sprintln(v...))
}

func (l *lines) contains(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, line := range l.out {
		if strings.Contains(line, s) {
			return true
		}
	}

	return false
}

func TestLoggerHearsTheHostChosenTheVerdictAndTheDestination(t *testing.T) {
	archive := zipOf(t, sample...)
	h := serveZip(t, archive, 0)
	url := h.URL + "/a.zip"

	t.Run("verified", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "tool")
		log := &lines{}

		err := fetch.Zip(context.Background(), dir, []string{url}, fetch.WithLogger(log), fetch.WithSHA256(sha256Hex(archive)))
		if err != nil {
			t.Fatal(err)
		}

		for _, want := range []string{url, dir, "verified"} {
			if !log.contains(want) {
				t.Errorf("nothing logged mentions %q: %q", want, log.out)
			}
		}
	})

	t.Run("unverified", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "tool")
		log := &lines{}

		if err := fetch.Zip(context.Background(), dir, []string{url}, fetch.WithLogger(log)); err != nil {
			t.Fatal(err)
		}

		if !log.contains("unverified") {
			t.Errorf("nothing logged says the archive was unverified: %q", log.out)
		}
	})
}

func TestNoURLsIsAnError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tool")

	if err := fetch.Zip(context.Background(), dir, nil); err == nil {
		t.Fatal("expected an error")
	}

	if exists(dir) {
		t.Fatalf("%s exists after a failed fetch", dir)
	}
}

func TestContentThatIsNotAZipIsRejected(t *testing.T) {
	garbage := []byte("not an archive")
	h := serveZip(t, garbage, 0)
	dir := filepath.Join(t.TempDir(), "tool")

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"}, fetch.WithSHA256(sha256Hex(garbage)))
	if err == nil {
		t.Fatal("expected an error")
	}

	if exists(dir) {
		t.Fatalf("%s exists after a failed fetch", dir)
	}

	assertNoLeftovers(t, filepath.Dir(dir), dir)
}

func TestLoggerHearsDownloadProgress(t *testing.T) {
	archive := bigMarked(t, "slow")

	cases := map[string]struct {
		withLength bool
		want       string
	}{
		"with Content-Length":    {withLength: true, want: "%"},
		"without Content-Length": {withLength: false, want: "MB"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := serveTrickle(t, archive, 8<<10, 50*time.Millisecond, c.withLength) // ~1.3 s for the whole body
			dir := filepath.Join(t.TempDir(), "tool")
			log := &lines{}

			err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"}, fetch.WithLogger(log))
			if err != nil {
				t.Fatal(err)
			}

			if !log.contains(c.want) {
				t.Fatalf("no progress line with %q was logged: %q", c.want, log.out)
			}
		})
	}
}

// via is a transport that sends every request to one test server, whatever
// host the URL names.
type via struct {
	target string
}

func (v via) RoundTrip(req *http.Request) (*http.Response, error) {
	target, err := url.Parse(v.target)
	if err != nil {
		return nil, err
	}

	r := req.Clone(req.Context())
	r.URL.Scheme = target.Scheme
	r.URL.Host = target.Host

	return http.DefaultTransport.RoundTrip(r)
}

func TestWithClientRoutesEveryRequestThroughTheCallersClient(t *testing.T) {
	h := serveZip(t, zipOf(t, sample...), 0)
	dir := filepath.Join(t.TempDir(), "tool")
	client := &http.Client{Transport: via{target: h.URL}}

	err := fetch.Zip(context.Background(), dir, []string{"http://origin.invalid/a.zip"}, fetch.WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	assertTree(t, dir, sampleTree)
}

func TestAFileInTheDestinationsPlaceIsAnError(t *testing.T) {
	h := serveZip(t, zipOf(t, sample...), 0)
	dir := filepath.Join(t.TempDir(), "tool")

	if err := os.WriteFile(dir, []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"})
	if err == nil {
		t.Fatal("expected an error")
	}

	if hits := h.hits.Load(); hits != 0 {
		t.Fatalf("host got %d requests, want none", hits)
	}
}

func TestConflictingEntriesAreRejected(t *testing.T) {
	// "a" is a file, then "a/b.txt" wants it to be a directory.
	h := serveZip(t, zipOf(t, entry{name: "a", content: "file"}, entry{name: "a/b.txt", content: "beta"}), 0)
	dir := filepath.Join(t.TempDir(), "tool")

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"})
	if err == nil {
		t.Fatal("expected an error")
	}

	if exists(dir) {
		t.Fatalf("%s exists after a failed fetch", dir)
	}

	assertNoLeftovers(t, filepath.Dir(dir), dir)
}

func TestCorruptedArchiveIsRejected(t *testing.T) {
	archive := zipOf(t, entry{name: "a.txt", content: strings.Repeat("alpha ", 200), stored: true})
	// Flip a byte inside the stored data, past the 30-byte local header and
	// the name, so the entry's checksum no longer matches.
	archive[30+len("a.txt")+100] ^= 0xff
	h := serveZip(t, archive, 0)
	dir := filepath.Join(t.TempDir(), "tool")

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"}, fetch.WithSHA256(sha256Hex(archive)))
	if err == nil {
		t.Fatal("expected an error")
	}

	if exists(dir) {
		t.Fatalf("%s exists after a failed fetch", dir)
	}

	assertNoLeftovers(t, filepath.Dir(dir), dir)
}

func TestAnUnparsableURLIsReportedAndTheRestTried(t *testing.T) {
	good := serveZip(t, zipOf(t, sample...), 0)
	bad := "http://bad host/a.zip"
	dir := filepath.Join(t.TempDir(), "tool")

	err := fetch.Zip(context.Background(), dir, []string{bad, good.URL + "/a.zip"})
	if err != nil {
		t.Fatal(err)
	}

	assertTree(t, dir, sampleTree)

	alone := filepath.Join(t.TempDir(), "tool")

	err = fetch.Zip(context.Background(), alone, []string{bad})
	if err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("got %v, want an error naming %s", err, bad)
	}
}

func TestAHostThatStopsAnsweringAfterTheProbeFallsBack(t *testing.T) {
	flakyBody := marked(t, "flaky")

	var calls atomic.Int32

	flaky := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 { // the probe
			_, _ = w.Write(flakyBody)

			return
		}

		w.WriteHeader(http.StatusServiceUnavailable)
	})
	good := serveZip(t, marked(t, "good"), 200*time.Millisecond)
	dir := filepath.Join(t.TempDir(), "tool")

	err := fetch.Zip(context.Background(), dir, []string{flaky.URL + "/a.zip", good.URL + "/a.zip"})
	if err != nil {
		t.Fatal(err)
	}

	assertTree(t, dir, map[string]string{"origin.txt": "good"})
}

func TestGivingUpOnTheLockReturnsTheContextError(t *testing.T) {
	h := serveTrickle(t, bigMarked(t, "slow"), 8<<10, 40*time.Millisecond, true) // ~1 s for the whole body
	dir := filepath.Join(t.TempDir(), "tool")
	urls := []string{h.URL + "/a.zip"}

	first := make(chan error, 1)

	go func() { first <- fetch.Zip(context.Background(), dir, urls) }()

	time.Sleep(100 * time.Millisecond) // let the first fetcher take the lock

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := fetch.Zip(ctx, dir, urls)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}

	if err := <-first; err != nil {
		t.Fatalf("first fetcher: %v", err)
	}

	if got := tree(t, dir)["origin.txt"]; got != "slow" {
		t.Fatalf("origin.txt: got %q, want %q", got, "slow")
	}
}
