package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func uploadOf(data []byte, opens *int32) Upload {
	return Upload{
		Name: "sample.bin",
		Size: int64(len(data)),
		Open: func() (io.ReadCloser, error) {
			if opens != nil {
				atomic.AddInt32(opens, 1)
			}
			return io.NopCloser(bytes.NewReader(data)), nil
		},
	}
}

func readUpload(t *testing.T, r *http.Request) (purpose, name string, body []byte) {
	t.Helper()
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	purpose = r.FormValue("purpose")
	f, hdr, err := r.FormFile("file")
	if err != nil {
		t.Fatalf("FormFile: %v", err)
	}
	defer func() { _ = f.Close() }()
	body, err = io.ReadAll(f)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	return purpose, hdr.Filename, body
}

func TestOCRMultipartAndDecode(t *testing.T) {
	const payload = "fake-pdf-bytes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/ocr" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("authorization = %s", got)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data;") {
			t.Errorf("content-type = %s", ct)
		}
		if r.ContentLength <= 0 {
			t.Errorf("expected a known Content-Length, got %d", r.ContentLength)
		}
		purpose, name, body := readUpload(t, r)
		if purpose != "text_recognition" {
			t.Errorf("purpose = %q", purpose)
		}
		if name != "sample.bin" || string(body) != payload {
			t.Errorf("file = %q (%q)", name, body)
		}
		fmt.Fprint(w, `{"id":"ocr-1","object":"ocr.result","created":1,"purpose":"text_recognition","page_count":2,"pages":[{"index":0,"text":"one","finish_reason":"stop"},{"index":1,"text":"two","finish_reason":"length"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL, 0).OCR(context.Background(), uploadOf([]byte(payload), nil), "text_recognition")
	if err != nil {
		t.Fatal(err)
	}
	if resp.PageCount != 2 || len(resp.Pages) != 2 || resp.Pages[1].FinishReason != "length" {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Usage.TotalTokens != 3 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestOCRReplaysUploadOnRetry(t *testing.T) {
	var hits, opens int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"message":"busy","type":"server_error","code":"server_error"}}`)
			return
		}
		_, _, body := readUpload(t, r)
		if string(body) != "payload" {
			t.Errorf("retry body = %q", body)
		}
		fmt.Fprint(w, `{"id":"ocr","page_count":0,"pages":[]}`)
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv.URL, 1).OCR(context.Background(), uploadOf([]byte("payload"), &opens), "rich_layout_recognition"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("hits = %d, want 2", got)
	}
	if got := atomic.LoadInt32(&opens); got < 2 {
		t.Fatalf("upload was not re-opened for the retry: opens=%d", got)
	}
}

func TestOCRInBodyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":{"message":"OCR did not finish in time","type":"gateway_timeout_error","param":null,"code":"gateway_timeout_error"}}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL, 0).OCR(context.Background(), uploadOf([]byte("x"), nil), "text_recognition")
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v (%T)", err, err)
	}
	if ae.Status != http.StatusOK || !strings.Contains(ae.Message, "did not finish") {
		t.Fatalf("api error = %+v", ae)
	}
}

func TestASRMultipart(t *testing.T) {
	const payload = "wav-bytes"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/asr" {
			t.Errorf("path = %s", r.URL.Path)
		}
		purpose, name, body := readUpload(t, r)
		if purpose != "" {
			t.Errorf("asr should not send purpose, got %q", purpose)
		}
		if name != "sample.bin" || string(body) != payload {
			t.Errorf("file = %q (%q)", name, body)
		}
		fmt.Fprint(w, `{"request_id":"r1","text":"你好","duration_ms":3200}`)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL, 0).ASR(context.Background(), uploadOf([]byte(payload), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "你好" || resp.DurationMS != 3200 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestTTSStreamsBody(t *testing.T) {
	audio := []byte("RIFF....WAVEfake")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/tts" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var req TTSRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if req.Text != "你好" || req.VoiceID != "tide" || req.Format != "pcm" {
			t.Errorf("req = %+v", req)
		}
		if req.FilterMarkdown == nil || !*req.FilterMarkdown {
			t.Errorf("filter_markdown = %v", req.FilterMarkdown)
		}
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write(audio)
	}))
	defer srv.Close()

	body, err := newTestClient(t, srv.URL, 0).TTS(context.Background(), &TTSRequest{
		Text: "你好", VoiceID: "tide", Format: "pcm", FilterMarkdown: new(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, audio) {
		t.Fatalf("audio = %q", got)
	}
}

func TestTTSRetriesBeforeBody(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"message":"busy","type":"server_error","code":"server_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("RIFF....WAVE"))
	}))
	defer srv.Close()

	body, err := newTestClient(t, srv.URL, 1).TTS(context.Background(), &TTSRequest{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("hits = %d, want 2", got)
	}
}

func TestTTSJSONErrorOn200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":{"message":"Content Exists Risk","type":"invalid_request_error","code":"content_filter"}}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL, 0).TTS(context.Background(), &TTSRequest{Text: "x"})
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v (%T)", err, err)
	}
	if ae.Status != http.StatusOK || ae.Message != "Content Exists Risk" {
		t.Fatalf("api error = %+v", ae)
	}
}

func TestTTSDoesNotRetry400(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"empty text","type":"invalid_request_error","code":"invalid_request_error"}}`)
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv.URL, 3).TTS(context.Background(), &TTSRequest{Text: ""}); err == nil {
		t.Fatal("expected an error")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
}

func TestUploadOpenFailureDoesNotHitNetwork(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	up := Upload{Name: "x", Size: 1, Open: func() (io.ReadCloser, error) {
		return nil, errors.New("boom")
	}}
	if _, err := newTestClient(t, srv.URL, 0).OCR(context.Background(), up, "text_recognition"); err == nil {
		t.Fatal("expected an error")
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("hits = %d, want 0", got)
	}
}

// dropConnection hijacks the connection and closes it without a response.
func dropConnection(t *testing.T, w http.ResponseWriter) {
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

func TestMultipartResponseLostIsNotRetried(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		readUpload(t, r)     // the whole body reaches the server...
		dropConnection(t, w) // ...then the response is lost
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL, 3).OCR(context.Background(), uploadOf([]byte("payload"), nil), "text_recognition")
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v (%T)", err, err)
	}
	if !te.Sent {
		t.Fatalf("upload was not marked as sent: %+v", te)
	}
	if te.Retryable() {
		t.Fatal("an upload whose response was lost must not be retryable")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want exactly 1", got)
	}
	if !strings.Contains(err.Error(), "已上传") || !strings.Contains(err.Error(), "计费") {
		t.Fatalf("message does not explain the billing risk: %q", err.Error())
	}
}

func TestMultipartRetriesWhenBodyWasNotSent(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		dropConnection(t, w) // close before reading the body
	}))
	defer srv.Close()

	large := bytes.Repeat([]byte("x"), 2<<20)
	_, err := newTestClient(t, srv.URL, 1).OCR(context.Background(), uploadOf(large, nil), "text_recognition")
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v (%T)", err, err)
	}
	if te.Sent {
		t.Fatalf("a partially sent upload must stay retryable: %+v", te)
	}
	if !te.Retryable() {
		t.Fatal("a partially sent upload must stay retryable")
	}
	if got := atomic.LoadInt32(&hits); got < 2 {
		t.Fatalf("hits = %d, want at least 2 attempts", got)
	}
}

func TestOCRGatewayTimeoutIsNotRetried(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		readUpload(t, r)
		w.WriteHeader(http.StatusGatewayTimeout)
		fmt.Fprint(w, `{"error":{"message":"gateway timeout","type":"server_error","code":"server_error"}}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL, 3).OCR(context.Background(), uploadOf([]byte("payload"), nil), "text_recognition")
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v (%T)", err, err)
	}
	if ae.Status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d", ae.Status)
	}
	if ae.Retryable() {
		t.Fatal("OCR 504 (upload+submit over 180s) must not be retryable")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
}

func TestOCRRetriesTransientInBodyError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			readUpload(t, r)
			fmt.Fprint(w, `{"error":{"message":"  The OCR service is temporarily unavailable. Please try again later. ","type":"server_error","code":"server_error"}}`)
			return
		}
		readUpload(t, r)
		fmt.Fprint(w, `{"id":"ocr","object":"ocr.result","page_count":1,"pages":[{"index":0,"text":"ok","finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL, 1).OCR(context.Background(), uploadOf([]byte("payload"), nil), "text_recognition")
	if err != nil {
		t.Fatal(err)
	}
	if resp.PageCount != 1 || len(resp.Pages) != 1 || resp.Pages[0].Text != "ok" {
		t.Fatalf("resp = %+v", resp)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("hits = %d, want 2 (transient 200-body error is retried)", got)
	}
}

func TestOCRInBodyTimeoutIsNotRetried(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		readUpload(t, r)
		fmt.Fprint(w, `{"error":{"message":"OCR did not finish in time","type":"gateway_timeout_error","code":"gateway_timeout_error"}}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL, 3).OCR(context.Background(), uploadOf([]byte("payload"), nil), "text_recognition")
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v (%T)", err, err)
	}
	if ae.Status != http.StatusOK || !strings.Contains(ae.Message, "did not finish") {
		t.Fatalf("api error = %+v", ae)
	}
	if ae.Retryable() {
		t.Fatal("a finished-with-timeout OCR error must not be retryable")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
}

func TestASRGatewayTimeoutKeepsGenericRetry(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusGatewayTimeout)
			fmt.Fprint(w, `{"error":{"message":"gateway timeout","type":"server_error","code":"server_error"}}`)
			return
		}
		readUpload(t, r)
		fmt.Fprint(w, `{"request_id":"r1","text":"ok","duration_ms":1000}`)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL, 1).ASR(context.Background(), uploadOf([]byte("wav"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "ok" {
		t.Fatalf("resp = %+v", resp)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("hits = %d, want 2 (ASR keeps the generic 5xx retry)", got)
	}
}

func TestTransientOCRMessage(t *testing.T) {
	for _, msg := range []string{
		"The OCR service is temporarily unavailable. Please try again later.",
		"  Internal Server Error  ",
		"the ocr service is temporarily unavailable",
	} {
		if !transientOCRMessage(msg) {
			t.Fatalf("%q should be transient", msg)
		}
	}
	for _, msg := range []string{
		"OCR did not finish in time",
		"Internal Server Error: disk full",
		"Failed to recognize the file: broken pdf",
		"",
	} {
		if transientOCRMessage(msg) {
			t.Fatalf("%q should not be transient", msg)
		}
	}
}

func TestTTSStreamAllowsSlowChunks(t *testing.T) {
	const chunks = 8
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/pcm")
		fl, _ := w.(http.Flusher)
		for i := range chunks {
			if _, err := w.Write([]byte{byte(i)}); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "sk-test", 300*time.Millisecond, 0)
	c.Sleep = func(ctx context.Context, d time.Duration) error { return nil }
	body, err := c.TTS(context.Background(), &TTSRequest{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(body)
	if closeErr := body.Close(); closeErr != nil {
		t.Fatalf("close: %v", closeErr)
	}
	if readErr != nil {
		t.Fatalf("read: %v (timeout must not apply to the whole stream)", readErr)
	}
	if len(got) != chunks {
		t.Fatalf("audio = %d bytes, want %d", len(got), chunks)
	}
}

func TestTTSStreamIdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/pcm")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("RIFF"))
		if fl != nil {
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "sk-test", 300*time.Millisecond, 0)
	c.Sleep = func(ctx context.Context, d time.Duration) error { return nil }
	body, err := c.TTS(context.Background(), &TTSRequest{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(body)
	_ = body.Close()
	if readErr == nil {
		t.Fatal("expected the stalled stream to fail")
	}
	if !strings.Contains(readErr.Error(), "未收到数据") || !strings.Contains(readErr.Error(), "300ms") {
		t.Fatalf("error does not name the idle timeout: %v", readErr)
	}
}

// TestTTSHeaderWaitAndFirstByteUseSeparateTimeouts pins that the timeout is
// re-armed when the audio headers arrive: the header wait and the wait for the
// first body byte each get the full budget instead of sharing one deadline
// started at request time.
func TestTTSHeaderWaitAndFirstByteUseSeparateTimeouts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		time.Sleep(600 * time.Millisecond) // header wait
		w.Header().Set("Content-Type", "audio/pcm")
		w.WriteHeader(http.StatusOK)
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(600 * time.Millisecond) // wait for the first audio byte
		_, _ = w.Write([]byte("RIFF"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "sk-test", 1*time.Second, 0)
	c.Sleep = func(ctx context.Context, d time.Duration) error { return nil }
	body, err := c.TTS(context.Background(), &TTSRequest{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(body)
	if closeErr := body.Close(); closeErr != nil {
		t.Fatalf("close: %v", closeErr)
	}
	if readErr != nil {
		t.Fatalf("read: %v (header wait and first byte must not share one deadline)", readErr)
	}
	if string(got) != "RIFF" {
		t.Fatalf("audio = %q", got)
	}
}
