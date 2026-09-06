// Package fetch downloads one zip archive from the first of several hosts to
// respond, verifies its SHA-256 before extraction, and lands the extracted
// tree in the destination directory with one atomic rename.
//
// The package knows nothing about what the archive holds: URL templates,
// platform names and version pins belong to the caller.
package fetch

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// Logger receives one line per event: the host chosen, download progress,
// verification and extraction. A *log.Logger satisfies it.
type Logger interface {
	Println(v ...any)
}

// HashMismatchError reports an archive whose SHA-256 differs from the one
// given with WithSHA256. Nothing was extracted from it.
type HashMismatchError struct {
	URL      string
	Expected string
	Actual   string
}

func (e *HashMismatchError) Error() string {
	return fmt.Sprintf("sha256 mismatch for %s: expected %s, got %s", e.URL, e.Expected, e.Actual)
}

// sha256Len is the length of a SHA-256 digest in bytes.
const sha256Len = 32

// lockRetry is how often a fetcher waiting for another's lock checks again.
const lockRetry = 50 * time.Millisecond

type quiet struct{}

func (quiet) Println(...any) {}

type config struct {
	sha256 string
	strip  bool
	client *http.Client
	logger Logger
}

// Option configures Zip.
type Option func(*config)

// WithSHA256 sets the expected hex digest of the archive. The download is
// hashed as it streams and compared before the archive is opened; a mismatch
// fails with a *HashMismatchError. An empty digest means no verification,
// which the logger reports.
func WithSHA256(hexDigest string) Option {
	return func(c *config) { c.sha256 = hexDigest }
}

// WithStripFirstDir expects the archive to hold exactly one top-level
// directory and makes that directory's content the destination, the way
// browser archives nest one folder.
func WithStripFirstDir() Option {
	return func(c *config) { c.strip = true }
}

// WithClient sets the HTTP client. The default is http.DefaultClient.
func WithClient(client *http.Client) Option {
	return func(c *config) {
		if client != nil {
			c.client = client
		}
	}
}

// WithLogger sets where event lines go. The default discards them.
func WithLogger(logger Logger) Option {
	return func(c *config) {
		if logger != nil {
			c.logger = logger
		}
	}
}

// Zip fetches the archive served at any of urls and extracts it into dir.
//
// Every URL is probed concurrently and the first host to answer with the
// start of the archive is downloaded from; the others are fallbacks, in the
// order they answered, for a download that fails or does not match the
// expected digest. Extraction goes into a temporary sibling of dir that is
// renamed into place at the end, so dir either does not exist or is
// complete. A file lock next to dir serializes concurrent fetchers of the
// same directory; when dir already exists, Zip returns without a request.
func Zip(ctx context.Context, dir string, urls []string, opts ...Option) error {
	cfg := config{client: http.DefaultClient, logger: quiet{}}
	for _, opt := range opts {
		opt(&cfg)
	}

	if len(urls) == 0 {
		return errors.New("fetch: no URLs to fetch from")
	}

	err := cfg.checkDigest()
	if err != nil {
		return err
	}

	dir = filepath.Clean(dir)

	err = os.MkdirAll(filepath.Dir(dir), 0o755)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	done, err := present(dir)
	if done || err != nil {
		return err
	}

	// The lock file sits next to dir rather than inside it, since dir is
	// renamed into place as a whole. It is never removed: deleting it would
	// race a fetcher that has just opened it to wait.
	lock := flock.New(dir + ".lock")

	_, err = lock.TryLockContext(ctx, lockRetry)
	if err != nil {
		return fmt.Errorf("fetch: waiting for the lock on %s: %w", dir, err)
	}
	defer func() { _ = lock.Unlock() }()

	// Whoever held the lock before may have landed dir meanwhile.
	done, err = present(dir)
	if done || err != nil {
		if done {
			cfg.logger.Println("fetch: found", dir, "in place")
		}

		return err
	}

	return fetchLocked(ctx, &cfg, dir, urls)
}

// fetchLocked does the work of Zip once the lock is held and dir is known to
// be absent.
func fetchLocked(ctx context.Context, cfg *config, dir string, urls []string) error {
	parent, base := filepath.Split(dir)

	work, err := os.MkdirTemp(parent, "."+base+".fetch-")
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	// Probes outlive the winner's download only as far as the fallbacks need
	// them; cancelling on return stops the stragglers.
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var errs []error

	for r := range probe(probeCtx, cfg.client, urls) {
		if r.err != nil {
			cfg.logger.Println("fetch: no answer from", r.err)
			errs = append(errs, r.err)

			continue
		}

		err := fetchFrom(ctx, cfg, r.url, work, dir)
		if err == nil {
			return nil
		}

		cfg.logger.Println("fetch: giving up on", err)
		errs = append(errs, err)
	}

	err = ctx.Err()
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	return fmt.Errorf("fetch: no URL yielded the archive: %w", errors.Join(errs...))
}

// fetchFrom downloads url into the work directory, extracts it and renames
// the extracted tree to dir.
func fetchFrom(ctx context.Context, cfg *config, url, work, dir string) error {
	cfg.logger.Println("fetch: downloading", url)

	archive := filepath.Join(work, "archive.zip")

	actual, err := download(ctx, cfg, url, archive)
	if err != nil {
		return err
	}

	switch {
	case cfg.sha256 == "":
		cfg.logger.Println("fetch: no expected SHA-256, extracting unverified")
	case actual != cfg.sha256:
		return &HashMismatchError{URL: url, Expected: cfg.sha256, Actual: actual}
	default:
		cfg.logger.Println("fetch: verified sha256", actual)
	}

	tree := filepath.Join(work, "tree")

	err = os.RemoveAll(tree)
	if err != nil {
		return err
	}

	err = unzip(ctx, archive, tree)
	if err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}

	if cfg.strip {
		tree, err = onlyDir(tree)
		if err != nil {
			return fmt.Errorf("%s: %w", url, err)
		}
	}

	err = os.Rename(tree, dir)
	if err != nil {
		return err
	}

	cfg.logger.Println("fetch: extracted to", dir)

	return nil
}

// checkDigest normalizes the expected digest and rejects one that is not a
// SHA-256 in hex before any request is made.
func (c *config) checkDigest() error {
	if c.sha256 == "" {
		return nil
	}

	c.sha256 = strings.ToLower(strings.TrimSpace(c.sha256))

	if b, err := hex.DecodeString(c.sha256); err != nil || len(b) != sha256Len {
		return fmt.Errorf("fetch: expected SHA-256 %q is not %d hex digits", c.sha256, 2*sha256Len)
	}

	return nil
}

// present reports whether dir already exists; a non-directory in its place is
// an error.
func present(dir string) (bool, error) {
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("fetch: %w", err)
	}

	if !info.IsDir() {
		return false, fmt.Errorf("fetch: %s exists and is not a directory", dir)
	}

	return true, nil
}

// onlyDir returns the single directory that tree holds, for WithStripFirstDir.
func onlyDir(tree string) (string, error) {
	entries, err := os.ReadDir(tree)
	if err != nil {
		return "", err
	}

	if len(entries) != 1 || !entries[0].IsDir() {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}

		return "", fmt.Errorf("expected one top-level directory to strip, found %q", names)
	}

	return filepath.Join(tree, entries[0].Name()), nil
}
