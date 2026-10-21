package loaders

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrNotPublished is returned for a 404: the file is not out yet, or the day
// had no session.
var ErrNotPublished = errors.New("not published")

// Fetcher downloads exchange files. It sends the configured User-Agent, which
// names this program honestly rather than posing as a browser.
type Fetcher struct {
	UserAgent string
	Client    *http.Client
}

const maxFile = 256 << 20

func (f Fetcher) Get(ctx context.Context, url string) ([]byte, error) {
	c := f.Client
	if c == nil {
		c = &http.Client{Timeout: 2 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if f.UserAgent != "" {
		req.Header.Set("User-Agent", f.UserAgent)
	}
	req.Header.Set("Accept", "*/*")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s: %w", url, ErrNotPublished)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxFile))
}
