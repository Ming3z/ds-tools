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

每个 URL 都是一次独立计费的请求，且该接口对账号和每个目标域名都限制 25 QPS，
所以 --jobs 不要开太大（默认 4）。

示例：
  ds-tools fetch https://example.com/post --raw
  ds-tools fetch -f urls.txt --jobs 4 -o pages/          # 每个 URL 一个 .md
  ds-tools fetch https://long.doc --offset 8000 --limit 8000   # 分段读取长文

某个 URL 失败不会中断其余 URL；多 URL 时每行输出一个 JSON 对象。

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

	type target struct{ input, canonical string }
	var targets []target
	seen := map[string]bool{}
	var invalid []fetchResult
	for _, raw := range rawURLs {
		if err := validateURL(raw); err != nil {
			invalid = append(invalid, fetchResult{
				InputURL: raw,
				URL:      raw,
				Err:      err.Error(),
				code:     codeOf(err),
			})
			continue
		}
		canon := canonicalURL(raw)
		if o.dedupe && seen[canon] {
			continue
		}
		seen[canon] = true
		targets = append(targets, target{input: raw, canonical: canon})
	}

	if len(targets) == 0 && len(invalid) == 0 {
		return usageErrf("去重后没有可抓取的 URL")
	}
	if o.raw {
		if len(targets)+len(invalid) != 1 {
			return usageErrf("--raw 只支持单个 URL")
		}
		// --raw is an explicit request for the bare Markdown body.
		a.format = "md"
	}

	if a.fDryRun {
		if len(targets) == 0 {
			return usageErrf("没有合法的 URL 可抓取")
		}
		for _, t := range targets {
			if err := a.printDryRun("POST", deepseek.PathFetch, &deepseek.FetchRequest{URL: t.canonical}); err != nil {
				return err
			}
		}
		return nil
	}

	var client *deepseek.Client
	if len(targets) > 0 {
		client, err = a.client()
		if err != nil {
			return err
		}
	}

	results := make([]fetchResult, len(targets))
	sem := make(chan struct{}, o.jobs)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(idx int, t target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := fetchResult{InputURL: t.input, URL: t.canonical}
			key := "fetch|" + t.canonical
			page := &deepseek.FetchResponse{}
			if a.cacheGet(key, page) {
				a.verbosef("缓存命中：%s", t.canonical)
			} else {
				a.verbosef("POST %s%s url=%s", a.baseURL, deepseek.PathFetch, t.canonical)
				got, fErr := client.Fetch(ctx, &deepseek.FetchRequest{URL: t.canonical})
				if fErr != nil {
					res.Err, res.code = fErr.Error(), codeOf(fErr)
					results[idx] = res
					return
				}
				page = got
				a.cacheSet(key, page)
			}
			res.Title = page.Title
			res.Content = page.Content
			results[idx] = res
		}(i, t)
	}
	wg.Wait()

	results = append(results, invalid...)
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

// worstFetchError maps per-URL failures onto the exit-code taxonomy.
func (a *App) worstFetchError(results []fetchResult) error {
	worst := ExitOK
	var msg string
	for _, r := range results {
		if r.Err == "" {
			continue
		}
		code := r.code
		if code == ExitOK {
			code = ExitServer
		}
		if code > worst {
			worst, msg = code, r.Err
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
