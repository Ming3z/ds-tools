// Package deepseek is a thin, dependency-free client for the two DeepSeek web
// tools endpoints (POST /v1/tools/search and POST /v1/tools/fetch) plus the
// account balance endpoint.
package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the public DeepSeek API base URL.
	DefaultBaseURL = "https://api.deepseek.com"

	pathSearch  = "/v1/tools/search"
	pathFetch   = "/v1/tools/fetch"
	pathBalance = "/user/balance"

	maxBackoff = 30 * time.Second
)

// Client talks to the DeepSeek API.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
	Retries int
	// Sleep is injectable so tests do not have to wait.
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewClient builds a Client with a sane default HTTP client.
func NewClient(baseURL, apiKey string, timeout time.Duration, retries int) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if retries < 0 {
		retries = 0
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: timeout},
		Retries: retries,
		Sleep:   sleepCtx,
	}
}

// Search calls POST /v1/tools/search.
func (c *Client) Search(ctx context.Context, req *SearchRequest) (*SearchResponse, error) {
	var out SearchResponse
	if err := c.do(ctx, http.MethodPost, pathSearch, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Fetch calls POST /v1/tools/fetch.
func (c *Client) Fetch(ctx context.Context, req *FetchRequest) (*FetchResponse, error) {
	var out FetchResponse
	if err := c.do(ctx, http.MethodPost, pathFetch, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Balance calls GET /user/balance.
func (c *Client) Balance(ctx context.Context) (*BalanceResponse, error) {
	var out BalanceResponse
	if err := c.do(ctx, http.MethodGet, pathBalance, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Do performs a request with retry-on-retryable-error semantics. It is exported
// so that callers can reproduce the exact wire behaviour (e.g. --dry-run).
func (c *Client) Do(ctx context.Context, method, path string, body any, out any) error {
	return c.do(ctx, method, path, body, out)
}

// URL returns the absolute URL for a path.
func (c *Client) URL(path string) string { return c.BaseURL + path }

// PathSearch / PathFetch / PathBalance expose the official paths.
const (
	PathSearch  = pathSearch
	PathFetch   = pathFetch
	PathBalance = pathBalance
)

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("编码请求失败：%w", err)
		}
	}

	var lastErr error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt)
			var retryAfter time.Duration
			if ae, ok := lastErr.(*APIError); ok && ae.RetryAfter > 0 {
				retryAfter = ae.RetryAfter
			}
			if retryAfter > delay {
				delay = retryAfter
			}
			if delay > maxBackoff {
				delay = maxBackoff
			}
			if err := c.Sleep(ctx, delay); err != nil {
				return err
			}
		}

		resp, err := c.attempt(ctx, method, path, payload)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return &TransportError{Err: ctx.Err()}
			}
			continue // network errors are always retryable
		}

		if resp.status == http.StatusOK {
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(resp.body, out); err != nil {
				return fmt.Errorf("解析响应失败：%w", err)
			}
			return nil
		}

		apiErr := parseAPIError(resp.status, resp.body, resp.retryAfter)
		lastErr = apiErr
		if !apiErr.Retryable() {
			return apiErr
		}
	}
	return lastErr
}

type rawResponse struct {
	status     int
	body       []byte
	retryAfter time.Duration
}

func (c *Client) attempt(ctx context.Context, method, path string, payload []byte) (*rawResponse, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL(path), rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ds-tools")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	return &rawResponse{
		status:     resp.StatusCode,
		body:       b,
		retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}, nil
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func backoff(attempt int) time.Duration {
	d := 500 * time.Millisecond
	for i := 1; i < attempt; i++ {
		d *= 2
		if d > maxBackoff {
			return maxBackoff
		}
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
