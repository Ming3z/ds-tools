package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const (
	pathOCR = "/v1/tools/ocr"
	pathASR = "/v1/tools/asr"
	pathTTS = "/v1/tools/tts"
)

// Official paths for the document/audio tool endpoints.
const (
	PathOCR = pathOCR
	PathASR = pathASR
	PathTTS = pathTTS
)

// Upload is a replayable multipart file source. Open must return a fresh reader
// on every call so that retries re-send the complete file.
type Upload struct {
	Name string
	Size int64
	Open func() (io.ReadCloser, error)
}

// OCRPage is one page of an OCR result.
type OCRPage struct {
	Index        int    `json:"index"`
	Text         string `json:"text"`
	FinishReason string `json:"finish_reason"`
}

// OCRUsage is the token usage of an OCR request.
type OCRUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// OCRResponse is the response of POST /v1/tools/ocr.
type OCRResponse struct {
	ID        string    `json:"id"`
	Object    string    `json:"object"`
	Created   int64     `json:"created"`
	Purpose   string    `json:"purpose"`
	PageCount int       `json:"page_count"`
	Pages     []OCRPage `json:"pages"`
	Usage     OCRUsage  `json:"usage"`
}

// ASRResponse is the response of POST /v1/tools/asr.
type ASRResponse struct {
	RequestID  string `json:"request_id"`
	Text       string `json:"text"`
	DurationMS int    `json:"duration_ms"`
}

// TTSRequest is the JSON body of POST /v1/tools/tts.
type TTSRequest struct {
	Text           string `json:"text"`
	VoiceID        string `json:"voice_id,omitempty"`
	Format         string `json:"format,omitempty"`
	FilterMarkdown *bool  `json:"filter_markdown,omitempty"`
}

// OCR calls POST /v1/tools/ocr with a multipart file upload.
func (c *Client) OCR(ctx context.Context, up Upload, purpose string) (*OCRResponse, error) {
	var out OCRResponse
	fields := [][2]string{{"purpose", purpose}}
	if err := c.doMultipart(ctx, pathOCR, up, fields, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ASR calls POST /v1/tools/asr with a multipart file upload.
func (c *Client) ASR(ctx context.Context, up Upload) (*ASRResponse, error) {
	var out ASRResponse
	if err := c.doMultipart(ctx, pathASR, up, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TTS calls POST /v1/tools/tts and returns the streaming audio body. The caller
// must close it. Retries happen only before the body is handed back.
//
// The client timeout is not a deadline on the audio stream: it bounds the wait
// for the response headers and the gap between two successful body reads. A
// stalled stream is aborted by cancelling the per-attempt context.
func (c *Client) TTS(ctx context.Context, req *TTSRequest) (io.ReadCloser, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("编码请求失败：%w", err)
	}
	if c.HTTP == nil {
		return nil, errors.New("HTTP 客户端未初始化")
	}
	timeout := c.HTTP.Timeout

	var lastErr error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt)
			if ae, ok := lastErr.(*APIError); ok && ae.RetryAfter > delay {
				delay = ae.RetryAfter
			}
			if delay > maxBackoff {
				delay = maxBackoff
			}
			if err := c.Sleep(ctx, delay); err != nil {
				return nil, err
			}
		}

		// Drop the total deadline: the stream may legitimately run for
		// minutes. Our own timers enforce the header wait and the read idle.
		httpClient := *c.HTTP
		httpClient.Timeout = 0

		body, err := c.ttsAttempt(ctx, &httpClient, payload, timeout)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, &TransportError{Err: ctx.Err()}
			}
			var ae *APIError
			if errors.As(err, &ae) {
				if !ae.Retryable() {
					return nil, err
				}
				continue
			}
			var te *TransportError
			if !errors.As(err, &te) {
				return nil, err // local failure (e.g. building the request)
			}
			if !te.Retryable() {
				return nil, err
			}
			continue
		}
		return body, nil
	}
	if lastErr == nil {
		lastErr = errors.New("请求失败")
	}
	return nil, lastErr
}

// ttsAttempt performs one TTS request. On success the returned stream owns the
// attempt context and its idle timer until Close.
func (c *Client) ttsAttempt(ctx context.Context, httpClient *http.Client, payload []byte, timeout time.Duration) (io.ReadCloser, error) {
	attemptCtx, cancel := context.WithCancel(ctx)
	idle := newIdleTimeout(timeout, cancel)

	httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.URL(pathTTS), bytes.NewReader(payload))
	if err != nil {
		idle.stop()
		cancel()
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "audio/wav, audio/pcm, application/json")
	httpReq.Header.Set("User-Agent", "ds-tools")

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		idle.stop()
		cancel()
		if idle.expired() {
			return nil, &TransportError{Err: fmt.Errorf("等待响应头超过 %s 未返回：%w", timeout, err)}
		}
		return nil, &TransportError{Err: err}
	}

	if resp.StatusCode == http.StatusOK {
		ct := strings.ToLower(resp.Header.Get("Content-Type"))
		if strings.Contains(ct, "application/json") {
			b, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			idle.stop()
			_ = resp.Body.Close()
			cancel()
			if readErr != nil {
				return nil, &TransportError{Err: readErr}
			}
			return nil, parseAPIError(http.StatusOK, b, 0)
		}
		// Headers are in: restart the watchdog so the wait for the first audio
		// byte gets the full timeout, not the leftovers of the header wait.
		idle.arm()
		return &idleStream{body: resp.Body, idle: idle, cancel: cancel, timeout: timeout}, nil
	}

	b, readErr := io.ReadAll(resp.Body)
	idle.stop()
	_ = resp.Body.Close()
	cancel()
	if readErr != nil {
		return nil, &TransportError{Err: readErr}
	}
	return nil, parseAPIError(resp.StatusCode, b, parseRetryAfter(resp.Header.Get("Retry-After")))
}

// idleStream is the TTS audio body with a read-idle watchdog: if no Read
// returns data for the configured timeout, the attempt context is cancelled and
// the pending Read fails with an error naming the timeout.
type idleStream struct {
	body    io.ReadCloser
	idle    *idleTimeout
	cancel  context.CancelFunc
	timeout time.Duration
}

func (s *idleStream) Read(p []byte) (int, error) {
	n, err := s.body.Read(p)
	if err != nil {
		s.idle.stop()
		if s.idle.expired() {
			return n, fmt.Errorf("音频流超过 %s 未收到数据：%w", s.timeout, err)
		}
		return n, err
	}
	s.idle.arm()
	return n, nil
}

func (s *idleStream) Close() error {
	s.idle.stop()
	err := s.body.Close()
	s.cancel()
	return err
}

// idleTimeout cancels an attempt context when nothing arrives for d. The timer
// is re-armed after every successful read; d <= 0 disables it.
type idleTimeout struct {
	d      time.Duration
	timer  *time.Timer
	cancel context.CancelFunc
	fired  atomic.Bool
}

func newIdleTimeout(d time.Duration, cancel context.CancelFunc) *idleTimeout {
	t := &idleTimeout{d: d, cancel: cancel}
	if d > 0 {
		t.timer = time.AfterFunc(d, func() {
			t.fired.Store(true)
			cancel()
		})
	}
	return t
}

// arm restarts the watchdog after a successful read.
func (t *idleTimeout) arm() {
	if t.timer != nil && !t.fired.Load() {
		t.timer.Reset(t.d)
	}
}

// stop prevents a pending expiry; one that already happened stays recorded.
func (t *idleTimeout) stop() {
	if t.timer != nil {
		t.timer.Stop()
	}
}

func (t *idleTimeout) expired() bool { return t.fired.Load() }

func (c *Client) doMultipart(ctx context.Context, path string, up Upload, fields [][2]string, out any) error {
	if up.Open == nil {
		return errors.New("上传源不可用")
	}
	probe, err := up.Open()
	if err != nil {
		return fmt.Errorf("打开上传文件失败：%w", err)
	}
	_ = probe.Close()

	boundary, contentLength := multipartShape(up, fields)

	var lastErr error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt)
			if ae, ok := lastErr.(*APIError); ok && ae.RetryAfter > delay {
				delay = ae.RetryAfter
			}
			if delay > maxBackoff {
				delay = maxBackoff
			}
			if err := c.Sleep(ctx, delay); err != nil {
				return err
			}
		}

		resp, err := c.attemptMultipart(ctx, path, up, fields, boundary, contentLength)
		if err != nil {
			lastErr = err
			var te *TransportError
			sent := errors.As(err, &te) && te.Sent
			if ctx.Err() != nil {
				return &TransportError{Err: ctx.Err(), Sent: sent}
			}
			if sent {
				// The server already received the whole file; resending it
				// could bill the same recognition twice.
				return err
			}
			continue
		}

		if resp.status == http.StatusOK {
			// OCR can return an error object with HTTP 200 after the upload
			// succeeded (e.g. gateway_timeout_error). Check before decoding.
			var env errorEnvelope
			if err := json.Unmarshal(resp.body, &env); err == nil && env.Error.Message != "" {
				apiErr := applyRetryPolicy(path, parseAPIError(http.StatusOK, resp.body, 0))
				lastErr = apiErr
				if !apiErr.Retryable() {
					return apiErr
				}
				continue
			}
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(resp.body, out); err != nil {
				return fmt.Errorf("解析响应失败：%w", err)
			}
			return nil
		}

		apiErr := applyRetryPolicy(path, parseAPIError(resp.status, resp.body, resp.retryAfter))
		lastErr = apiErr
		if !apiErr.Retryable() {
			return apiErr
		}
	}
	return lastErr
}

// multipartShape computes a stable boundary and the exact Content-Length by
// writing only the multipart headers to a buffer. up.Size must be known.
func multipartShape(up Upload, fields [][2]string) (string, int64) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		_ = mw.WriteField(f[0], f[1])
	}
	if _, err := mw.CreateFormFile("file", up.Name); err != nil {
		return mw.Boundary(), -1
	}
	_ = mw.Close()
	if up.Size < 0 {
		return mw.Boundary(), -1
	}
	return mw.Boundary(), int64(buf.Len()) + up.Size
}

func (c *Client) attemptMultipart(ctx context.Context, path string, up Upload, fields [][2]string, boundary string, contentLength int64) (*rawResponse, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	if boundary != "" {
		if err := mw.SetBoundary(boundary); err != nil {
			_ = pr.Close()
			return nil, err
		}
	}
	contentType := mw.FormDataContentType()

	// sent reports whether the whole multipart body reached the transport.
	// Pipe writes block until the transport consumes them, so a successful
	// mw.Close() proves every byte was handed over: the server now owns the
	// upload and a retry would bill the same file twice.
	sent := make(chan bool, 1)

	go func() {
		err := func() error {
			for _, f := range fields {
				if err := mw.WriteField(f[0], f[1]); err != nil {
					return err
				}
			}
			part, err := mw.CreateFormFile("file", up.Name)
			if err != nil {
				return err
			}
			f, err := up.Open()
			if err != nil {
				return fmt.Errorf("打开上传文件失败：%w", err)
			}
			_, copyErr := io.Copy(part, f)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			return mw.Close()
		}()
		if err != nil {
			_ = pw.CloseWithError(err)
			sent <- false
			return
		}
		_ = pw.Close()
		sent <- true
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL(path), pr)
	if err != nil {
		_ = pr.Close()
		return nil, err
	}
	if contentLength > 0 {
		req.ContentLength = contentLength
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ds-tools")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		_ = pr.Close()
		return nil, &TransportError{Err: err, Sent: <-sent}
	}
	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = pr.Close()
		return nil, &TransportError{Err: err, Sent: <-sent}
	}
	_ = pr.Close()
	<-sent
	return &rawResponse{
		status:     resp.StatusCode,
		body:       b,
		retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}, nil
}
