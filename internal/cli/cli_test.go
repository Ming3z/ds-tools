package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"dstools/internal/config"
	"dstools/internal/skill"
)

// harness runs the CLI with an isolated config dir and captured streams.
type harness struct {
	t      *testing.T
	srv    *httptest.Server
	out    bytes.Buffer
	err    bytes.Buffer
	dir    string
	hits   int32
	handle func(w http.ResponseWriter, r *http.Request)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DS_TOOLS_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("DEEPSEEK_API_KEY", "sk-test-key")
	t.Setenv("DEEPSEEK_BASE_URL", "")
	t.Setenv("DS_TOOLS_FORMAT", "")
	t.Setenv("DS_TOOLS_SKILL_DIR", "")
	t.Setenv("NO_COLOR", "1")

	h := &harness{t: t, dir: dir}
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&h.hits, 1)
		h.handle(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) run(args ...string) int {
	h.t.Helper()
	h.out.Reset()
	h.err.Reset()
	full := append([]string{"--base-url", h.srv.URL, "--retries", "0"}, args...)
	return Execute(full, strings.NewReader(""), &h.out, &h.err)
}

func (h *harness) runStdin(stdin string, args ...string) int {
	h.t.Helper()
	h.out.Reset()
	h.err.Reset()
	full := append([]string{"--base-url", h.srv.URL, "--retries", "0"}, args...)
	return Execute(full, strings.NewReader(stdin), &h.out, &h.err)
}

func (h *harness) jsonOut(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &v); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, h.out.String())
	}
	return v
}

func searchResponse(n int) string {
	items := make([]string, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, fmt.Sprintf(`{
			"rerank_score": %.2f,
			"title": "Result %d",
			"url": "https://example.com/%d",
			"snippet": "snippet %d",
			"summary": "summary %d",
			"content": "content %d",
			"site_name": "Example",
			"site_icon": null,
			"date": "2026-06-01T00:00:00+00:00",
			"authority": 3
		}`, 2.5-float64(i), i, i, i, i, i))
	}
	return `{"results":[` + strings.Join(items, ",") + `]}`
}

func TestSearchJSONProjection(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/search" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test-key" {
			t.Errorf("auth header = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["query"] != "hello world" {
			t.Errorf("query = %v", req["query"])
		}
		if req["count"] != float64(2) {
			t.Errorf("count = %v", req["count"])
		}
		fmt.Fprint(w, searchResponse(2))
	}

	if code := h.run("search", "hello", "world", "-n", "2", "--format", "json", "--fields", "title,url,score"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	v := h.jsonOut(t)
	results := v["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
	first := results[0].(map[string]any)
	if _, ok := first["rerank_score"]; !ok {
		t.Fatalf("alias score was not mapped to rerank_score: %v", first)
	}
	if len(first) != 3 {
		t.Fatalf("projection kept %d fields: %v", len(first), first)
	}
}

func TestSearchJSONLOrderPreserved(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, searchResponse(1)) }
	if code := h.run("search", "x", "--format", "jsonl", "--fields", "url,title"); code != 0 {
		t.Fatalf("exit = %d: %s", code, h.err.String())
	}
	line := strings.TrimSpace(h.out.String())
	if !strings.HasPrefix(line, `{"url":"https://example.com/0","title":"Result 0"}`) {
		t.Fatalf("jsonl = %s", line)
	}
}

func TestSearchEmptyResultsIsSuccess(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"results":[]}`) }
	if code := h.run("search", "nothing", "--format", "json"); code != 0 {
		t.Fatalf("empty result must exit 0, got %d", code)
	}
	v := h.jsonOut(t)
	if len(v["results"].([]any)) != 0 {
		t.Fatalf("expected empty results")
	}
}

func TestSearchFilters(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, searchResponse(3)) }
	if code := h.run("search", "x", "--format", "jsonl", "--min-score", "1.8"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	// scores are 2.50, 1.50, 0.50 -> only the first survives 1.8
	if got := len(strings.Split(strings.TrimSpace(h.out.String()), "\n")); got != 1 {
		t.Fatalf("expected 1 result, got %d lines: %s", got, h.out.String())
	}
}

func TestAuthAndServerExitCodes(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   int
	}{
		{401, `{"error":{"message":"invalid key","type":"authentication_error","param":null,"code":"invalid_api_key"}}`, ExitAuth},
		{402, `{"error":{"message":"insufficient balance","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`, ExitServer},
		{400, `{"error":{"message":"'count' must be between 1 and 50","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`, ExitServer},
		{504, `{"error":{"message":"timeout","type":"server_error","param":null,"code":"server_error"}}`, ExitServer},
	}
	for _, c := range cases {
		h := newHarness(t)
		h.handle = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			fmt.Fprint(w, c.body)
		}
		if code := h.run("search", "x"); code != c.want {
			t.Fatalf("status %d -> exit %d, want %d", c.status, code, c.want)
		}
	}
}

func TestStructuredErrorOutput(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit_error","param":null,"code":"rate_limit_error"}}`)
	}
	if code := h.run("search", "x", "--error-format", "json"); code != ExitServer {
		t.Fatalf("exit = %d", code)
	}
	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(h.err.Bytes()), &payload); err != nil {
		t.Fatalf("stderr not JSON: %v (%s)", err, h.err.String())
	}
	if payload["retryable"] != true || payload["status"] != float64(429) {
		t.Fatalf("payload = %v", payload)
	}
}

func TestRetryOn429ThenSuccess(t *testing.T) {
	h := newHarness(t)
	var calls int32
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit_error"}}`)
			return
		}
		fmt.Fprint(w, searchResponse(1))
	}
	// --retries 1 overrides the harness default of 0.
	full := []string{"--base-url", h.srv.URL, "--retries", "1"}
	full = append(full, "search", "x", "--format", "json")
	if code := Execute(full, strings.NewReader(""), &h.out, &h.err); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	if calls != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls)
	}
}

func TestUsageErrorsNeverTouchTheNetwork(t *testing.T) {
	cases := [][]string{
		{"search", "x", "-n", "0"},
		{"search", "x", "-n", "51"},
		{"search", "x", "--fill-missing"},
		{"search", "x", "--limit", "-1"},
		{"search", "x", "--fields", "nope"},
		{"search", "x", "--format", "xml"},
		{"fetch", "ftp://example.com/x"},
		{"fetch", "https://user:pw@example.com/x"},
		{"search", strings.Repeat("a", 129)},
	}
	for _, args := range cases {
		h := newHarness(t)
		if code := h.run(args...); code != ExitUsage {
			t.Fatalf("%v -> exit %d, want 4 (stderr=%s)", args, code, h.err.String())
		}
		if h.hits != 0 {
			t.Fatalf("%v hit the network", args)
		}
	}
}

func TestDryRunPrintsWithoutSending(t *testing.T) {
	h := newHarness(t)
	if code := h.run("search", "x", "--content", "--dry-run"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if h.hits != 0 {
		t.Fatal("dry-run made a request")
	}
	if !strings.Contains(h.out.String(), "/v1/tools/search") || !strings.Contains(h.out.String(), `"need_content": true`) {
		t.Fatalf("dry-run output = %s", h.out.String())
	}
	if strings.Contains(h.out.String(), "sk-test-key") {
		t.Fatal("dry-run leaked the API key")
	}
}

func TestFetchMultipleDedupeAndPacing(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		var req struct{ URL string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.URL, "bad") {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"message":"no markdown content","type":"invalid_request_error","code":"not_found"}}`)
			return
		}
		fmt.Fprintf(w, `{"title":"T %s","content":"# %s"}`, req.URL, req.URL)
	}
	code := h.run("fetch", "https://ex.com/a", "https://ex.com/a#frag", "https://ex.com/a?utm_source=x", "--format", "json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	// three inputs canonicalise to one URL -> one request
	if h.hits != 1 {
		t.Fatalf("expected 1 request after de-duplication, got %d", h.hits)
	}
	v := h.jsonOut(t)
	if v["url"] != "https://ex.com/a" {
		t.Fatalf("url = %v", v["url"])
	}
}

func TestFetchPartialFailureWinsExitCode(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		var req struct{ URL string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.URL, "bad") {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"message":"no markdown content","type":"invalid_request_error","code":"not_found"}}`)
			return
		}
		fmt.Fprintf(w, `{"title":"ok","content":"body"}`)
	}
	code := h.run("fetch", "https://ex.com/good", "https://ex.com/bad", "--format", "jsonl")
	if code != ExitServer {
		t.Fatalf("exit = %d, want %d", code, ExitServer)
	}
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d: %s", len(lines), h.out.String())
	}
	if !strings.Contains(lines[1], `"error"`) {
		t.Fatalf("second line = %s", lines[1])
	}
}

func TestFetchOffsetLimit(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"title":"t","content":"0123456789"}`)
	}
	if code := h.run("fetch", "https://ex.com/doc", "--offset", "3", "--limit", "4", "--raw"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if got := strings.TrimSpace(h.out.String()); got != "3456" {
		t.Fatalf("window = %q", got)
	}
}

func TestCacheAvoidsSecondRequest(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, searchResponse(1)) }
	if code := h.run("search", "cached", "--format", "json"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if code := h.run("search", "cached", "--format", "json"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if h.hits != 1 {
		t.Fatalf("expected 1 upstream request, got %d", h.hits)
	}
	// --no-cache must re-request
	if code := h.run("search", "cached", "--format", "json", "--no-cache"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if h.hits != 2 {
		t.Fatalf("--no-cache should hit upstream, got %d hits", h.hits)
	}
}

func TestCacheDirFollowsConfigFlag(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, searchResponse(1)) }

	// A --config file that is neither named config.json nor in the default dir.
	cfgPath := filepath.Join(h.dir, "custom-name.json")
	for range 2 {
		if code := h.run("--config", cfgPath, "search", "cached", "--format", "json"); code != 0 {
			t.Fatalf("exit = %d", code)
		}
	}
	if h.hits != 1 {
		t.Fatalf("expected a cache hit, got %d upstream requests", h.hits)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "cache")); err != nil {
		t.Fatalf("cache dir not next to --config: %v", err)
	}
	if fi, err := os.Stat(cfgPath); err == nil && fi.IsDir() {
		t.Fatalf("cache was written under the config file path %s", cfgPath)
	}
}

func TestCachePathExpandsTildeConfig(t *testing.T) {
	h := newHarness(t)
	t.Setenv("HOME", h.dir)
	if code := h.run("--config", "~/conf/custom.json", "cache", "path"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	want := filepath.Join(h.dir, "conf", "cache")
	if got := strings.TrimSpace(h.out.String()); got != want {
		t.Fatalf("cache path = %q, want %q", got, want)
	}
}

func TestInitFromStdin(t *testing.T) {
	h := newHarness(t)
	t.Setenv("DEEPSEEK_API_KEY", "")
	if code := h.runStdin("sk-abc123456789\n", "init", "--api-key-stdin", "--no-verify"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	cfgPath := filepath.Join(h.dir, "config.json")
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config mode = %#o, want 0600", perm)
	}
	if h.out.String()+" "+h.err.String() == "" {
		t.Fatal("no output")
	}
	// The key must never be printed in full.
	if strings.Contains(h.out.String(), "sk-abc123456789") {
		t.Fatalf("init leaked the key: %s", h.out.String())
	}
}

func TestParseSkillSelection(t *testing.T) {
	cases := []struct {
		in      string
		n       int
		want    []int
		wantErr bool
	}{
		{"", 3, nil, false},
		{"all", 3, []int{0, 1, 2}, false},
		{"1,3", 3, []int{0, 2}, false},
		{"2 1", 3, []int{0, 1}, false},
		{"1，3", 3, []int{0, 2}, false},
		{"3,3,1", 3, []int{0, 2}, false},
		{"4", 3, nil, true},
		{"0", 3, nil, true},
		{"x", 3, nil, true},
	}
	for _, c := range cases {
		got, err := parseSkillSelection(c.in, c.n)
		if c.wantErr {
			if err == nil {
				t.Fatalf("parseSkillSelection(%q) expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseSkillSelection(%q): %v", c.in, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("parseSkillSelection(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Fatalf("parseSkillSelection(%q) = %v, want %v", c.in, got, c.want)
			}
		}
	}
}

func TestResolvedBaseURL(t *testing.T) {
	t.Setenv("DEEPSEEK_BASE_URL", "")
	cfg := &config.Config{}
	if v, src := resolvedBaseURL(cfg); v != config.DefaultBaseURL || src != "default" {
		t.Fatalf("empty config = %q/%q", v, src)
	}
	cfg.BaseURL = "https://config.example"
	if v, src := resolvedBaseURL(cfg); v != cfg.BaseURL || src != "config" {
		t.Fatalf("config base = %q/%q", v, src)
	}
	t.Setenv("DEEPSEEK_BASE_URL", "https://env.example/")
	if v, src := resolvedBaseURL(cfg); v != "https://env.example" || src != "env" {
		t.Fatalf("env base = %q/%q", v, src)
	}
}

func TestInitRefusesOverwriteWithoutForce(t *testing.T) {
	h := newHarness(t)
	t.Setenv("DEEPSEEK_API_KEY", "")
	if code := h.runStdin("sk-first-000000\n", "init", "--api-key-stdin", "--no-verify"); code != 0 {
		t.Fatalf("first init exit = %d", code)
	}
	if code := h.runStdin("sk-second-11111\n", "init", "--api-key-stdin", "--no-verify"); code != ExitUsage {
		t.Fatalf("overwrite without --force exit = %d, want 4", code)
	}
	if code := h.runStdin("sk-second-11111\n", "init", "--api-key-stdin", "--no-verify", "--force"); code != 0 {
		t.Fatalf("forced init exit = %d, stderr=%s", code, h.err.String())
	}
}

func TestDoctorReportsMissingKey(t *testing.T) {
	h := newHarness(t)
	t.Setenv("DEEPSEEK_API_KEY", "")
	if code := h.run("doctor"); code != ExitAuth {
		t.Fatalf("exit = %d, want 2 (stderr=%s out=%s)", code, h.err.String(), h.out.String())
	}
}

func TestDoctorProbeValidatesKey(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/balance" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"42.00","granted_balance":"0.00","topped_up_balance":"42.00"}]}`)
	}
	if code := h.run("doctor", "--probe", "--format", "json"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	if !strings.Contains(h.out.String(), "42.00 CNY") {
		t.Fatalf("balance missing from output: %s", h.out.String())
	}
}

func TestVersionAndSkillEmbedded(t *testing.T) {
	h := newHarness(t)
	if code := h.run("version", "--format", "json"); code != 0 {
		t.Fatalf("version exit = %d", code)
	}
	if !strings.Contains(h.out.String(), `"version"`) {
		t.Fatalf("version output = %s", h.out.String())
	}

	dir := t.TempDir()
	t.Setenv(config.EnvSkill, dir)
	h.out.Reset()
	if code := h.run("skill", "install", "--format", "text"); code != 0 {
		t.Fatalf("skill install exit = %d, stderr=%s", code, h.err.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "ds-tools", "SKILL.md")); err != nil {
		t.Fatalf("skill not installed: %v", err)
	}
	if !strings.Contains(h.out.String(), "已安装") {
		t.Fatalf("first install should report 已安装: %s", h.out.String())
	}

	h.out.Reset()
	if code := h.run("skill", "install", "--format", "text"); code != 0 {
		t.Fatalf("re-install exit = %d, stderr=%s", code, h.err.String())
	}
	if !strings.Contains(h.out.String(), "已是最新") {
		t.Fatalf("re-install should be idempotent: %s", h.out.String())
	}
}

func TestSkillInstallRequiresExplicitTarget(t *testing.T) {
	h := newHarness(t)
	t.Setenv("HOME", h.dir)
	t.Setenv(config.EnvSkill, "")

	if code := h.run("skill", "install"); code != ExitUsage {
		t.Fatalf("install without target exit = %d, want 4", code)
	}
	if !strings.Contains(h.err.String(), "--claude") {
		t.Fatalf("error should mention the harness flags: %s", h.err.String())
	}

	if code := h.run("skill", "install", "--all", "--claude"); code != ExitUsage {
		t.Fatalf("--all with --claude exit = %d, want 4", code)
	}
}

func TestSkillInstallAllCreatesThreeDirs(t *testing.T) {
	h := newHarness(t)
	t.Setenv("HOME", h.dir)

	if code := h.run("skill", "install", "--all", "--format", "json"); code != 0 {
		t.Fatalf("install --all exit = %d, stderr=%s", code, h.err.String())
	}
	var got skillOutput
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, h.out.String())
	}
	if len(got.Results) != 3 {
		t.Fatalf("results = %v, want 3 targets", got.Results)
	}
	for _, result := range got.Results {
		if result.Action != "installed" {
			t.Fatalf("action = %q, want installed", result.Action)
		}
		if _, err := os.Stat(result.Path); err != nil {
			t.Fatalf("missing %s: %v", result.Path, err)
		}
	}

	h.out.Reset()
	if code := h.run("skill", "install", "--all", "--format", "json"); code != 0 {
		t.Fatalf("re-install exit = %d, stderr=%s", code, h.err.String())
	}
	got = skillOutput{}
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, result := range got.Results {
		if result.Action != "unchanged" {
			t.Fatalf("action = %q, want unchanged", result.Action)
		}
	}
}

func TestSkillInstallMultipleHarnessFlags(t *testing.T) {
	h := newHarness(t)
	t.Setenv("HOME", h.dir)

	if code := h.run("skill", "install", "--claude", "--agents", "--format", "json"); code != 0 {
		t.Fatalf("install exit = %d, stderr=%s", code, h.err.String())
	}
	var got skillOutput
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, h.out.String())
	}
	want := []string{
		filepath.Join(h.dir, ".claude", "skills", "ds-tools", "SKILL.md"),
		filepath.Join(h.dir, ".agents", "skills", "ds-tools", "SKILL.md"),
	}
	if len(got.Results) != len(want) {
		t.Fatalf("results = %v, want %v", got.Results, want)
	}
	for i, result := range got.Results {
		if result.Path != want[i] {
			t.Fatalf("results[%d] = %q, want %q", i, result.Path, want[i])
		}
	}
	if _, err := os.Stat(filepath.Join(h.dir, ".pi", "agent", "skills", "ds-tools", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("--claude --agents must not touch .pi: %v", err)
	}
}

func TestSkillUninstallRequiresExplicitTarget(t *testing.T) {
	h := newHarness(t)
	t.Setenv("HOME", h.dir)
	t.Setenv(config.EnvSkill, "")

	if code := h.run("skill", "uninstall"); code != ExitUsage {
		t.Fatalf("uninstall without target exit = %d, want 4", code)
	}
	if !strings.Contains(h.err.String(), "--claude") {
		t.Fatalf("error should mention the target flags: %s", h.err.String())
	}

	if code := h.run("skill", "uninstall", "--all", "--claude"); code != ExitUsage {
		t.Fatalf("--all with --claude exit = %d, want 4", code)
	}
}

func TestSkillUninstallRemovesAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	t.Setenv(config.EnvSkill, dir)

	skillPath := filepath.Join(dir, "ds-tools", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// 旧版本或本地修改过的内容同样直接删除。
	if err := os.WriteFile(skillPath, []byte("stale skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := h.run("skill", "uninstall", "--format", "json"); code != 0 {
		t.Fatalf("uninstall exit = %d, stderr=%s", code, h.err.String())
	}
	var got skillOutput
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, h.out.String())
	}
	if got.Skill != skill.Name {
		t.Fatalf("skill = %q, want %q", got.Skill, skill.Name)
	}
	if len(got.Results) != 1 || got.Results[0].Action != "removed" || got.Results[0].Path != skillPath {
		t.Fatalf("results = %v, want one removed %s", got.Results, skillPath)
	}
	if _, err := os.Stat(skillPath); !os.IsNotExist(err) {
		t.Fatalf("SKILL.md should be gone: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(skillPath)); !os.IsNotExist(err) {
		t.Fatalf("empty ds-tools dir should be pruned: %v", err)
	}

	h.out.Reset()
	if code := h.run("skill", "uninstall", "--format", "text"); code != 0 {
		t.Fatalf("repeat uninstall exit = %d, stderr=%s", code, h.err.String())
	}
	if !strings.Contains(h.out.String(), "未安装") {
		t.Fatalf("repeat uninstall should report 未安装: %s", h.out.String())
	}
}

func TestSkillUninstallAllKeepsForeignFiles(t *testing.T) {
	h := newHarness(t)
	t.Setenv("HOME", h.dir)
	t.Setenv(config.EnvSkill, "")

	if code := h.run("skill", "install", "--all"); code != 0 {
		t.Fatalf("install --all exit = %d, stderr=%s", code, h.err.String())
	}
	claudeDir := filepath.Join(h.dir, ".claude", "skills", "ds-tools")
	foreign := filepath.Join(claudeDir, "notes.md")
	if err := os.WriteFile(foreign, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.out.Reset()
	if code := h.run("skill", "uninstall", "--all", "--format", "json"); code != 0 {
		t.Fatalf("uninstall --all exit = %d, stderr=%s", code, h.err.String())
	}
	var got skillOutput
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, h.out.String())
	}
	if len(got.Results) != 3 {
		t.Fatalf("results = %v, want 3 targets", got.Results)
	}
	for _, result := range got.Results {
		if result.Action != "removed" {
			t.Fatalf("action = %q, want removed", result.Action)
		}
	}
	for _, dir := range []string{
		filepath.Join(h.dir, ".pi", "agent", "skills", "ds-tools"),
		filepath.Join(h.dir, ".agents", "skills", "ds-tools"),
	} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s should be pruned: %v", dir, err)
		}
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("unrelated file must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("SKILL.md should be gone: %v", err)
	}
}

func TestSkillStatusReportsInstallState(t *testing.T) {
	h := newHarness(t)
	t.Setenv("HOME", h.dir)
	write := func(dir, content string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(h.dir, ".claude", "skills", "ds-tools"), skill.Markdown)
	write(filepath.Join(h.dir, ".agents", "skills", "ds-tools"), "stale skill\n")

	if code := h.run("skill", "status", "--format", "json"); code != 0 {
		t.Fatalf("status exit = %d, stderr=%s", code, h.err.String())
	}
	var got skillStatusOutput
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, h.out.String())
	}
	states := map[string]string{}
	for _, target := range got.Targets {
		states[target.Agent] = target.State
	}
	want := map[string]string{"claude": "ok", "pi": "missing", "agents": "changed"}
	for agent, state := range want {
		if states[agent] != state {
			t.Fatalf("%s state = %q, want %q (all: %v)", agent, states[agent], state, states)
		}
	}

	if code := h.run("skill", "status", "--format", "text"); code != 0 {
		t.Fatalf("status text exit = %d", code)
	}
	for _, want := range []string{"ok", "missing", "changed", "install --all", "uninstall --all"} {
		if !strings.Contains(h.out.String(), want) {
			t.Fatalf("status text missing %q:\n%s", want, h.out.String())
		}
	}
}

func TestCanonicalURL(t *testing.T) {
	cases := map[string]string{
		"https://Ex.com/a#x":                "https://ex.com/a",
		"https://ex.com/a?utm_source=t&b=2": "https://ex.com/a?b=2",
		"https://ex.com/":                   "https://ex.com",
		"https://ex.com/a?fbclid=1&gclid=2": "https://ex.com/a",
		"http://ex.com/x?ref=hn&keep=1":     "http://ex.com/x?keep=1",
	}
	for in, want := range cases {
		if got := canonicalURL(in); got != want {
			t.Errorf("canonicalURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseFields(t *testing.T) {
	f, err := parseFields("score,site,url", canonicalFields, fieldAliases)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rerank_score", "site_name", "url"}
	if strings.Join(f, ",") != strings.Join(want, ",") {
		t.Fatalf("fields = %v", f)
	}
	if _, err := parseFields("bogus", canonicalFields, fieldAliases); err == nil {
		t.Fatal("expected an error for an unknown field")
	}
	if f, err := parseFields("", canonicalFields, fieldAliases); err != nil || f != nil {
		t.Fatalf("empty spec should mean all fields: %v %v", f, err)
	}
}

func TestFetchRejectsFieldsItCannotProject(t *testing.T) {
	h := newHarness(t)
	// Fetch only returns url/title/content; search-only fields used to be
	// accepted and silently dropped.
	if code := h.run("fetch", "https://example.com", "--fields", "snippet", "--format", "json"); code != ExitUsage {
		t.Fatalf("search-only field on fetch: exit = %d, stderr=%s", code, h.err.String())
	}
	h.out.Reset()
	h.err.Reset()
	// text/md rendering never reads --fields, so reject instead of ignoring it.
	if code := h.run("fetch", "https://example.com", "--fields", "title", "--format", "text"); code != ExitUsage {
		t.Fatalf("--fields with text format: exit = %d, stderr=%s", code, h.err.String())
	}
}

func TestHelpIsChinese(t *testing.T) {
	h := newHarness(t)
	if code := h.run("--help"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := h.out.String()
	for _, want := range []string{"用法：", "可用命令：", "选项：", "显示此命令的帮助信息"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help 缺少中文标签 %q：\n%s", want, out)
		}
	}
	if strings.Count(out, "  help ") != 1 {
		t.Fatalf("help 命令应只出现一次：\n%s", out)
	}

	// 子命令的帮助里应出现 "全局选项："。
	h.out.Reset()
	if code := h.run("search", "--help"); code != 0 {
		t.Fatalf("search --help exit = %d", code)
	}
	if !strings.Contains(h.out.String(), "全局选项：") {
		t.Fatalf("search --help 缺少「全局选项：」：\n%s", h.out.String())
	}
	for _, english := range []string{"Available Commands:", "Global Flags:", "Usage:", "help for ds-tools", "Generate the autocompletion"} {
		if strings.Contains(out, english) {
			t.Fatalf("help 仍含英文 %q：\n%s", english, out)
		}
	}
	// pflag 的默认值标记也必须中文化
	if strings.Contains(out, "(default ") {
		t.Fatalf("help 仍含 pflag 英文默认值标记：\n%s", out)
	}
	if !strings.Contains(out, "（默认 30s）") {
		t.Fatalf("help 缺少中文默认值标记：\n%s", out)
	}
}

func TestHelpCommandOrderAndGroups(t *testing.T) {
	h := newHarness(t)
	if code := h.run("--help"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := h.out.String()

	order := []string{"fetch", "search", "ocr", "asr", "tts", "init", "config", "skill", "cache", "doctor", "help", "version"}
	prev := -1
	for _, name := range order {
		i := strings.Index(out, "\n  "+name)
		if i < 0 {
			t.Fatalf("help 缺少命令 %q：\n%s", name, out)
		}
		if i <= prev {
			t.Fatalf("命令 %q 顺序错误：\n%s", name, out)
		}
		prev = i
	}
	for _, name := range []string{"init", "cache", "help"} {
		if !strings.Contains(out, "\n\n  "+name) {
			t.Fatalf("命令组 %q 前缺少空行：\n%s", name, out)
		}
	}
	for _, name := range []string{"search", "ocr", "asr", "tts", "config", "skill", "doctor", "version"} {
		if strings.Contains(out, "\n\n  "+name) {
			t.Fatalf("命令 %q 不应另起一组：\n%s", name, out)
		}
	}
}

func TestConfigHelpDropsRedundantCommands(t *testing.T) {
	h := newHarness(t)
	if code := h.run("config", "--help"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := h.out.String()
	for _, gone := range []string{"\n  clear-key ", "\n  path "} {
		if strings.Contains(out, gone) {
			t.Fatalf("config 帮助仍包含 %q：\n%s", gone, out)
		}
	}
	for _, want := range []string{"\n  set ", "\n  show ", "\n  unset "} {
		if !strings.Contains(out, want) {
			t.Fatalf("config 帮助缺少 %q：\n%s", want, out)
		}
	}
}

func TestConfigShowReportsSourcesAndFileValues(t *testing.T) {
	h := newHarness(t)
	if code := h.run("config", "set", "timeout", "45s"); code != 0 {
		t.Fatalf("config set exit = %d, stderr=%s", code, h.err.String())
	}
	if code := h.run("config", "show", "--format", "text"); code != 0 {
		t.Fatalf("config show exit = %d, stderr=%s", code, h.err.String())
	}
	out := h.out.String()
	for _, want := range []string{
		"config_file_exists", "true",
		"config_file_mode", "0600",
		"45s", "source=config", "file=45s",
		"source=env", "file=(未设置)",
		"sk-tes…-key",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("config show 输出缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "sk-test-key") {
		t.Fatalf("config show 泄漏了完整 Key：\n%s", out)
	}
}

func TestConfigShowJSON(t *testing.T) {
	h := newHarness(t)
	if code := h.run("config", "show", "--format", "json"); code != 0 {
		t.Fatalf("config show exit = %d, stderr=%s", code, h.err.String())
	}
	var got map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, h.out.String())
	}
	if got["config_file_exists"] != false {
		t.Fatalf("config_file_exists = %v, want false", got["config_file_exists"])
	}
	if got["config_file_mode"] != nil {
		t.Fatalf("config_file_mode = %v, want null", got["config_file_mode"])
	}
	if got["api_key"] != "sk-tes…-key" {
		t.Fatalf("api_key = %v, want masked", got["api_key"])
	}
	if got["api_key_file"] != nil {
		t.Fatalf("api_key_file = %v, want null", got["api_key_file"])
	}
	if got["timeout_source"] != "default" {
		t.Fatalf("timeout_source = %v, want default", got["timeout_source"])
	}

	if code := h.run("config", "set", "timeout", "45s"); code != 0 {
		t.Fatalf("config set exit = %d", code)
	}
	if code := h.run("config", "show", "--format", "json"); code != 0 {
		t.Fatalf("config show exit = %d", code)
	}
	got = nil
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["config_file_exists"] != true || got["config_file_mode"] != "0600" {
		t.Fatalf("config file state = %v/%v", got["config_file_exists"], got["config_file_mode"])
	}
	if got["timeout"] != "45s" || got["timeout_source"] != "config" || got["timeout_file"] != "45s" {
		t.Fatalf("timeout view = %v/%v/%v", got["timeout"], got["timeout_source"], got["timeout_file"])
	}
}

func TestErrorsAndUnknownCommandAreChinese(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"bogus"}, []string{"未知命令", `"bogus"`}},
		{[]string{"search", "x", "--nope"}, []string{"未知选项", "--nope"}},
		{[]string{"search", "x", "--count", "abc"}, []string{"参数值", `"abc"`}},
		{[]string{"config", "set", "onlyone"}, []string{"需要 2 个参数"}},
		{[]string{"help", "bogus"}, []string{"没有", `"bogus"`}},
	}
	for _, c := range cases {
		h := newHarness(t)
		if code := h.run(c.args...); code != ExitUsage {
			t.Fatalf("%v -> exit %d, want 4 (stderr=%s)", c.args, code, h.err.String())
		}
		for _, want := range c.want {
			if !strings.Contains(h.err.String(), want) {
				t.Fatalf("%v 的错误信息缺少 %q：%s", c.args, want, h.err.String())
			}
		}
	}
}
