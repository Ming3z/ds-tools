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
	if hits != 2 {
		t.Fatalf("hits = %d, want 2", hits)
	}
	if opens < 2 {
		t.Fatalf("upload was not re-opened for the retry: opens=%d", opens)
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
	if hits != 2 {
		t.Fatalf("hits = %d, want 2", hits)
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
	if hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
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
	if hits != 0 {
		t.Fatalf("hits = %d, want 0", hits)
	}
}
