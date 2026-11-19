package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// searchEmptyContentResponse has one result without content, so --fill-missing
// has work to do.
const searchEmptyContentResponse = `{"results":[{"rerank_score":2.5,"title":"R","url":"https://ex.com/page","snippet":"s","summary":"","content":"","site_name":"Example","site_icon":null,"date":null,"authority":3}]}`

func TestSearchFillMissingReusesFetchCache(t *testing.T) {
	h := newHarness(t)
	fetches := 0
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tools/search":
			fmt.Fprint(w, searchEmptyContentResponse)
		case "/v1/tools/fetch":
			fetches++
			fmt.Fprint(w, `{"title":"Page","content":"fetched body"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}
	for run := 1; run <= 2; run++ {
		code := h.run("search", "cached fill", "--content", "--fill-missing", "--format", "json", "--fields", "title,url,content")
		if code != 0 {
			t.Fatalf("run %d exit = %d, stderr=%s", run, code, h.err.String())
		}
		if !strings.Contains(h.out.String(), "fetched body") {
			t.Fatalf("run %d lost the fetched content: %s", run, h.out.String())
		}
	}
	if fetches != 1 {
		t.Fatalf("fetch requests = %d, want 1 (the second run must hit the fetch cache)", fetches)
	}
	if h.hits != 2 {
		t.Fatalf("upstream requests = %d, want 2 (search + fetch once, then cached)", h.hits)
	}
}

func TestSearchFillMissingFailureWarnsButSucceeds(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tools/search":
			fmt.Fprint(w, searchEmptyContentResponse)
		case "/v1/tools/fetch":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"message":"no content","type":"invalid_request_error","code":"not_found"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}
	if code := h.run("search", "x", "--content", "--fill-missing", "--format", "json"); code != 0 {
		t.Fatalf("exit = %d, want 0 (a failed fill must not fail the search)", code)
	}
	if !strings.Contains(h.err.String(), "补抓正文失败") {
		t.Fatalf("missing stderr warning: %q", h.err.String())
	}
	if code := h.run("search", "x", "--content", "--fill-missing", "--format", "json", "--quiet"); code != 0 {
		t.Fatalf("quiet exit = %d", code)
	}
	if strings.Contains(h.err.String(), "补抓正文失败") {
		t.Fatalf("--quiet still warned: %q", h.err.String())
	}
}

func TestFetchMixedValidAndInvalidURLExitCode(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/fetch" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"title":"ok","content":"body"}`)
	}
	code := h.run("fetch", "https://ex.com/ok", "ftp://example.com/x", "--format", "jsonl")
	if code != ExitServer {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, ExitServer, h.err.String())
	}
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d: %s", len(lines), h.out.String())
	}
	if strings.Contains(lines[0], `"error"`) || !strings.Contains(lines[0], "https://ex.com/ok") {
		t.Fatalf("first line = %s, want the successful result", lines[0])
	}
	if !strings.Contains(lines[1], `"error"`) || !strings.Contains(lines[1], "ftp://example.com/x") {
		t.Fatalf("second line = %s, want the invalid URL", lines[1])
	}
}

func TestFetchAllInvalidURLsIsUsage(t *testing.T) {
	h := newHarness(t)
	code := h.run("fetch", "ftp://example.com/a", "https://user:pw@example.com/b", "--format", "jsonl")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, ExitUsage, h.err.String())
	}
	if h.hits != 0 {
		t.Fatalf("all-invalid run touched the network: %d requests", h.hits)
	}
}

func TestFetchResultsFollowInputOrder(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"title":"t","content":"c"}`)
	}
	inputs := []string{"https://ex.com/1", "ftp://bad/2", "https://ex.com/3"}
	args := append([]string{"fetch"}, inputs...)
	args = append(args, "--format", "jsonl")
	if code := h.run(args...); code != ExitServer {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, ExitServer, h.err.String())
	}
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if len(lines) != len(inputs) {
		t.Fatalf("lines = %d, want %d: %s", len(lines), len(inputs), h.out.String())
	}
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d is not JSON: %v", i, err)
		}
		if got := obj["url"]; got != inputs[i] {
			t.Fatalf("line %d url = %v, want %q", i, got, inputs[i])
		}
	}
}

func TestFetchDirFileNumbersFollowInputOrder(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"title":"t","content":"c"}`)
	}
	dir := filepath.Join(t.TempDir(), "pages")
	code := h.run("fetch", "https://ex.com/b", "ftp://bad/x", "https://ex.com/a", "-o", dir)
	if code != ExitServer {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, ExitServer, h.err.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"01-ex.com-b.md", "03-ex.com-a.md"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", names, want)
	}
}

func TestWorstFetchErrorRanksBySeverity(t *testing.T) {
	a := &App{}
	cases := []struct {
		name    string
		results []fetchResult
		want    int
	}{
		{"auth beats server", []fetchResult{{Err: "e500", code: ExitServer}, {Err: "e401", code: ExitAuth}}, ExitAuth},
		{"server beats transient", []fetchResult{{Err: "net", code: ExitNetwork}, {Err: "srv", code: ExitServer}}, ExitServer},
		{"transient beats usage", []fetchResult{{Err: "use", code: ExitUsage}, {Err: "net", code: ExitNetwork}}, ExitNetwork},
		{"success only", []fetchResult{{URL: "https://ex.com"}}, ExitOK},
	}
	for _, c := range cases {
		if got := codeOf(a.worstFetchError(c.results)); got != c.want {
			t.Fatalf("%s: exit = %d, want %d", c.name, got, c.want)
		}
	}
}
