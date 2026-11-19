package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"dstools/internal/deepseek"
)

type fetchOpts struct {
	fromFile string
	jobs     int
	offset   int
	limit    int
	raw      bool
	dedupe   bool
	noDedupe bool
	output   string
}

func (a *App) newFetchCmd() *cobra.Command {
	o := fetchOpts{jobs: 4, dedupe: true}
	cmd := &cobra.Command{
		Use:   "fetch <url...|->",
		Short: "调用 DeepSeek Fetch 接口把网页抓成 Markdown",
		Long: `抓取一个或多个 URL 并返回其 Markdown 正文（POST /v1/tools/fetch）。

每个 URL 是一次独立计费的请求。账号级限速 25 QPS，目标域名另有 QPS 限制
（超限返回 429），所以 --jobs 不要开太大（默认 4）。

示例：
  ds-tools fetch https://example.com/post --raw
  ds-tools fetch -f urls.txt --jobs 4 -o pages/          # 每个 URL 一个 .md
  ds-tools fetch https://long.doc --offset 8000 --limit 8000   # 分段读取长文

某个 URL 失败不会中断其余 URL；多 URL 时每行输出一个 JSON 对象。
输入里混有非法 URL 时，只要还有合法 URL 被请求（并可能已计费），退出码按
请求错误（1）计算；只有全部 URL 都非法才是用法错误（4）。

每个 URL 的结果都会本地缓存（默认 1 小时），重复运行同一条命令不再计费；
--no-cache 跳过读取缓存、强制重新抓取（抓到的结果仍会写入缓存）。

` + fieldsUsage(fetchFields, nil),
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runFetch(cmd.Context(), args, o)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.fromFile, "from-file", "f", "", "从文件读取 URL，每行一个")
	f.IntVar(&o.jobs, "jobs", 4, "并发请求数（建议 <= 4，账号限速 25 QPS）")
	f.IntVar(&o.offset, "offset", 0, "正文起始字符偏移")
	f.IntVar(&o.limit, "limit", 0, "正文截断到 N 字符（0 表示不截断）")
	f.BoolVar(&o.raw, "raw", false, "只输出 Markdown 正文（仅限单个 URL）")
	f.BoolVar(&o.dedupe, "dedupe", true, "URL 规范化后去重")
	f.BoolVar(&o.noDedupe, "no-dedupe", false, "关闭 URL 去重")
	f.StringVarP(&o.output, "output", "o", "", "写入文件；多个 URL 时写入目录")
	_ = cmd.MarkFlagFilename("from-file")
	_ = cmd.MarkFlagFilename("output")
	return cmd
}

func (a *App) runFetch(ctx context.Context, args []string, o fetchOpts) error {
	if len(a.fields) > 0 && a.format != "json" && a.format != "jsonl" {
		return usageErrf("--fields 仅支持 --format json 或 jsonl")
	}
	if o.jobs < 1 {
		return usageErrf("--jobs 必须 >= 1")
	}
	if o.offset < 0 || o.limit < 0 {
		return usageErrf("--offset 与 --limit 必须 >= 0")
	}
	if o.noDedupe {
		o.dedupe = false
	}

	rawURLs, err := a.collectURLs(args, o.fromFile)
	if err != nil {
		return err
	}
	if len(rawURLs) == 0 {
		return usageErrf("没有提供 URL（作为参数传入、用 '-' 从 stdin 读取，或用 -f urls.txt）")
	}

	// plan keeps the input order so results (and -o file numbering) follow it.
	type planItem struct {
		input   string
		invalid error // non-nil when the URL can never be fetched
	}
	plan := make([]planItem, 0, len(rawURLs))
	seen := map[string]bool{}
	valid := 0
	for _, raw := range rawURLs {
		if err := validateURL(raw); err != nil {
			plan = append(plan, planItem{input: raw, invalid: err})
			continue
		}
		// canonicalURL is only a de-duplication / cache key; the API request
		// carries the URL exactly as the user gave it (whitespace already trimmed).
		canon := canonicalURL(raw)
		if o.dedupe && seen[canon] {
			continue
		}
		seen[canon] = true
		plan = append(plan, planItem{input: raw})
		valid++
	}

	if len(plan) == 0 {
		return usageErrf("去重后没有可抓取的 URL")
	}
	if o.raw {
		if len(plan) != 1 {
			return usageErrf("--raw 只支持单个 URL")
		}
		// --raw is an explicit request for the bare Markdown body.
		a.format = "md"
	}

	if a.fDryRun {
		if valid == 0 {
			return usageErrf("没有合法的 URL 可抓取")
		}
		for _, p := range plan {
			if p.invalid != nil {
				continue
			}
			if err := a.printDryRun("POST", deepseek.PathFetch, &deepseek.FetchRequest{URL: p.input}); err != nil {
				return err
			}
		}
		return nil
	}

	var client *deepseek.Client
	if valid > 0 {
		client, err = a.client()
		if err != nil {
			return err
		}
	}

	results := make([]fetchResult, len(plan))
	for i, p := range plan {
		if p.invalid == nil {
			continue
		}
		code := codeOf(p.invalid)
		// An invalid URL is a local usage error only when the run has nothing
		// else to do: once valid URLs are requested, some of them are billed and
		// printed, so exit 4 ("nothing was sent") would be a lie.
		if valid > 0 {
			code = ExitServer
		}
		results[i] = fetchResult{URL: p.input, Err: p.invalid.Error(), code: code}
	}

	sem := make(chan struct{}, o.jobs)
	var wg sync.WaitGroup
	for i, p := range plan {
		if p.invalid != nil {
			continue
		}
		wg.Add(1)
		go func(idx int, rawURL string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := fetchResult{URL: rawURL}
			page, _, fErr := a.fetchPage(ctx, client, rawURL)
			if fErr != nil {
				res.Err, res.code = fErr.Error(), codeOf(fErr)
				results[idx] = res
				return
			}
			res.Title = page.Title
			res.Content = page.Content
			results[idx] = res
		}(i, p.input)
	}
	wg.Wait()

	results = applyWindow(results, o.offset, o.limit)

	// A directory target means "one file per page".
	if o.output != "" && len(results) > 1 {
		if err := a.writeFetchFiles(o.output, results); err != nil {
			return err
		}
		return a.worstFetchError(results)
	}

	if err := a.emit(o.output, func(w io.Writer) error {
		return a.renderFetch(w, results)
	}); err != nil {
		return err
	}
	return a.worstFetchError(results)
}

// fetchPage returns rawURL's page through the on-disk fetch cache shared with
// search --fill-missing. cached reports a cache hit, so callers can tell a
// repeat run from a billed one.
func (a *App) fetchPage(ctx context.Context, client *deepseek.Client, rawURL string) (page *deepseek.FetchResponse, cached bool, err error) {
	key := "fetch|" + canonicalURL(rawURL)
	page = &deepseek.FetchResponse{}
	if a.cacheGet(key, page) {
		a.verbosef("缓存命中：%s", rawURL)
		return page, true, nil
	}
	a.verbosef("POST %s%s url=%s", a.baseURL, deepseek.PathFetch, rawURL)
	page, err = client.Fetch(ctx, &deepseek.FetchRequest{URL: rawURL})
	if err != nil {
		return nil, false, err
	}
	a.cacheSet(key, page)
	return page, false, nil
}

// fetchErrRank orders failures by severity rather than numeric code:
// auth beats server/request, which beats transient, which beats usage.
func fetchErrRank(code int) int {
	switch code {
	case ExitAuth:
		return 4
	case ExitServer:
		return 3
	case ExitNetwork:
		return 2
	case ExitUsage:
		return 1
	}
	return 0
}

// worstFetchError maps per-URL failures onto the exit-code taxonomy.
func (a *App) worstFetchError(results []fetchResult) error {
	worst, rank := ExitOK, 0
	var msg string
	for _, r := range results {
		if r.Err == "" {
			continue
		}
		code := r.code
		if code == ExitOK {
			code = ExitServer
		}
		if cr := fetchErrRank(code); cr > rank {
			worst, rank, msg = code, cr, r.Err
		}
	}
	if worst == ExitOK {
		return nil
	}
	return withCode(worst, fmt.Errorf("%s", msg))
}

func (a *App) collectURLs(args []string, fromFile string) ([]string, error) {
	var out []string
	for _, raw := range args {
		if strings.TrimSpace(raw) == "-" {
			continue
		}
		if v := strings.TrimSpace(raw); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 && fromFile == "" {
		if !a.stdinPiped {
			return out, nil
		}
		b, err := io.ReadAll(a.stdin)
		if err != nil {
			return nil, withCode(ExitUsage, fmt.Errorf("读取 stdin 失败：%w", err))
		}
		for _, line := range strings.Split(string(b), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				out = append(out, line)
			}
		}
		return out, nil
	}
	if len(out) == 0 && fromFile != "" {
		b, err := os.ReadFile(fromFile)
		if err != nil {
			return nil, withCode(ExitUsage, fmt.Errorf("读取 %s 失败：%w", fromFile, err))
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			out = append(out, line)
		}
	}
	return out, nil
}

// applyWindow slices content by rune offset/limit.
func applyWindow(results []fetchResult, offset, limit int) []fetchResult {
	if offset == 0 && limit == 0 {
		return results
	}
	for i := range results {
		if results[i].Err != "" {
			continue
		}
		runes := []rune(results[i].Content)
		if offset >= len(runes) {
			results[i].Content = ""
			continue
		}
		runes = runes[offset:]
		if limit > 0 && len(runes) > limit {
			runes = runes[:limit]
		}
		results[i].Content = string(runes)
	}
	return results
}

func (a *App) writeFetchFiles(dir string, results []fetchResult) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return withCode(ExitUsage, fmt.Errorf("创建目录 %s 失败：%w", dir, err))
	}
	written := 0
	for i, r := range results {
		if r.Err != "" {
			if !a.fQuiet {
				fmt.Fprintf(a.stderr, "错误：%s：%s\n", r.URL, r.Err)
			}
			continue
		}
		name := fmt.Sprintf("%02d-%s.md", i+1, slugify(r.URL))
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(r.Content), 0o644); err != nil {
			return withCode(ExitUsage, fmt.Errorf("写入 %s 失败：%w", path, err))
		}
		written++
		a.verbosef("已写入 %s", path)
	}
	if a.format == "json" || a.format == "jsonl" {
		payload := struct {
			Dir     string   `json:"dir"`
			Written int      `json:"written"`
			Files   []string `json:"files"`
		}{Dir: dir, Written: written}
		for i, r := range results {
			if r.Err == "" {
				payload.Files = append(payload.Files, fmt.Sprintf("%02d-%s.md", i+1, slugify(r.URL)))
			}
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(a.stdout, "%s\n", b)
		return nil
	}
	if !a.fQuiet {
		fmt.Fprintf(a.stderr, "已写入 %d 个文件到 %s\n", written, dir)
	}
	return nil
}
