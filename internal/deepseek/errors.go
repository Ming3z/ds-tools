package deepseek

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrNoAPIKey is returned when no credential could be resolved.
var ErrNoAPIKey = errors.New("未配置 API Key")

// APIError is a non-2xx response from the DeepSeek API. The wire format is
// OpenAI compatible, with "param" always null.
type APIError struct {
	Status     int
	Type       string
	Code       string
	Message    string
	RetryAfter time.Duration

	// retry overrides the generic status rule when an endpoint has its own
	// policy (OCR). It is set by the retry loop itself, so Retryable always
	// reports the decision the loop actually took.
	retry *bool
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Type != "" {
		return fmt.Sprintf("%s（HTTP %d，type=%s）", msg, e.Status, e.Type)
	}
	return fmt.Sprintf("%s（HTTP %d）", msg, e.Status)
}

// Retryable reports whether the official docs advise retrying this failure:
// 408, 429 and 5xx. Every other 4xx must not be retried. Endpoints with their
// own policy (OCR) set retry explicitly.
func (e *APIError) Retryable() bool {
	if e.retry != nil {
		return *e.retry
	}
	return e.Status == http.StatusRequestTimeout ||
		e.Status == http.StatusTooManyRequests ||
		e.Status >= 500
}

// TransportError is a DNS/connect/TLS/timeout failure. It is retryable unless
// the request body had already been handed to the transport (Sent): the server
// may then still be processing — and billing — the upload, so resending would
// double-charge. Sent is only set for multipart uploads.
type TransportError struct {
	Err  error
	Sent bool
}

func (e *TransportError) Error() string {
	if e.Sent {
		return fmt.Sprintf("文件已上传，但等待结果时连接中断：%v；服务端可能仍在处理并计费，请勿立即重试", e.Err)
	}
	return e.Err.Error()
}

func (e *TransportError) Unwrap() error { return e.Err }

// Retryable reports whether sending the request again is safe.
func (e *TransportError) Retryable() bool { return !e.Sent }

// transientOCRMessage reports whether an HTTP-200 OCR body error is one of the
// two documented transient failures.
func transientOCRMessage(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	return strings.Contains(m, "temporarily unavailable") || m == "internal server error"
}

// applyRetryPolicy records the endpoint-specific retry decision on e so that
// Retryable() and the retry loop can never disagree. Only OCR overrides the
// generic 408/429/5xx rule.
func applyRetryPolicy(path string, e *APIError) *APIError {
	if path != pathOCR {
		return e
	}
	var retry bool
	switch {
	case e.Status == http.StatusOK:
		// Recognition already started and is billed; only the two transient
		// messages are worth another upload.
		retry = transientOCRMessage(e.Message)
	case e.Status == http.StatusGatewayTimeout:
		// Upload + submit exceeded the server's 180 s budget. Re-uploading a
		// smaller file is the fix; a retry would just burn another 180 s.
		retry = false
	default:
		return e
	}
	e.retry = &retry
	return e
}

type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   any    `json:"param"`
		Code    string `json:"code"`
	} `json:"error"`
}

func parseAPIError(status int, body []byte, retryAfter time.Duration) *APIError {
	e := &APIError{Status: status, RetryAfter: retryAfter}
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		e.Message = env.Error.Message
		e.Type = env.Error.Type
		e.Code = env.Error.Code
	} else if s := string(body); len(s) > 0 && len(s) < 512 {
		e.Message = s
	}
	return e
}
