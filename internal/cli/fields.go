package cli

import (
	"sort"
	"strings"
)

// canonicalFields is the official search response schema, in API order.
var canonicalFields = []string{
	"title", "url", "snippet", "summary", "content",
	"site_name", "site_icon", "date", "rerank_score", "authority",
}

var fieldAliases = map[string]string{
	"score": "rerank_score",
	"site":  "site_name",
	"icon":  "site_icon",
}

// fetchFields are the only fields a fetch result carries. Exposing the search
// schema here would accept flags that the renderer silently drops.
var fetchFields = []string{"url", "title", "content"}

// Fields accepted by the tool commands. OCR uses one set for the document-level
// JSON response and another for one-page-per-line JSONL.
var (
	ocrJSONFields = []string{"id", "object", "created", "purpose", "page_count", "pages", "usage", "text"}
	ocrPageFields = []string{"index", "text", "finish_reason"}
	ocrAllFields  = []string{
		"id", "object", "created", "purpose", "page_count", "pages", "usage", "text",
		"index", "finish_reason",
	}
	asrFields = []string{"request_id", "text", "duration_ms"}
)

// fieldsForCommand returns the fields permitted while parsing the global
// --fields flag for a command. Search/fetch keep the search schema.
func fieldsForCommand(name string) ([]string, map[string]string) {
	switch name {
	case "fetch":
		return fetchFields, nil
	case "ocr":
		return ocrAllFields, nil
	case "asr":
		return asrFields, nil
	case "tts":
		return nil, nil
	default:
		return canonicalFields, fieldAliases
	}
}

func parseFields(spec string, allowed []string, aliases map[string]string) ([]string, error) {
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
		if real, ok := aliases[f]; ok {
			f = real
		}
		if !contains(allowed, f) {
			return nil, usageErrf("--fields 中的 %q 无效（可选：%s）", f, strings.Join(allowed, "、"))
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

// fieldsUsage renders the --fields contract for one command's help text. It
// derives from the same lists that validate the flag, so help cannot drift
// away from what the command actually accepts.
func fieldsUsage(allowed []string, aliases map[string]string) string {
	var b strings.Builder
	b.WriteString("可用 --fields：")
	b.WriteString(strings.Join(allowed, " "))
	b.WriteString("。")
	if len(aliases) > 0 {
		keys := make([]string, 0, len(aliases))
		for k := range aliases {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"→"+aliases[k])
		}
		b.WriteString("\n别名：" + strings.Join(parts, "、") + "。")
	}
	return b.String()
}

// ocrFieldsUsage documents OCR's two field sets, which depend on --format.
func ocrFieldsUsage() string {
	return "可用 --fields（--format json）：" + strings.Join(ocrJSONFields, " ") + "；\n" +
		"（--format jsonl）：" + strings.Join(ocrPageFields, " ") + "。"
}

func contains(allowed []string, f string) bool {
	for _, a := range allowed {
		if a == f {
			return true
		}
	}
	return false
}

// requireFields rejects fields that are not meaningful for the selected output
// format, before any request is sent.
func requireFields(fields, allowed []string) error {
	for _, f := range fields {
		if !contains(allowed, f) {
			return usageErrf("--fields 中的 %q 不适用于当前格式（可选：%s）", f, strings.Join(allowed, "、"))
		}
	}
	return nil
}
