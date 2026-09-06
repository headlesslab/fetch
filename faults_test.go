package fetch_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/headlesslab/fetch"
)

// The tests here reach the error branches a healthy machine never takes:
// they make the operating system refuse a step, or hand Zip an archive
// whose entries cannot be written.

func TestUnwritableWorkDirectoryFailsTheDownload(t *testing.T) {
	requireFaultInjection(t)

	h := serveZip(t, zipOf(t, sample...), 0)
	dir := filepath.Join(t.TempDir(), "tool")
	deny := &hook{trigger: "fetch: downloading", fn: func() { readOnly(t, workDir(t, dir), 0o500) }}

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"}, fetch.WithLogger(deny))
	if err == nil {
		t.Fatal("expected an error")
	}

	if exists(dir) {
		t.Fatalf("%s exists after a failed fetch", dir)
	}
}

func TestUnwritableWorkDirectoryFailsTheExtraction(t *testing.T) {
	requireFaultInjection(t)

	h := serveZip(t, zipOf(t, sample...), 0)
	dir := filepath.Join(t.TempDir(), "tool")
	deny := &hook{trigger: "extracting unverified", fn: func() { readOnly(t, workDir(t, dir), 0o500) }}

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"}, fetch.WithLogger(deny))
	if err == nil {
		t.Fatal("expected an error")
	}

	if exists(dir) {
		t.Fatalf("%s exists after a failed fetch", dir)
	}
}

func TestUnwritableParentFailsBeforeAnyRequest(t *testing.T) {
	requireFaultInjection(t)

	h := serveZip(t, zipOf(t, sample...), 0)
	parent := t.TempDir()
	dir := filepath.Join(parent, "tool")

	// The lock file already exists, so the lock is taken; creating the work
	// directory is the first step to need a writable parent.
	if err := os.WriteFile(dir+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}

	readOnly(t, parent, 0o500)

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"})
	if err == nil {
		t.Fatal("expected an error")
	}

	if hits := h.hits.Load(); hits != 0 {
		t.Fatalf("host got %d requests, want none", hits)
	}
}

func TestUnreadableParentIsAnError(t *testing.T) {
	requireFaultInjection(t)

	h := serveZip(t, zipOf(t, sample...), 0)
	parent := t.TempDir()
	dir := filepath.Join(parent, "tool")

	readOnly(t, parent, 0)

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"})
	if err == nil {
		t.Fatal("expected an error")
	}

	if hits := h.hits.Load(); hits != 0 {
		t.Fatalf("host got %d requests, want none", hits)
	}
}

func TestDeletedWorkingDirectoryIsAnError(t *testing.T) {
	// Windows refuses to remove the working directory, and macOS still
	// reports a path for a removed one; only Linux fails Getwd.
	if runtime.GOOS != "linux" {
		t.Skip("only Linux reports a removed working directory")
	}

	h := serveZip(t, zipOf(t, sample...), 0)
	gone := filepath.Join(t.TempDir(), "gone")

	if err := os.Mkdir(gone, 0o700); err != nil {
		t.Fatal(err)
	}

	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chdir(gone); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chdir(previous) })

	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	// A relative destination cannot be made absolute without a working
	// directory.
	err = fetch.Zip(context.Background(), "tool", []string{h.URL + "/a.zip"})
	if err == nil {
		t.Fatal("expected an error")
	}

	if hits := h.hits.Load(); hits != 0 {
		t.Fatalf("host got %d requests, want none", hits)
	}
}

func TestFileOnTheWayToTheDestinationIsAnError(t *testing.T) {
	h := serveZip(t, zipOf(t, sample...), 0)
	file := filepath.Join(t.TempDir(), "file")

	if err := os.WriteFile(file, []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := fetch.Zip(context.Background(), filepath.Join(file, "tool"), []string{h.URL + "/a.zip"})
	if err == nil {
		t.Fatal("expected an error")
	}

	if hits := h.hits.Load(); hits != 0 {
		t.Fatalf("host got %d requests, want none", hits)
	}
}

func TestDestinationCreatedMeanwhileIsNotClobbered(t *testing.T) {
	h := serveZip(t, zipOf(t, sample...), 0)
	dir := filepath.Join(t.TempDir(), "tool")
	mine := filepath.Join(dir, "mine.txt")

	// Someone lands dir between the check under the lock and the rename.
	intrude := &hook{trigger: "extracting unverified", fn: func() {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(mine, []byte("kept"), 0o644); err != nil {
			t.Fatal(err)
		}
	}}

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"}, fetch.WithLogger(intrude))
	if err == nil {
		t.Fatal("expected an error")
	}

	assertTree(t, dir, map[string]string{"mine.txt": "kept"})
}

func TestCancelDuringExtractionAborts(t *testing.T) {
	h := serveZip(t, zipOf(t, sample...), 0)
	dir := filepath.Join(t.TempDir(), "tool")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	abort := &hook{trigger: "extracting unverified", fn: cancel}

	err := fetch.Zip(ctx, dir, []string{h.URL + "/a.zip"}, fetch.WithLogger(abort))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}

	if exists(dir) {
		t.Fatalf("%s exists after a cancelled fetch", dir)
	}

	assertNoLeftovers(t, filepath.Dir(dir), dir)
}

func TestUnsupportedCompressionIsRejected(t *testing.T) {
	const bzip2 = 12 // a method that archive/zip cannot read

	cases := map[string]entry{
		"file":          {name: "a.txt", content: "alpha", method: bzip2},
		"symbolic link": {name: "link", content: "a.txt", mode: fs.ModeSymlink | 0o777, method: bzip2},
	}

	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			h := serveZip(t, zipOf(t, e), 0)
			dir := filepath.Join(t.TempDir(), "tool")

			err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"})
			if err == nil {
				t.Fatal("expected an error")
			}

			if exists(dir) {
				t.Fatalf("%s exists after a failed fetch", dir)
			}
		})
	}
}

func TestEntriesThatCannotBeWrittenAreRejected(t *testing.T) {
	cases := map[string][]entry{
		"file where a directory stands": {
			{name: "a/", content: ""},
			{name: "a", content: "file"},
		},
		"symbolic link under a file": {
			{name: "a", content: "file"},
			{name: "a/link", content: "x", mode: fs.ModeSymlink | 0o777},
		},
	}

	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			h := serveZip(t, zipOf(t, entries...), 0)
			dir := filepath.Join(t.TempDir(), "tool")

			err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"})
			if err == nil {
				t.Fatal("expected an error")
			}

			if exists(dir) {
				t.Fatalf("%s exists after a failed fetch", dir)
			}
		})
	}
}

func TestCorruptedSymlinkEntryIsRejected(t *testing.T) {
	target := strings.Repeat("target/", 10) + "file"
	archive := zipOf(t, entry{name: "link", content: target, mode: fs.ModeSymlink | 0o777, stored: true})
	// Flip a byte of the stored link target, past the 30-byte local header
	// and the name, so the checksum no longer matches.
	archive[30+len("link")+10] ^= 0xff
	h := serveZip(t, archive, 0)
	dir := filepath.Join(t.TempDir(), "tool")

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"}, fetch.WithSHA256(sha256Hex(archive)))
	if err == nil {
		t.Fatal("expected an error")
	}

	if exists(dir) {
		t.Fatalf("%s exists after a failed fetch", dir)
	}
}

func TestSymlinkOverAnExistingFileIsRejected(t *testing.T) {
	requireSymlinks(t)

	h := serveZip(t, zipOf(t,
		entry{name: "a/x", content: "file"},
		entry{name: "a/x", content: "elsewhere", mode: fs.ModeSymlink | 0o777},
	), 0)
	dir := filepath.Join(t.TempDir(), "tool")

	err := fetch.Zip(context.Background(), dir, []string{h.URL + "/a.zip"})
	if err == nil {
		t.Fatal("expected an error")
	}

	if exists(dir) {
		t.Fatalf("%s exists after a failed fetch", dir)
	}
}
