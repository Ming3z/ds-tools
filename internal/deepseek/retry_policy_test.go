package deepseek

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// withJitter pins the backoff jitter for the duration of a test.
func withJitter(t *testing.T, factor float64) {
	t.Helper()
	old := backoffJitter
	backoffJitter = func() float64 { return factor }
	t.Cleanup(func() { backoffJitter = old })
}

// newOCRTestClient returns a client whose sleeps are recorded instead of
// performed, plus the slice the recorded delays land in.
func newOCRTestClient(baseURL string, retries int) (*Client, *[]time.Duration) {
	sleeps := new([]time.Duration)
	c := NewClient(baseURL, "sk-test", time.Second, retries)
	c.Sleep = func(_ context.Context, d time.Duration) error {
		*sleeps = append(*sleeps, d)
		return nil
	}
	return c, sleeps
}

func TestOCR429WithoutRetryAfterIsNotRetriedButTransient(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	}))
	defer srv.Close()

	c, sleeps := newOCRTestClient(srv.URL, 3)
	_, err := c.OCR(context.Background(), uploadOf([]byte("pdf"), nil), "document")

	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want 1: OCR must not re-upload without Retry-After", got)
	}
	if ae.Retryable() {
		t.Fatal("Retryable = true, want false: the loop must not retry")
	}
	if !ae.Transient() {
		t.Fatal("Transient = false, want true: the caller may retry later")
	}
	if len(*sleeps) != 0 {
		t.Fatalf("slept %v, want no sleep", *sleeps)
	}
}

func TestOCR429WithRetryAfterRetries(t *testing.T) {
	withJitter(t, 1)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"ocr-1","pages":[{"index":0,"text":"ok"}]}`)
	}))
	defer srv.Close()

	c, sleeps := newOCRTestClient(srv.URL, 3)
	resp, err := c.OCR(context.Background(), uploadOf([]byte("pdf"), nil), "document")
	if err != nil {
		t.Fatalf("OCR: %v", err)
	}
	if resp.ID != "ocr-1" {
		t.Fatalf("id = %q", resp.ID)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("hits = %d, want 2", got)
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != time.Second {
		t.Fatalf("sleeps = %v, want one 1s sleep honouring Retry-After", *sleeps)
	}
}

func TestOCR504IsTerminal(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = io.WriteString(w, `{"error":{"message":"gateway timeout","type":"server_error"}}`)
	}))
	defer srv.Close()

	c, sleeps := newOCRTestClient(srv.URL, 3)
	_, err := c.OCR(context.Background(), uploadOf([]byte("pdf"), nil), "document")

	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
	if ae.Retryable() {
		t.Fatal("Retryable = true, want false for OCR 504")
	}
	if ae.Transient() {
		t.Fatal("Transient = true, want false for OCR 504")
	}
	if len(*sleeps) != 0 {
		t.Fatalf("slept %v, want no sleep", *sleeps)
	}
}

func TestOCR200BodyPolicy(t *testing.T) {
	t.Run("transient message retries", func(t *testing.T) {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if atomic.AddInt32(&hits, 1) == 1 {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"error":{"message":"Service temporarily unavailable"}}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"ocr-2","pages":[]}`)
		}))
		defer srv.Close()

		c, _ := newOCRTestClient(srv.URL, 3)
		if _, err := c.OCR(context.Background(), uploadOf([]byte("pdf"), nil), "document"); err != nil {
			t.Fatalf("OCR: %v", err)
		}
		if got := atomic.LoadInt32(&hits); got != 2 {
			t.Fatalf("hits = %d, want 2", got)
		}
	})

	t.Run("gateway timeout is terminal", func(t *testing.T) {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"error":{"message":"gateway timeout"}}`)
		}))
		defer srv.Close()

		c, _ := newOCRTestClient(srv.URL, 3)
		_, err := c.OCR(context.Background(), uploadOf([]byte("pdf"), nil), "document")

		var ae *APIError
		if !errors.As(err, &ae) {
			t.Fatalf("error = %v, want *APIError", err)
		}
		if got := atomic.LoadInt32(&hits); got != 1 {
			t.Fatalf("hits = %d, want 1", got)
		}
		if ae.Retryable() || ae.Transient() {
			t.Fatalf("Retryable=%v Transient=%v, want both false", ae.Retryable(), ae.Transient())
		}
	})
}

func TestAPIErrorTransientSemantics(t *testing.T) {
	cases := []struct {
		name string
		err  *APIError
		want bool
	}{
		{"408", &APIError{Status: http.StatusRequestTimeout}, true},
		{"429", &APIError{Status: http.StatusTooManyRequests}, true},
		{"500", &APIError{Status: http.StatusInternalServerError}, true},
		{"503", &APIError{Status: http.StatusServiceUnavailable}, true},
		{"504 non-OCR", &APIError{Status: http.StatusGatewayTimeout}, true},
		{"400", &APIError{Status: http.StatusBadRequest}, false},
		{"402", &APIError{Status: http.StatusPaymentRequired}, false},
		{"403", &APIError{Status: http.StatusForbidden}, false},
		{"404", &APIError{Status: http.StatusNotFound}, false},
		{"413", &APIError{Status: http.StatusRequestEntityTooLarge}, false},
		{"401", &APIError{Status: http.StatusUnauthorized}, false},
		{"OCR 429 retry override", &APIError{Status: http.StatusTooManyRequests, ocr: true, retry: new(false)}, true},
		{"OCR 504", &APIError{Status: http.StatusGatewayTimeout, ocr: true, retry: new(false)}, false},
		{"OCR 200 transient", &APIError{Status: http.StatusOK, ocr: true, Message: "Internal Server Error"}, true},
		{"OCR 200 gateway timeout", &APIError{Status: http.StatusOK, ocr: true, Message: "gateway timeout"}, false},
		{"non-OCR 200 body", &APIError{Status: http.StatusOK, Message: "Service temporarily unavailable"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.err.Transient(); got != c.want {
				t.Fatalf("Transient = %v, want %v", got, c.want)
			}
		})
	}
}

func TestBackoffJitterBounds(t *testing.T) {
	t.Run("low", func(t *testing.T) {
		withJitter(t, 0.8)
		if got := backoff(1); got != 400*time.Millisecond {
			t.Fatalf("backoff(1) = %v, want 400ms", got)
		}
	})
	t.Run("high", func(t *testing.T) {
		withJitter(t, 1.2)
		if got := backoff(1); got != 600*time.Millisecond {
			t.Fatalf("backoff(1) = %v, want 600ms", got)
		}
	})
	t.Run("never exceeds maxBackoff", func(t *testing.T) {
		withJitter(t, 1.2)
		if got := backoff(12); got != maxBackoff {
			t.Fatalf("backoff(12) = %v, want %v", got, maxBackoff)
		}
	})
	t.Run("exponential up to the cap", func(t *testing.T) {
		withJitter(t, 1)
		for attempt, want := range map[int]time.Duration{
			1: 500 * time.Millisecond,
			2: time.Second,
			3: 2 * time.Second,
		} {
			if got := backoff(attempt); got != want {
				t.Fatalf("backoff(%d) = %v, want %v", attempt, got, want)
			}
		}
	})
}
