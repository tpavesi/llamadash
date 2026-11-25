package llama

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxBody = 8 << 20 // responses larger than this are rejected

// Client performs read-only GET requests against a llama-server.
type Client struct {
	base   string
	apiKey string
	http   *http.Client
}

// New creates a client. timeout bounds each request.
func New(baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{
		base:   strings.TrimRight(baseURL, "/"),
		apiKey: apiKey,
		http:   &http.Client{Timeout: timeout},
	}
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotImplemented:
		return nil, ErrDisabled
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrUnauthorized
	case http.StatusServiceUnavailable:
		return nil, ErrLoading
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("GET %s: HTTP %d: %w", path, resp.StatusCode, ErrHTTPStatus)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBody {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", path, maxBody)
	}
	return b, nil
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	b, err := c.get(ctx, "/health")
	if err != nil {
		return Health{}, err
	}
	return ParseHealth(b)
}

func (c *Client) Props(ctx context.Context) (Props, error) {
	b, err := c.get(ctx, "/props")
	if err != nil {
		return Props{}, err
	}
	return ParseProps(b)
}

func (c *Client) Slots(ctx context.Context) ([]Slot, error) {
	b, err := c.get(ctx, "/slots")
	if err != nil {
		return nil, err
	}
	return ParseSlots(b)
}

func (c *Client) Metrics(ctx context.Context) (Metrics, error) {
	b, err := c.get(ctx, "/metrics")
	if err != nil {
		return nil, err
	}
	return ParseMetrics(b)
}
