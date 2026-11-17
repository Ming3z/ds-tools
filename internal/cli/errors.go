package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"dstools/internal/deepseek"
)

// Exit codes are the machine-facing contract of the CLI.
const (
	ExitOK = 0 // success, including an empty search result
	// ExitServer covers server-side/request errors (400/402/403/404/413/500/504)
	// and a multipart upload whose response was lost: the file reached the
	// server, so the request must not be retried blindly.
	ExitServer  = 1
	ExitAuth    = 2 // missing or invalid API key (401)
	ExitNetwork = 3 // DNS / connect / TLS / timeout
	ExitUsage   = 4 // local usage error, nothing was sent
)

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func withCode(code int, err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: code, err: err}
}

func usageErrf(format string, a ...any) error {
	return &exitError{code: ExitUsage, err: fmt.Errorf(format, a...)}
}

// codeOf maps any error to the documented exit-code taxonomy.
func codeOf(err error) int {
	if err == nil {
		return ExitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	var ae *deepseek.APIError
	if errors.As(err, &ae) {
		if ae.Status == 401 {
			return ExitAuth
		}
		return ExitServer
	}
	var te *deepseek.TransportError
	if errors.As(err, &te) {
		if te.Sent {
			return ExitServer
		}
		return ExitNetwork
	}
	if errors.Is(err, deepseek.ErrNoAPIKey) {
		return ExitAuth
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ExitNetwork
	}
	return ExitServer
}

// errorPayload is the structured stderr shape used by --error-format json.
type errorPayload struct {
	Status    int    `json:"status"`
	Type      string `json:"type"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	Message   string `json:"message"`
}

func (a *App) printError(err error) {
	if err == nil {
		return
	}
	if a.fErrorFormat == "json" {
		p := errorPayload{Message: err.Error()}
		var ae *deepseek.APIError
		var te *deepseek.TransportError
		switch {
		case errors.As(err, &ae):
			p.Status = ae.Status
			p.Type = ae.Type
			p.Code = ae.Code
			p.Retryable = ae.Retryable()
			if p.Type == "" {
				p.Type = "api_error"
			}
		case errors.As(err, &te):
			p.Status = 0
			p.Type = "network_error"
			p.Code = "network_error"
			if te.Sent {
				p.Code = "response_lost"
			}
			p.Retryable = te.Retryable()
		case errors.Is(err, deepseek.ErrNoAPIKey):
			p.Type = "auth_error"
			p.Code = "missing_api_key"
		default:
			switch codeOf(err) {
			case ExitUsage:
				p.Type, p.Code = "usage_error", "usage_error"
			case ExitAuth:
				p.Type, p.Code = "auth_error", "auth_error"
			case ExitNetwork:
				p.Type, p.Code = "network_error", "network_error"
			default:
				p.Type, p.Code = "api_error", "api_error"
			}
		}
		b, mErr := json.Marshal(p)
		if mErr == nil {
			fmt.Fprintln(a.stderr, string(b))
			return
		}
	}
	fmt.Fprintf(a.stderr, "错误：%v\n", err)
	if codeOf(err) == ExitUsage {
		target := "ds-tools"
		if a.executed != "" && a.executed != "help" {
			target += " " + a.executed
		}
		fmt.Fprintf(a.stderr, "运行 '%s --help' 查看用法\n", target)
	}
}
