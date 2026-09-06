package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// progressInterval is the least time between two progress lines.
const progressInterval = time.Second

// download streams the body of url into the file at path and returns the
// hex SHA-256 of what it wrote.
func (f *fetcher) download(ctx context.Context, url, path string) (string, error) {
	res, err := get(ctx, f.client, url)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()

	file, err := os.Create(path)
	if err != nil {
		return "", err
	}

	hash := sha256.New()
	meter := &progress{logger: f.logger, total: res.ContentLength, last: time.Now()}

	// A write error can surface at close, so both are one verdict.
	_, err = io.Copy(io.MultiWriter(file, hash, meter), res.Body)

	err = errors.Join(err, file.Close())
	if err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// progress counts the bytes written through it and logs how far the
// download is, at most once per progressInterval.
type progress struct {
	logger Logger
	total  int64 // -1 when the server sent no Content-Length
	done   int64
	last   time.Time
}

func (p *progress) Write(b []byte) (int, error) {
	p.done += int64(len(b))

	if now := time.Now(); now.Sub(p.last) >= progressInterval {
		p.last = now
		p.report()
	}

	return len(b), nil
}

func (p *progress) report() {
	const mb = 1 << 20

	if p.total > 0 {
		p.logger.Println(fmt.Sprintf("fetch: downloaded %d%% of %.1f MB", p.done*100/p.total, float64(p.total)/mb))

		return
	}

	p.logger.Println(fmt.Sprintf("fetch: downloaded %.1f MB", float64(p.done)/mb))
}
