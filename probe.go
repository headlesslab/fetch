package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// probeSize is how much of the archive a host must deliver to count as a
// responder.
const probeSize = 64 << 10

// responder is a URL that answered a probe, or the reason it did not.
type responder struct {
	url string
	err error
}

// probe requests every URL at once and reports each on the returned channel
// as soon as it has delivered the first bytes of the archive, so the channel
// yields hosts in the order they answered; a URL that fails is reported with
// its error. The channel closes once every probe has finished.
func probe(ctx context.Context, client *http.Client, urls []string) <-chan responder {
	out := make(chan responder, len(urls))

	var wg sync.WaitGroup

	for _, u := range urls {
		u := u

		wg.Add(1)

		go func() {
			defer wg.Done()

			out <- responder{url: u, err: probeOne(ctx, client, u)}
		}()
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}

func probeOne(ctx context.Context, client *http.Client, url string) error {
	res, err := get(ctx, client, url)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()

	_, err = io.CopyN(io.Discard, res.Body, probeSize)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w", url, err)
	}

	return nil
}
