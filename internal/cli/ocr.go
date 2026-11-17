package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"dstools/internal/deepseek"
)

type ocrOpts struct {
	purpose  string
	page     string
	filename string
	output   string
}

func (a *App) newOCRCmd() *cobra.Command {
	o := ocrOpts{purpose: "text_recognition", filename: "stdin"}
	cmd := &cobra.Command{
		Use:   "ocr <file|->",
		Short: "调用 DeepSeek OCR 接口识别文档或图片中的文字",
		Long: `识别 PDF、Word、PowerPoint 文档或图片中的文字（POST /v1/tools/ocr）。

文件以 multipart 上传，服务端按页返回。未显式指定 --format 时默认 text，
便于直接重定向成 .txt；需要结构化结果时显式使用 --format json / jsonl。

--purpose text_recognition（默认）输出纯文本：不含图片的 Word/PPT 直接提取文字，
PDF 与图片逐页识别。rich_layout_recognition 对所有文件逐页识别，并在文本中保留
<｜ref｜>/<｜box｜> 版面标记与坐标（[[x1,y1,x2,y2]]，按页面宽高归一化到 0–1000），
text/md 输出也原样保留。
--page 在本地按 1-based 页码过滤，支持 1-3,5、5-、-3。

限制：文件 <= 100 MiB、<= 1000 页；上传与识别总时长由服务端限制为 180 秒。
--timeout 未指定时取配置值与 210 秒的较大者（配置不会把它压低）。
上传完成后的断连、HTTP 504 与识别超时不会自动重试（服务端可能仍在识别并计费）。
计费：每 1000 个成功识别的页面 1.5 元；失败或超时不计费。

示例：
  ds-tools ocr scan.pdf
  ds-tools ocr report.docx --purpose rich_layout_recognition --format md -o report.md
  ds-tools ocr scan.pdf --page 1-3 --format jsonl
  cat scan.png | ds-tools ocr - --filename scan.png

` + ocrFieldsUsage(),
		Args: exactPositionalArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runOCR(cmd.Context(), args[0], o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.purpose, "purpose", "text_recognition", "识别模式：text_recognition|rich_layout_recognition")
	f.StringVar(&o.page, "page", "", "只输出指定页（1-based，如 1-3,5；默认全部）")
	f.StringVar(&o.filename, "filename", "stdin", "从 stdin 读取时使用的文件名")
	f.StringVarP(&o.output, "output", "o", "", "把输出写入文件而不是 stdout")
	_ = cmd.RegisterFlagCompletionFunc("purpose", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"text_recognition", "rich_layout_recognition"}, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.MarkFlagFilename("output")
	return cmd
}

func (a *App) runOCR(ctx context.Context, path string, o ocrOpts) error {
	if !a.formatSet {
		a.format = "text"
	}
	a.timeout = a.toolTimeout(210 * time.Second)
	switch o.purpose {
	case "text_recognition", "rich_layout_recognition":
	default:
		return usageErrf("--purpose 只能是 text_recognition 或 rich_layout_recognition")
	}
	ranges, err := parsePageSpec(o.page)
	if err != nil {
		return err
	}
	if a.format == "text" || a.format == "md" {
		if len(a.fields) > 0 {
			return usageErrf("--fields 仅支持 --format json 或 jsonl")
		}
	} else {
		allowed := ocrJSONFields
		if a.format == "jsonl" {
			allowed = ocrPageFields
		}
		if err := requireFields(a.fields, allowed); err != nil {
			return err
		}
	}

	if a.fDryRun {
		name, size, err := a.statUpload(path, o.filename, 100<<20)
		if err != nil {
			return err
		}
		return a.printDryRunMultipart(deepseek.PathOCR, [][2]string{{"purpose", o.purpose}}, name, size)
	}

	client, err := a.client()
	if err != nil {
		return err
	}

	up, err := a.openUpload(path, o.filename, 100<<20)
	if err != nil {
		return err
	}
	defer up.Close()

	key := "ocr|" + o.purpose + "|" + up.Hash
	resp := &deepseek.OCRResponse{}
	if a.cacheGet(key, resp) {
		a.verbosef("缓存命中：ocr %s", up.Name)
	} else {
		a.verbosef("POST %s%s purpose=%s file=%s（%s）", a.baseURL, deepseek.PathOCR, o.purpose, up.Name, humanBytes(up.Size))
		resp, err = client.OCR(ctx, deepseek.Upload{Name: up.Name, Size: up.Size, Open: up.Open}, o.purpose)
		if err != nil {
			return err
		}
		// A flaky provider can return empty text for a page that is not blank.
		// Caching that would keep serving "" until the TTL expires, so an
		// all-blank response is left out of the cache. A response with at least
		// one non-blank page is cached whole: re-requesting would re-bill every
		// page just because one of them is legitimately blank.
		if ocrHasText(resp.Pages) {
			a.cacheSet(key, resp)
		} else {
			a.verbosef("OCR 结果全部为空，未写入缓存")
		}
	}

	pages := selectOCRPages(resp.Pages, ranges)
	if len(ranges) > 0 && len(pages) == 0 && !a.fQuiet {
		fmt.Fprintf(a.stderr, "警告：--page 未匹配到任何页（共 %d 页）\n", len(resp.Pages))
	}
	for _, p := range pages {
		if p.FinishReason == "length" && !a.fQuiet {
			fmt.Fprintf(a.stderr, "警告：第 %d 页输出被截断（finish_reason=length）\n", p.Index+1)
		}
		if strings.TrimSpace(p.Text) == "" && !a.fQuiet {
			fmt.Fprintf(a.stderr, "警告：第 %d 页识别结果为空；若不是空白页，可加 --no-cache 重试一次\n", p.Index+1)
		}
	}
	return a.emit(o.output, func(w io.Writer) error {
		return a.renderOCR(w, resp, pages)
	})
}

type pageRange struct {
	start int
	end   int // 0 means open-ended
}

func parsePageSpec(spec string) ([]pageRange, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var out []pageRange
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, usageErrf("--page 格式无效：%q", spec)
		}
		if i := strings.Index(part, "-"); i >= 0 {
			left := strings.TrimSpace(part[:i])
			right := strings.TrimSpace(part[i+1:])
			if left == "" && right == "" {
				return nil, usageErrf("--page 范围无效：%q", part)
			}
			start, end := 1, 0
			if left != "" {
				n, err := strconv.Atoi(left)
				if err != nil || n < 1 {
					return nil, usageErrf("--page 页码无效：%q", part)
				}
				start = n
			}
			if right != "" {
				n, err := strconv.Atoi(right)
				if err != nil || n < 1 {
					return nil, usageErrf("--page 页码无效：%q", part)
				}
				end = n
			}
			if end != 0 && end < start {
				return nil, usageErrf("--page 范围无效：%q", part)
			}
			out = append(out, pageRange{start: start, end: end})
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 {
			return nil, usageErrf("--page 页码无效：%q", part)
		}
		out = append(out, pageRange{start: n, end: n})
	}
	return out, nil
}

func selectOCRPages(pages []deepseek.OCRPage, ranges []pageRange) []deepseek.OCRPage {
	if len(ranges) == 0 {
		return pages
	}
	n := len(pages)
	selected := make([]bool, n)
	for _, r := range ranges {
		start := r.start
		end := r.end
		if start < 1 {
			start = 1
		}
		if end == 0 || end > n {
			end = n
		}
		for i := start; i <= end && i <= n; i++ {
			selected[i-1] = true
		}
	}
	out := make([]deepseek.OCRPage, 0, n)
	for i, p := range pages {
		if selected[i] {
			out = append(out, p)
		}
	}
	return out
}

// ocrHasText reports whether any page carries non-blank text. An all-blank
// response is treated as unreliable rather than as a real empty document.
func ocrHasText(pages []deepseek.OCRPage) bool {
	for _, p := range pages {
		if strings.TrimSpace(p.Text) != "" {
			return true
		}
	}
	return false
}

func joinOCRPages(pages []deepseek.OCRPage) string {
	parts := make([]string, 0, len(pages))
	for _, p := range pages {
		parts = append(parts, strings.TrimRight(p.Text, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

func (a *App) renderOCR(w io.Writer, resp *deepseek.OCRResponse, pages []deepseek.OCRPage) error {
	switch a.format {
	case "json":
		fields := a.fields
		if len(fields) == 0 {
			fields = ocrJSONFields
		}
		var o obj
		for _, f := range fields {
			switch f {
			case "id":
				o = append(o, kv{"id", resp.ID})
			case "object":
				o = append(o, kv{"object", resp.Object})
			case "created":
				o = append(o, kv{"created", resp.Created})
			case "purpose":
				o = append(o, kv{"purpose", resp.Purpose})
			case "page_count":
				o = append(o, kv{"page_count", resp.PageCount})
			case "pages":
				o = append(o, kv{"pages", pages})
			case "usage":
				o = append(o, kv{"usage", resp.Usage})
			case "text":
				o = append(o, kv{"text", joinOCRPages(pages)})
			}
		}
		b, err := json.Marshal(o)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "%s\n", b)
		return err
	case "jsonl":
		fields := a.fields
		if len(fields) == 0 {
			fields = ocrPageFields
		}
		for _, p := range pages {
			var o obj
			for _, f := range fields {
				switch f {
				case "index":
					o = append(o, kv{"index", p.Index})
				case "text":
					o = append(o, kv{"text", p.Text})
				case "finish_reason":
					o = append(o, kv{"finish_reason", p.FinishReason})
				}
			}
			b, err := json.Marshal(o)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "%s\n", b); err != nil {
				return err
			}
		}
		return nil
	case "md":
		for i, p := range pages {
			if i > 0 {
				if _, err := io.WriteString(w, "\n"); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(w, "## 第 %d 页\n\n%s\n", p.Index+1, strings.TrimRight(p.Text, "\n")); err != nil {
				return err
			}
		}
		return nil
	default:
		for i, p := range pages {
			if i > 0 {
				if _, err := io.WriteString(w, "\n\n"); err != nil {
					return err
				}
			}
			if _, err := io.WriteString(w, strings.TrimRight(p.Text, "\n")); err != nil {
				return err
			}
		}
		if len(pages) > 0 {
			_, err := io.WriteString(w, "\n")
			return err
		}
		return nil
	}
}
