package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"dstools/internal/deepseek"
)

type searchOpts struct {
	count        int
	needContent  bool
	limit        int
	fillMissing  bool
	minScore     float64
	minAuthority int
	excludeSites []string
	oriQuestion  string
	localization string
	output       string
}

func (a *App) newSearchCmd() *cobra.Command {
	o := searchOpts{minAuthority: -1}
	cmd := &cobra.Command{
		Use:   "search <query|->",
		Short: "调用 DeepSeek Search 接口搜索网页",
		Long: `搜索网页并返回结构化结果（POST /v1/tools/search）。

无论 --count 和 --content 取什么值都只计一次请求，因此用 --content 连带正文
不会额外收费。搜索无结果是正常成功（退出码 0）。

搜索语法原样透传，可自由组合：
  site:github.com          -site:example.com       "精确短语"         -"排除短语"
  lang:zh,en               after:2025-01-01        before:2026

示例：
  ds-tools search "DeepSeek V4 发布" -n 5 --jsonl --fields title,url,summary
  ds-tools search "某事 进展" -n 3 --content --min-score 1.5 --limit 8000 --format md
  echo "rust async runtime" | ds-tools search -` + "\n\n" + fieldsUsage(canonicalFields, fieldAliases),
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSearch(cmd.Context(), args, o)
		},
	}
	f := cmd.Flags()
	f.IntVarP(&o.count, "count", "n", 10, "返回结果条数（1-50）")
	f.BoolVar(&o.needContent, "content", false, "在同一请求中返回网页正文（need_content）")
	f.IntVar(&o.limit, "limit", 0, "每条结果的 content/summary 截断到 N 字符（0 表示不截断）")
	f.BoolVar(&o.fillMissing, "fill-missing", false, "对正文为空的结果补抓一次（额外计费，需配合 --content）")
	f.Float64Var(&o.minScore, "min-score", 0, "丢弃 rerank_score 低于 N 的结果（0 不过滤，1.5 为优质线）")
	f.IntVar(&o.minAuthority, "min-authority", -1, "丢弃 authority 低于 N 的结果（-1 不过滤，3 为极权威）")
	f.StringArrayVar(&o.excludeSites, "exclude-site", nil, "丢弃该站点的结果（可重复指定）")
	f.StringVar(&o.oriQuestion, "ori-question", "", "触发搜索的用户原始问题，有助于理解搜索意图（1-128 字符）")
	f.StringVar(&o.localization, "localization", "", "用于优先返回本地化结果的 IP 地址")
	f.StringVarP(&o.output, "output", "o", "", "把输出写入文件而不是 stdout")
	_ = cmd.RegisterFlagCompletionFunc("exclude-site", cobra.NoFileCompletions)
	return cmd
}

func (a *App) runSearch(ctx context.Context, args []string, o searchOpts) error {
	query, err := a.readInput(args, "query")
	if err != nil {
		return err
	}
	if query == "" {
		return usageErrf("搜索词不能为空")
	}
	if n := runeLen(query); n > 128 {
		return usageErrf("搜索词长度为 %d 个字符，接口上限为 128", n)
	}
	if o.count < 1 || o.count > 50 {
		return usageErrf("--count 必须在 1 到 50 之间，当前为 %d", o.count)
	}
	if o.limit < 0 {
		return usageErrf("--limit 必须 >= 0")
	}
	if o.fillMissing && !o.needContent {
		return usageErrf("--fill-missing 需要同时指定 --content")
	}
	if o.oriQuestion != "" && runeLen(o.oriQuestion) > 128 {
		return usageErrf("--ori-question 最多 128 个字符")
	}
	if o.minAuthority < -1 || o.minAuthority > 4 {
		return usageErrf("--min-authority 必须在 -1 到 4 之间")
	}

	req := &deepseek.SearchRequest{
		Query:        query,
		Count:        o.count,
		NeedContent:  o.needContent,
		Localization: o.localization,
	}
	if o.oriQuestion != "" {
		req.OriQuestion = &o.oriQuestion
	}

	if a.fDryRun {
		return a.printDryRun("POST", deepseek.PathSearch, req)
	}

	client, err := a.client()
	if err != nil {
		return err
	}

	keyBytes, _ := json.Marshal(req)
	cacheKey := "search|" + string(keyBytes)
	resp := &deepseek.SearchResponse{}
	hit := a.cacheGet(cacheKey, resp)
	if hit {
		a.verbosef("缓存命中：search %q", query)
	} else {
		a.verbosef("POST %s%s (count=%d need_content=%v)", a.baseURL, deepseek.PathSearch, o.count, o.needContent)
		resp, err = client.Search(ctx, req)
		if err != nil {
			return err
		}
		a.cacheSet(cacheKey, resp)
	}

	results := filterResults(resp.Results, o)
	if o.fillMissing && len(results) > 0 {
		results = a.fillMissing(ctx, client, results)
	}

	if len(results) == 0 && !a.fQuiet && (a.format == "text" || a.format == "md") {
		fmt.Fprintln(a.stderr, "没有找到结果")
	}
	return a.emit(o.output, func(w io.Writer) error {
		return a.renderSearch(w, results, o.limit)
	})
}

func filterResults(in []deepseek.SearchResult, o searchOpts) []deepseek.SearchResult {
	out := make([]deepseek.SearchResult, 0, len(in))
	for _, r := range in {
		if o.minScore > 0 && r.RerankScore < o.minScore {
			continue
		}
		if o.minAuthority > 0 && r.Authority < o.minAuthority {
			continue
		}
		if excluded(r.URL, o.excludeSites) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func excluded(rawURL string, hosts []string) bool {
	if len(hosts) == 0 {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "-site:")))
		if h == "" {
			continue
		}
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// fillMissing fetches pages whose content came back empty. Each fetch is a
// separate billed request, which is why this behaviour is opt-in.
func (a *App) fillMissing(ctx context.Context, client *deepseek.Client, results []deepseek.SearchResult) []deepseek.SearchResult {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range results {
		if strings.TrimSpace(results[i].Content) != "" {
			continue
		}
		if results[i].URL == "" {
			continue
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := validateURL(results[idx].URL); err != nil {
				return
			}
			page, err := client.Fetch(ctx, &deepseek.FetchRequest{URL: results[idx].URL})
			if err != nil {
				a.verbosef("补抓正文失败 %s：%v", results[idx].URL, err)
				return
			}
			results[idx].Content = page.Content
			if results[idx].Title == "" {
				results[idx].Title = page.Title
			}
		}(i)
	}
	wg.Wait()
	return results
}
