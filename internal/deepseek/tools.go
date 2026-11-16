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
func (c *Client) TTS(ctx context.Context, req *TTSRequest) (io.ReadCloser, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("编码请求失败：%w", err)
	}

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

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL(pathTTS), bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "audio/wav, audio/pcm, application/json")
		httpReq.Header.Set("User-Agent", "ds-tools")

		resp, err := c.HTTP.Do(httpReq)
		if err != nil {
			lastErr = &TransportError{Err: err}
			if ctx.Err() != nil {
				return nil, &TransportError{Err: ctx.Err()}
			}
			continue
		}

		if resp.StatusCode == http.StatusOK {
			ct := strings.ToLower(resp.Header.Get("Content-Type"))
			if strings.Contains(ct, "application/json") {
				b, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				_ = resp.Body.Close()
				if readErr != nil {
					return nil, &TransportError{Err: readErr}
				}
				return nil, parseAPIError(http.StatusOK, b, 0)
			}
			return resp.Body, nil
		}

		b, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = &TransportError{Err: readErr}
			continue
		}
		apiErr := parseAPIError(resp.StatusCode, b, parseRetryAfter(resp.Header.Get("Retry-After")))
		lastErr = apiErr
		if !apiErr.Retryable() {
			return nil, apiErr
		}
	}
	if lastErr == nil {
		lastErr = errors.New("请求失败")
	}
	return nil, lastErr
}

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
			if ctx.Err() != nil {
				return &TransportError{Err: ctx.Err()}
			}
			continue
		}

		if resp.status == http.StatusOK {
			if out == nil {
				return nil
			}
			// OCR can return an error object with HTTP 200 after the upload
			// succeeded (e.g. gateway_timeout_error). Check before decoding.
			var env errorEnvelope
			if err := json.Unmarshal(resp.body, &env); err == nil && env.Error.Message != "" {
				return parseAPIError(http.StatusOK, resp.body, 0)
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
			return
		}
		_ = pw.Close()
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
		return nil, &TransportError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = pr.Close()
		return nil, &TransportError{Err: err}
	}
	_ = pr.Close()
	return &rawResponse{
		status:     resp.StatusCode,
		body:       b,
		retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}, nil
}
