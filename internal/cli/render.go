package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"dstools/internal/deepseek"
)

// canonicalFields is the official response schema, in API order.
var canonicalFields = []string{
	"title", "url", "snippet", "summary", "content",
	"site_name", "site_icon", "date", "rerank_score", "authority",
}

var fieldAliases = map[string]string{
	"score": "rerank_score",
	"site":  "site_name",
	"icon":  "site_icon",
}

func parseFields(spec string) ([]string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, raw := range strings.Split(spec, ",") {
		f := strings.ToLower(strings.TrimSpace(raw))
		if f == "" {
			continue
		}
		if real, ok := fieldAliases[f]; ok {
			f = real
		}
		if !knownField(f) {
			return nil, usageErrf("--fields 中的 %q 无效（可选：%s）", f, strings.Join(canonicalFields, "、"))
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, usageErrf("--fields 至少要指定一个字段")
	}
	return out, nil
}

func knownField(f string) bool {
	for _, c := range canonicalFields {
		if c == f {
			return true
		}
	}
	return false
}

// kv / obj implement order-preserving JSON objects so that --fields projections
// keep the requested field order instead of Go's alphabetical map order.
type kv struct {
	K string
	V any
}

type obj []kv

func (o obj) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, p := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := json.Marshal(p.K)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		v, err := json.Marshal(p.V)
		if err != nil {
			return nil, err
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func (a *App) selectedFields() []string {
	if len(a.fields) > 0 {
		return a.fields
	}
	return canonicalFields
}

func hasField(fields []string, f string) bool {
	for _, x := range fields {
		if x == f {
			return true
		}
	}
	return false
}

// truncate cuts s to at most limit runes (0 = unlimited).
func truncate(s string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit])
}

func (a *App) bold(s string) string {
	if !a.color {
		return s
	}
	return "\033[1m" + s + "\033[0m"
}

func (a *App) dim(s string) string {
	if !a.color {
		return s
	}
	return "\033[2m" + s + "\033[0m"
}

func shortDate(raw string) string {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	if len(raw) >= 10 {
		return raw[:10]
	}
	return raw
}

func deref(p *string) any {
	if p == nil {
		return nil
	}
	if *p == "" {
		return nil
	}
	return *p
}

// searchFieldValue returns the projected value of one field.
func searchFieldValue(r deepseek.SearchResult, field string, limit int) any {
	switch field {
	case "title":
		return r.Title
	case "url":
		return r.URL
	case "snippet":
		return truncate(r.Snippet, limit)
	case "summary":
		return truncate(r.Summary, limit)
	case "content":
		return truncate(r.Content, limit)
	case "site_name":
		return deref(r.SiteName)
	case "site_icon":
		return deref(r.SiteIcon)
	case "date":
		return deref(r.Date)
	case "rerank_score":
		return r.RerankScore
	case "authority":
		return r.Authority
	}
	return nil
}

func (a *App) resultObj(r deepseek.SearchResult, limit int) obj {
	var o obj
	for _, f := range a.selectedFields() {
		o = append(o, kv{f, searchFieldValue(r, f, limit)})
	}
	return o
}

// renderSearch writes the search response in the selected format.
func (a *App) renderSearch(w io.Writer, results []deepseek.SearchResult, limit int) error {
	fields := a.selectedFields()
	switch a.format {
	case "json":
		if _, err := io.WriteString(w, `{"results":[`); err != nil {
			return err
		}
		for i, r := range results {
			if i > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			b, err := json.Marshal(a.resultObj(r, limit))
			if err != nil {
				return err
			}
			if _, err := w.Write(b); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, "]}\n")
		return err
	case "jsonl":
		for _, r := range results {
			b, err := json.Marshal(a.resultObj(r, limit))
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "%s\n", b); err != nil {
				return err
			}
		}
		return nil
	case "md":
		return a.renderSearchMD(w, results, fields, limit)
	default:
		return a.renderSearchText(w, results, fields, limit)
	}
}

func (a *App) renderSearchText(w io.Writer, results []deepseek.SearchResult, fields []string, limit int) error {
	for i, r := range results {
		title := r.Title
		if title == "" {
			title = r.URL
		}
		line := fmt.Sprintf("%d. ", i+1)
		if hasField(fields, "title") {
			line += a.bold(title)
		}
		var meta []string
		if hasField(fields, "rerank_score") {
			meta = append(meta, fmt.Sprintf("score %.2f", r.RerankScore))
		}
		if hasField(fields, "authority") && r.Authority >= 0 {
			meta = append(meta, fmt.Sprintf("authority %d", r.Authority))
		}
		if hasField(fields, "site_name") && r.SiteName != nil && *r.SiteName != "" {
			meta = append(meta, *r.SiteName)
		}
		if hasField(fields, "date") && r.Date != nil && *r.Date != "" {
			meta = append(meta, shortDate(*r.Date))
		}
		if len(meta) > 0 {
			line += "  " + a.dim("["+strings.Join(meta, " · ")+"]")
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		if hasField(fields, "url") && r.URL != "" {
			if _, err := fmt.Fprintf(w, "   %s\n", a.dim(r.URL)); err != nil {
				return err
			}
		}
		// Exactly one body per result, best available first: content > summary > snippet.
		for _, f := range []string{"content", "summary", "snippet"} {
			if !hasField(fields, f) {
				continue
			}
			body := truncate(searchFieldString(r, f), limit)
			if strings.TrimSpace(body) == "" {
				continue
			}
			if _, err := fmt.Fprintf(w, "%s\n", indent(strings.TrimRight(body, "\n"), "   ")); err != nil {
				return err
			}
			break
		}
		if i < len(results)-1 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
	}
	return nil
}

func searchFieldString(r deepseek.SearchResult, field string) string {
	switch field {
	case "snippet":
		return r.Snippet
	case "summary":
		return r.Summary
	case "content":
		return r.Content
	}
	return ""
}

func (a *App) renderSearchMD(w io.Writer, results []deepseek.SearchResult, fields []string, limit int) error {
	for i, r := range results {
		if hasField(fields, "title") {
			title := r.Title
			if title == "" {
				title = r.URL
			}
			if _, err := fmt.Fprintf(w, "## %s\n\n", title); err != nil {
				return err
			}
		}
		if hasField(fields, "url") && r.URL != "" {
			if _, err := fmt.Fprintf(w, "<%s>\n\n", r.URL); err != nil {
				return err
			}
		}
		var meta []string
		if hasField(fields, "rerank_score") {
			meta = append(meta, fmt.Sprintf("score %.2f", r.RerankScore))
		}
		if hasField(fields, "authority") && r.Authority >= 0 {
			meta = append(meta, fmt.Sprintf("authority %d", r.Authority))
		}
		if hasField(fields, "date") && r.Date != nil && *r.Date != "" {
			meta = append(meta, shortDate(*r.Date))
		}
		if hasField(fields, "site_name") && r.SiteName != nil && *r.SiteName != "" {
			meta = append(meta, *r.SiteName)
		}
		if len(meta) > 0 {
			if _, err := fmt.Fprintf(w, "> %s\n\n", strings.Join(meta, " · ")); err != nil {
				return err
			}
		}
		for _, f := range []string{"content", "summary", "snippet"} {
			if !hasField(fields, f) {
				continue
			}
			body := truncate(searchFieldString(r, f), limit)
			if strings.TrimSpace(body) == "" {
				continue
			}
			if _, err := fmt.Fprintf(w, "%s\n\n", strings.TrimRight(body, "\n")); err != nil {
				return err
			}
			break
		}
		if i < len(results)-1 {
			if _, err := io.WriteString(w, "---\n\n"); err != nil {
				return err
			}
		}
	}
	return nil
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}

// fetchResult is one URL outcome of a fetch run.
type fetchResult struct {
	InputURL string
	URL      string
	Title    string
	Content  string
	Err      string
	// code is the exit code this failure maps to (0 when there is no error).
	code int
}

func (a *App) fetchObj(r fetchResult) obj {
	fields := a.fields
	if len(fields) == 0 {
		fields = []string{"url", "title", "content"}
	}
	// Only url/title/content are meaningful for fetch; ignore the rest.
	o := obj{{K: "url", V: r.URL}}
	if hasField(fields, "title") {
		o = append(o, kv{"title", r.Title})
	}
	if r.Err != "" {
		o = append(o, kv{"error", r.Err})
		return o
	}
	if hasField(fields, "content") {
		o = append(o, kv{"content", r.Content})
	}
	if r.InputURL != "" && r.InputURL != r.URL {
		o = append(o, kv{"input_url", r.InputURL})
	}
	return o
}

// renderFetch writes fetch results in the selected format.
func (a *App) renderFetch(w io.Writer, results []fetchResult) error {
	single := len(results) == 1
	switch a.format {
	case "json":
		if single {
			b, err := json.Marshal(a.fetchObj(results[0]))
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(w, "%s\n", b)
			return err
		}
		if _, err := io.WriteString(w, `{"results":[`); err != nil {
			return err
		}
		for i, r := range results {
			if i > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			b, err := json.Marshal(a.fetchObj(r))
			if err != nil {
				return err
			}
			if _, err := w.Write(b); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, "]}\n")
		return err
	case "jsonl":
		for _, r := range results {
			b, err := json.Marshal(a.fetchObj(r))
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "%s\n", b); err != nil {
				return err
			}
		}
		return nil
	case "md":
		if single && results[0].Err == "" {
			_, err := fmt.Fprintf(w, "%s\n", strings.TrimRight(results[0].Content, "\n"))
			return err
		}
		for i, r := range results {
			if i > 0 {
				if _, err := io.WriteString(w, "\n---\n\n"); err != nil {
					return err
				}
			}
			title := r.Title
			if title == "" {
				title = r.URL
			}
			if _, err := fmt.Fprintf(w, "# %s\n\n<%s>\n\n", title, r.URL); err != nil {
				return err
			}
			if r.Err != "" {
				if _, err := fmt.Fprintf(w, "error: %s\n", r.Err); err != nil {
					return err
				}
				continue
			}
			if _, err := fmt.Fprintf(w, "%s\n", strings.TrimRight(r.Content, "\n")); err != nil {
				return err
			}
		}
		return nil
	default:
		for i, r := range results {
			if i > 0 {
				if _, err := fmt.Fprintln(w); err != nil {
					return err
				}
			}
			head := r.URL
			if r.InputURL != "" && r.InputURL != r.URL {
				head = r.InputURL + " -> " + r.URL
			}
			if _, err := fmt.Fprintf(w, "==> %s\n", a.dim(head)); err != nil {
				return err
			}
			if r.Err != "" {
				if _, err := fmt.Fprintf(w, "   error: %s\n", r.Err); err != nil {
					return err
				}
				continue
			}
			if r.Title != "" {
				if _, err := fmt.Fprintf(w, "%s\n\n", a.bold(r.Title)); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(w, "%s\n", strings.TrimRight(r.Content, "\n")); err != nil {
				return err
			}
		}
		return nil
	}
}

// canonicalURL normalises a URL for de-duplication.
func canonicalURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	q := u.Query()
	for k := range q {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "utm_") || lk == "gclid" || lk == "fbclid" || lk == "spm" || lk == "ref" || lk == "ref_src" {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	if u.Path == "/" {
		u.Path = ""
	}
	return u.String()
}

// validateURL enforces the documented fetch constraints without spending a request.
func validateURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return usageErrf("URL 为空")
	}
	if len(raw) > 8192 {
		return usageErrf("URL 超过 8192 字符：%s", raw[:64]+"…")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return usageErrf("URL %q 无效：%v", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return usageErrf("URL 必须使用 http 或 https：%q", raw)
	}
	if u.Host == "" {
		return usageErrf("URL 必须包含主机名：%q", raw)
	}
	if u.User != nil {
		return usageErrf("URL 不能携带用户名或密码：%q", raw)
	}
	return nil
}

func slugify(u string) string {
	parsed, err := url.Parse(u)
	s := u
	if err == nil {
		s = parsed.Host + parsed.Path
	}
	s = strings.TrimSuffix(s, "/")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if len(out) > 80 {
		out = out[:80]
	}
	if out == "" {
		out = "page"
	}
	return out
}
