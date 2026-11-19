package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"dstools/internal/deepseek"
)

func decodeErrorPayload(t *testing.T, h *harness) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(h.err.Bytes()), &payload); err != nil {
		t.Fatalf("stderr not JSON: %v (%s)", err, h.err.String())
	}
	return payload
}

func TestCodeOfAPIErrorStatuses(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{
		{http.StatusUnauthorized, ExitAuth},
		{http.StatusBadRequest, ExitServer},
		{http.StatusPaymentRequired, ExitServer},
		{http.StatusForbidden, ExitServer},
		{http.StatusNotFound, ExitServer},
		{http.StatusRequestEntityTooLarge, ExitServer},
		{http.StatusRequestTimeout, ExitNetwork},
		{http.StatusTooManyRequests, ExitNetwork},
		{http.StatusInternalServerError, ExitNetwork},
		{http.StatusServiceUnavailable, ExitNetwork},
		{http.StatusGatewayTimeout, ExitNetwork},
	}
	for _, c := range cases {
		err := &deepseek.APIError{Status: c.status, Message: "boom"}
		if got := codeOf(err); got != c.want {
			t.Errorf("status %d -> exit %d, want %d", c.status, got, c.want)
		}
	}
}

func TestCodeOfTransportError(t *testing.T) {
	sent := &deepseek.TransportError{Err: errors.New("connection reset"), Sent: true}
	if got := codeOf(sent); got != ExitServer {
		t.Fatalf("sent transport error -> exit %d, want %d", got, ExitServer)
	}
	unsent := &deepseek.TransportError{Err: errors.New("connection refused")}
	if got := codeOf(unsent); got != ExitNetwork {
		t.Fatalf("unsent transport error -> exit %d, want %d", got, ExitNetwork)
	}
}

func TestOCRRateLimitWithoutRetryAfterIsTransient(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/ocr" {
			t.Errorf("path = %s", r.URL.Path)
		}
		readCLIUpload(t, r)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	}
	path := writeTemp(t, "doc.pdf", []byte("pdf"))

	args := []string{"--base-url", h.srv.URL, "--retries", "3", "ocr", path, "--error-format", "json"}
	if code := Execute(args, strings.NewReader(""), &h.out, &h.err); code != ExitNetwork {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, ExitNetwork, h.err.String())
	}
	if got := atomic.LoadInt32(&h.hits); got != 1 {
		t.Fatalf("hits = %d, want 1: OCR must not re-upload without Retry-After", got)
	}
	payload := decodeErrorPayload(t, h)
	if payload["retryable"] != true {
		t.Fatalf("retryable = %v, want true: the caller may retry later", payload["retryable"])
	}
	if payload["status"] != float64(http.StatusTooManyRequests) {
		t.Fatalf("status = %v, want 429", payload["status"])
	}
}

func TestOCRGatewayTimeoutIsServerError(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		readCLIUpload(t, r)
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte(`{"error":{"message":"gateway timeout","type":"server_error"}}`))
	}
	path := writeTemp(t, "doc.pdf", []byte("pdf"))

	args := []string{"--base-url", h.srv.URL, "--retries", "3", "ocr", path, "--error-format", "json"}
	if code := Execute(args, strings.NewReader(""), &h.out, &h.err); code != ExitServer {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, ExitServer, h.err.String())
	}
	if got := atomic.LoadInt32(&h.hits); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
	if payload := decodeErrorPayload(t, h); payload["retryable"] != false {
		t.Fatalf("retryable = %v, want false", payload["retryable"])
	}
}

func TestSearchExhausted503IsTransient(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded","type":"server_error"}}`))
	}

	args := []string{"--base-url", h.srv.URL, "--retries", "1", "search", "x", "--error-format", "json"}
	if code := Execute(args, strings.NewReader(""), &h.out, &h.err); code != ExitNetwork {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, ExitNetwork, h.err.String())
	}
	if got := atomic.LoadInt32(&h.hits); got != 2 {
		t.Fatalf("hits = %d, want 2 (one retry)", got)
	}
	payload := decodeErrorPayload(t, h)
	if payload["retryable"] != true {
		t.Fatalf("retryable = %v, want true", payload["retryable"])
	}
	if payload["status"] != float64(http.StatusServiceUnavailable) {
		t.Fatalf("status = %v, want 503", payload["status"])
	}
}

func TestNonTransientStatusesExitServer(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   int
	}{
		{http.StatusUnauthorized, `{"error":{"message":"invalid key","type":"authentication_error"}}`, ExitAuth},
		{http.StatusBadRequest, `{"error":{"message":"bad request","type":"invalid_request_error"}}`, ExitServer},
		{http.StatusPaymentRequired, `{"error":{"message":"no balance","type":"insufficient_quota"}}`, ExitServer},
		{http.StatusForbidden, `{"error":{"message":"forbidden","type":"permission_error"}}`, ExitServer},
		{http.StatusNotFound, `{"error":{"message":"not found","type":"not_found_error"}}`, ExitServer},
	}
	for _, c := range cases {
		h := newHarness(t)
		h.handle = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}
		if code := h.run("search", "x", "--error-format", "json"); code != c.want {
			t.Errorf("status %d -> exit %d, want %d (stderr=%s)", c.status, code, c.want, h.err.String())
		}
		if got := atomic.LoadInt32(&h.hits); got != 1 {
			t.Errorf("status %d -> hits %d, want 1", c.status, got)
		}
		if payload := decodeErrorPayload(t, h); payload["retryable"] != false {
			t.Errorf("status %d -> retryable %v, want false", c.status, payload["retryable"])
		}
	}
}

func TestUsageErrorIsNotRetryable(t *testing.T) {
	h := newHarness(t)
	if code := h.run("search", "x", "-n", "0", "--error-format", "json"); code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if got := atomic.LoadInt32(&h.hits); got != 0 {
		t.Fatalf("hits = %d, want 0: usage errors must not reach the network", got)
	}
	if payload := decodeErrorPayload(t, h); payload["retryable"] != false {
		t.Fatalf("retryable = %v, want false", payload["retryable"])
	}
}
