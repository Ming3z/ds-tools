package deepseek

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
// 408, 429 and 5xx. Every other 4xx must not be retried.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusRequestTimeout ||
		e.Status == http.StatusTooManyRequests ||
		e.Status >= 500
}

// TransportError is a DNS/connect/TLS/timeout failure; always retryable.
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

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
