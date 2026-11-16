package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"dstools/internal/deepseek"
)

type ttsOpts struct {
	voice        string
	audioFormat  string
	keepMarkdown bool
	output       string
}

func (a *App) newTTSCmd() *cobra.Command {
	o := ttsOpts{voice: "stella", audioFormat: "wav"}
	cmd := &cobra.Command{
		Use:   "tts <text|->",
		Short: "调用 DeepSeek TTS 接口把文字合成为语音",
		Long: `把文字合成为语音（POST /v1/tools/tts），音频在响应体中流式返回。

音频是二进制：未指定 -o 且 stdout 是终端时拒绝执行；stdout 是管道时直接写
stdout，例如 ds-tools tts "..." > speech.wav。指定 -o 文件时，stdout 按
--format 输出一行元信息（text 为人读提示，json/jsonl 为机器可读 JSON）。

--voice          stella（海星，女声，默认）| tide（暗潮，男声）
--audio-format   wav（默认）| pcm（24 kHz、16 位、单声道裸 PCM）
--keep-markdown  保留 Markdown 符号；默认朗读前去掉

限制：文本最多 10,000 个计费字符（汉字算 2，其他算 1），请求体最多 64 KiB；
单次请求最多返回 30 分钟音频。计费：每 10,000 字符 3 元（限时 5 折 1.5 元）。
未显式指定 --timeout 时默认 600 秒。

示例：
  ds-tools tts "你好，我是 DeepSeek。" -o hello.wav
  ds-tools tts - < article.md > speech.wav
  ds-tools tts "..." --voice tide --audio-format pcm -o hello.pcm

tts 不支持 --fields；带 -o 时 stdout 只有一行元信息。`,
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runTTS(cmd.Context(), args, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.voice, "voice", "stella", "音色：stella（女声）| tide（男声）")
	f.StringVar(&o.audioFormat, "audio-format", "wav", "音频格式：wav|pcm")
	f.BoolVar(&o.keepMarkdown, "keep-markdown", false, "保留 Markdown 符号（默认朗读前去掉）")
	f.StringVarP(&o.output, "output", "o", "", "音频写入文件；'-' 表示 stdout")
	_ = cmd.RegisterFlagCompletionFunc("voice", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"stella", "tide"}, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("audio-format", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"wav", "pcm"}, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.MarkFlagFilename("output")
	return cmd
}

func (a *App) runTTS(ctx context.Context, args []string, o ttsOpts) error {
	if !a.timeoutSet {
		a.timeout = 600 * time.Second
	}
	text, err := a.readInput(args, "文本")
	if err != nil {
		return err
	}
	if text == "" {
		return usageErrf("文本不能为空")
	}
	switch o.voice {
	case "stella", "tide":
	default:
		return usageErrf("--voice 只能是 stella 或 tide")
	}
	switch o.audioFormat {
	case "wav", "pcm":
	default:
		return usageErrf("--audio-format 只能是 wav 或 pcm")
	}
	if len(a.fields) > 0 {
		return usageErrf("tts 不支持 --fields")
	}
	switch a.format {
	case "text", "json", "jsonl":
	default:
		return usageErrf("tts 的 --format 只支持 text、json、jsonl")
	}

	chars := ttsBillingChars(text)
	if chars > 10000 {
		return usageErrf("文本计费字符 %d 超过 10,000（汉字算 2，其他算 1）", chars)
	}
	filter := !o.keepMarkdown
	req := &deepseek.TTSRequest{
		Text:           text,
		VoiceID:        o.voice,
		Format:         o.audioFormat,
		FilterMarkdown: &filter,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if len(body) > 64*1024 {
		return usageErrf("请求体 %d 字节超过 64 KiB 上限", len(body))
	}

	if a.fDryRun {
		return a.printDryRun("POST", deepseek.PathTTS, req)
	}

	toStdout := false
	switch {
	case o.output == "":
		if a.stdoutTTY {
			return usageErrf("TTS 输出是二进制音频，请用 -o 写入文件，或重定向到管道")
		}
		toStdout = true
	case o.output == "-":
		if a.stdoutTTY {
			return usageErrf("stdout 是终端，不能用 -o - 输出二进制音频")
		}
		toStdout = true
	}

	client, err := a.client()
	if err != nil {
		return err
	}
	a.verbosef("POST %s%s voice=%s format=%s chars=%d", a.baseURL, deepseek.PathTTS, o.voice, o.audioFormat, chars)
	stream, err := client.TTS(ctx, req)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()

	var n int64
	if toStdout {
		if o.audioFormat == "wav" {
			br := bufio.NewReader(stream)
			if err := checkWAVHeader(br); err != nil {
				return withCode(ExitServer, err)
			}
			n, err = io.Copy(a.stdout, br)
		} else {
			n, err = io.Copy(a.stdout, stream)
		}
		if err != nil {
			return withCode(ExitServer, fmt.Errorf("音频流中断：%w", err))
		}
		a.verbosef("已输出 %s 音频到 stdout", humanBytes(n))
		return nil
	}

	if err := a.writeTTSFile(o.output, stream, o.audioFormat, &n); err != nil {
		return err
	}
	if a.fQuiet {
		return nil
	}
	switch a.format {
	case "json", "jsonl":
		payload := struct {
			Output       string `json:"output"`
			Bytes        int64  `json:"bytes"`
			VoiceID      string `json:"voice_id"`
			AudioFormat  string `json:"audio_format"`
			BillingChars int    `json:"billing_chars"`
		}{
			Output:       o.output,
			Bytes:        n,
			VoiceID:      o.voice,
			AudioFormat:  o.audioFormat,
			BillingChars: chars,
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(a.stdout, "%s\n", b)
		return err
	default:
		_, err := fmt.Fprintf(a.stdout, "已写入 %s（%s，voice=%s，%s）\n", o.output, humanBytes(n), o.voice, o.audioFormat)
		return err
	}
}

func (a *App) writeTTSFile(target string, r io.Reader, format string, n *int64) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".tts-*.tmp")
	if err != nil {
		return withCode(ExitUsage, fmt.Errorf("创建临时文件失败：%w", err))
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	var src io.Reader = r
	if format == "wav" {
		br := bufio.NewReader(r)
		if err := checkWAVHeader(br); err != nil {
			_ = tmp.Close()
			return withCode(ExitServer, err)
		}
		src = br
	}
	written, copyErr := io.Copy(tmp, src)
	closeErr := tmp.Close()
	if copyErr != nil {
		return withCode(ExitServer, fmt.Errorf("写入音频失败：%w", copyErr))
	}
	if closeErr != nil {
		return withCode(ExitUsage, fmt.Errorf("写入音频失败：%w", closeErr))
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return withCode(ExitUsage, fmt.Errorf("设置权限失败：%w", err))
	}
	if err := os.Rename(tmpName, target); err != nil {
		return withCode(ExitUsage, fmt.Errorf("写入 %s 失败：%w", target, err))
	}
	*n = written
	return nil
}

func checkWAVHeader(br *bufio.Reader) error {
	hdr, err := br.Peek(12)
	if err != nil {
		return fmt.Errorf("音频响应过短，不是有效的 WAV：%w", err)
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return fmt.Errorf("音频响应不是有效的 WAV 文件")
	}
	return nil
}

func ttsBillingChars(s string) int {
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			n += 2
		} else {
			n++
		}
	}
	return n
}
