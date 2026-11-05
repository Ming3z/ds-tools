package deepseek

import (
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

func newTestClient(t *testing.T, url string, retries int) *Client {
	t.Helper()
	c := NewClient(url, "sk-test", 5*time.Second, retries)
	c.Sleep = func(ctx context.Context, d time.Duration) error { return nil }
	return c
}

func TestSearchSendsDocumentedRequest(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/v1/tools/search" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %s", ct)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer sk-test" {
			t.Errorf("authorization = %s", auth)
		}
		_ = decodeJSON(r, &got)
		fmt.Fprint(w, `{"results":[{"rerank_score":2.4,"title":"t","url":"u","snippet":"s","summary":"sm","content":"","site_name":null,"site_icon":null,"date":null,"authority":3}]}`)
	}))
	defer srv.Close()

	q := "original question"
	resp, err := newTestClient(t, srv.URL, 0).Search(context.Background(), &SearchRequest{
		Query: "hello", Count: 5, NeedContent: true, OriQuestion: &q, Localization: "1.2.3.4",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].RerankScore != 2.4 {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Results[0].SiteName != nil {
		t.Fatal("null site_name should stay nil")
	}
	if got["ori_question"] != q || got["need_content"] != true || got["localization"] != "1.2.3.4" {
		t.Fatalf("request body = %v", got)
	}
}

func TestRetriesStopAtBudget(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"boom","type":"server_error","code":"server_error"}}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL, 2).Search(context.Background(), &SearchRequest{Query: "x", Count: 1})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 503 {
		t.Fatalf("err = %v", err)
	}
	if hits != 3 {
		t.Fatalf("attempts = %d, want 3 (1 + 2 retries)", hits)
	}
}

func TestNonRetryableIsNotRetried(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"'count' must be between 1 and 50","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL, 3).Search(context.Background(), &SearchRequest{Query: "x", Count: 0})
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v", err)
	}
	if ae.Retryable() {
		t.Fatal("400 must not be retryable")
	}
	if !strings.Contains(ae.Message, "count") {
		t.Fatalf("message = %q", ae.Message)
	}
	if hits != 1 {
		t.Fatalf("attempts = %d, want 1", hits)
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"slow","type":"rate_limit_error","code":"rate_limit_error"}}`)
			return
		}
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer srv.Close()

	var slept time.Duration
	c := NewClient(srv.URL, "sk-test", 5*time.Second, 1)
	c.Sleep = func(ctx context.Context, d time.Duration) error {
		slept = d
		return nil
	}
	if _, err := c.Search(context.Background(), &SearchRequest{Query: "x", Count: 1}); err != nil {
		t.Fatal(err)
	}
	if slept != 7*time.Second {
		t.Fatalf("slept %v, want 7s from Retry-After", slept)
	}
}

func TestTransportErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening any more

	_, err := newTestClient(t, url, 1).Search(context.Background(), &SearchRequest{Query: "x", Count: 1})
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v (%T)", err, err)
	}
}

func TestFetchDecodesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/fetch" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"title":"Example article","content":"# Example article\n\nArticle content."}`)
	}))
	defer srv.Close()

	page, err := newTestClient(t, srv.URL, 0).Fetch(context.Background(), &FetchRequest{URL: "https://example.com/a"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Title != "Example article" || !strings.HasPrefix(page.Content, "# Example article") {
		t.Fatalf("page = %+v", page)
	}
}

func TestBalanceUsesGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/user/balance" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"9.00"}]}`)
	}))
	defer srv.Close()

	bal, err := newTestClient(t, srv.URL, 0).Balance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bal.IsAvailable || bal.BalanceInfos[0].TotalBalance != "9.00" {
		t.Fatalf("balance = %+v", bal)
	}
}

func decodeJSON(r *http.Request, out any) error {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
