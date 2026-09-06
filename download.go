package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// progressInterval is the least time between two progress lines.
const progressInterval = time.Second

// download streams the body of url into the file at path and returns the
// hex SHA-256 of what it wrote.
func download(ctx context.Context, cfg *config, url, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	res, err := cfg.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %s", url, res.Status)
	}

	f, err := os.Create(path)
	if err != nil {
		return "", err
	}

	hash := sha256.New()
	meter := &progress{logger: cfg.logger, total: res.ContentLength, last: time.Now()}

	_, err = io.Copy(io.MultiWriter(f, hash, meter), res.Body)
	if err != nil {
		_ = f.Close()

		return "", fmt.Errorf("%s: %w", url, err)
	}

	err = f.Close()
	if err != nil {
		return "", err
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
