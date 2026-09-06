package fetch

import (
	"context"
	"fmt"
	"net/http"
)

// get issues a GET for url and returns the response when it is 200 OK. Any
// other status is an error, with the body closed.
func get(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		_ = res.Body.Close()

		return nil, fmt.Errorf("%s: HTTP %s", url, res.Status)
	}

	return res, nil
}
