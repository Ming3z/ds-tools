package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// dropResponse reads the multipart upload and then closes the connection
// without sending a response: the server owns (and bills) the recognition.
func dropResponse(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Errorf("response writer does not support hijacking")
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	_ = conn.Close()
}

// TestOCRUploadResponseLost pins the upload-then-disconnect behaviour: the file
// must not be re-sent (billing), the exit code is 1, and the structured error
// reports a non-retryable response_lost.
func TestOCRUploadResponseLost(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/ocr" {
			t.Errorf("path = %s", r.URL.Path)
		}
		readCLIUpload(t, r)
		dropResponse(t, w)
	}
	path := writeTemp(t, "doc.pdf", []byte("pdf"))

	args := []string{"--base-url", h.srv.URL, "--retries", "3", "ocr", path, "--error-format", "json"}
	if code := Execute(args, strings.NewReader(""), &h.out, &h.err); code != ExitServer {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, ExitServer, h.err.String())
	}
	if got := atomic.LoadInt32(&h.hits); got != 1 {
		t.Fatalf("hits = %d, want 1 (a completed upload must never be retried)", got)
	}

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(h.err.Bytes()), &payload); err != nil {
		t.Fatalf("stderr not JSON: %v (%s)", err, h.err.String())
	}
	if payload["type"] != "network_error" || payload["code"] != "response_lost" {
		t.Fatalf("payload = %v", payload)
	}
	if payload["retryable"] != false {
		t.Fatalf("retryable = %v, want false", payload["retryable"])
	}

	// The human-readable form explains the billing risk.
	h.out.Reset()
	h.err.Reset()
	args = []string{"--base-url", h.srv.URL, "--retries", "3", "ocr", path}
	if code := Execute(args, strings.NewReader(""), &h.out, &h.err); code != ExitServer {
		t.Fatalf("text exit = %d, want %d (stderr=%s)", code, ExitServer, h.err.String())
	}
	if got := h.err.String(); !strings.Contains(got, "文件已上传") || !strings.Contains(got, "计费") {
		t.Fatalf("stderr does not explain the billing risk: %s", got)
	}
	if got := atomic.LoadInt32(&h.hits); got != 2 {
		t.Fatalf("hits = %d, want 2 (one request per run)", got)
	}
}
