package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"dstools/internal/deepseek"
)

type asrOpts struct {
	filename string
	output   string
}

func (a *App) newASRCmd() *cobra.Command {
	o := asrOpts{filename: "stdin"}
	cmd := &cobra.Command{
		Use:   "asr <file|->",
		Short: "调用 DeepSeek ASR 接口把音频转写为文字",
		Long: `把音频文件转写为文字（POST /v1/tools/asr）。

支持 16 位 PCM WAV 或 Ogg Opus，最大 8 MiB、最长 120 秒；语种由服务端自动
识别。双声道音频只识别左声道。CLI 会在上传前检查 WAV 头/时长或 Ogg Opus
魔数，避免无效请求计费。
未显式指定 --format 时默认 text，便于直接重定向成 .txt。

计费：按音频时长，每小时 0.78 元（限时 5 折 0.39 元），不足 1 秒按 1 秒计。
--timeout 未指定时取配置值与 300 秒的较大者（配置不会把它压低）。

示例：
  ds-tools asr speech.wav
  ds-tools asr speech.ogg --format json
  cat speech.wav | ds-tools asr - -o speech.txt

` + fieldsUsage(asrFields, nil),
		Args: exactPositionalArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runASR(cmd.Context(), args[0], o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.filename, "filename", "stdin", "从 stdin 读取时使用的文件名")
	f.StringVarP(&o.output, "output", "o", "", "把输出写入文件而不是 stdout")
	_ = cmd.MarkFlagFilename("output")
	return cmd
}

func (a *App) runASR(ctx context.Context, path string, o asrOpts) error {
	if !a.formatSet {
		a.format = "text"
	}
	a.timeout = a.toolTimeout(300 * time.Second)
	if a.format == "text" || a.format == "md" {
		if len(a.fields) > 0 {
			return usageErrf("--fields 仅支持 --format json 或 jsonl")
		}
	} else if err := requireFields(a.fields, asrFields); err != nil {
		return err
	}

	if a.fDryRun {
		name, size, err := a.statUpload(path, o.filename, 8<<20)
		if err != nil {
			return err
		}
		if path != "-" {
			if err := a.validateASRPath(path); err != nil {
				return err
			}
		}
		return a.printDryRunMultipart(deepseek.PathASR, nil, name, size)
	}

	client, err := a.client()
	if err != nil {
		return err
	}

	up, err := a.openUpload(path, o.filename, 8<<20)
	if err != nil {
		return err
	}
	defer up.Close()
	if err := validateASR(up); err != nil {
		return err
	}

	key := "asr|" + up.Hash
	resp := &deepseek.ASRResponse{}
	if a.cacheGet(key, resp) {
		a.verbosef("缓存命中：asr %s", up.Name)
	} else {
		a.verbosef("POST %s%s file=%s（%s）", a.baseURL, deepseek.PathASR, up.Name, humanBytes(up.Size))
		resp, err = client.ASR(ctx, deepseek.Upload{Name: up.Name, Size: up.Size, Open: up.Open})
		if err != nil {
			return err
		}
		a.cacheSet(key, resp)
	}
	return a.emit(o.output, func(w io.Writer) error {
		return a.renderASR(w, resp)
	})
}

func (a *App) validateASRPath(path string) error {
	up, err := a.openUpload(path, "stdin", 8<<20)
	if err != nil {
		return err
	}
	defer up.Close()
	return validateASR(up)
}

func validateASR(up *uploadInput) error {
	f, err := up.Open()
	if err != nil {
		return withCode(ExitUsage, fmt.Errorf("打开 %s 失败：%w", up.Name, err))
	}
	var hdr [4]byte
	_, readErr := io.ReadFull(f, hdr[:])
	_ = f.Close()
	if readErr != nil {
		return usageErrf("音频文件过短或无法读取")
	}
	switch string(hdr[:]) {
	case "RIFF":
		return validateWAV(up)
	case "OggS":
		return validateOggOpus(up)
	default:
		return usageErrf("仅支持 16 位 PCM WAV 或 Ogg Opus")
	}
}

func validateOggOpus(up *uploadInput) error {
	f, err := up.Open()
	if err != nil {
		return withCode(ExitUsage, fmt.Errorf("打开 %s 失败：%w", up.Name, err))
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 512)
	n, _ := io.ReadFull(f, buf)
	if !bytes.Contains(buf[:n], []byte("OpusHead")) {
		return usageErrf("Ogg 文件不是 Opus 编码")
	}
	return nil
}

func validateWAV(up *uploadInput) error {
	f, err := up.Open()
	if err != nil {
		return withCode(ExitUsage, fmt.Errorf("打开 %s 失败：%w", up.Name, err))
	}
	defer func() { _ = f.Close() }()

	var riff [12]byte
	if _, err := io.ReadFull(f, riff[:]); err != nil {
		return usageErrf("WAV 文件头不完整")
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return usageErrf("不是有效的 WAV 文件")
	}

	var (
		fmtFound   bool
		audioFmt   uint16
		channels   uint16
		sampleRate uint32
		bits       uint16
	)
	for {
		var ch [8]byte
		if _, err := io.ReadFull(f, ch[:]); err != nil {
			if err == io.EOF {
				break
			}
			return usageErrf("WAV 文件结构无效")
		}
		id := string(ch[0:4])
		size := binary.LittleEndian.Uint32(ch[4:8])
		switch id {
		case "fmt ":
			if size < 16 || size > 1024 {
				return usageErrf("WAV fmt 块无效")
			}
			buf := make([]byte, size)
			if _, err := io.ReadFull(f, buf); err != nil {
				return usageErrf("WAV fmt 块不完整")
			}
			audioFmt = binary.LittleEndian.Uint16(buf[0:2])
			channels = binary.LittleEndian.Uint16(buf[2:4])
			sampleRate = binary.LittleEndian.Uint32(buf[4:8])
			bits = binary.LittleEndian.Uint16(buf[14:16])
			if size%2 == 1 {
				if _, err := io.CopyN(io.Discard, f, 1); err != nil {
					return usageErrf("WAV fmt 块不完整")
				}
			}
			fmtFound = true
		case "data":
			if !fmtFound {
				return usageErrf("WAV data 块出现在 fmt 块之前")
			}
			if err := checkWAVFmt(audioFmt, channels, sampleRate, bits); err != nil {
				return err
			}
			if size == 0 {
				return usageErrf("WAV 音频数据为空")
			}
			if size != 0xFFFFFFFF {
				seconds := float64(size) / (float64(sampleRate) * float64(channels) * 2)
				if seconds > 120 {
					return usageErrf("音频时长 %.1f 秒超过 120 秒上限", seconds)
				}
			}
			return nil
		default:
			if _, err := io.CopyN(io.Discard, f, int64(size)); err != nil {
				return usageErrf("WAV 文件结构无效")
			}
			if size%2 == 1 {
				if _, err := io.CopyN(io.Discard, f, 1); err != nil {
					return usageErrf("WAV 文件结构无效")
				}
			}
		}
	}
	if !fmtFound {
		return usageErrf("WAV 缺少 fmt 块")
	}
	return usageErrf("WAV 缺少 data 块")
}

func checkWAVFmt(audioFmt, channels uint16, sampleRate uint32, bits uint16) error {
	// 1 = PCM, 0xFFFE = WAVE_FORMAT_EXTENSIBLE. With 16-bit samples the
	// extensible sub-format is PCM in practice; rejecting it would turn away
	// valid files. The server still validates the actual encoding.
	if audioFmt != 1 && audioFmt != 0xFFFE {
		return usageErrf("WAV 必须是 16 位 PCM（当前格式 %d）", audioFmt)
	}
	if bits != 16 {
		return usageErrf("WAV 必须是 16 位 PCM（当前 %d 位）", bits)
	}
	if channels != 1 && channels != 2 {
		return usageErrf("WAV 只支持单声道或双声道（当前 %d 声道）", channels)
	}
	switch sampleRate {
	case 8000, 16000, 22050, 24000, 32000, 44100, 48000:
		return nil
	default:
		return usageErrf("WAV 采样率 %d Hz 不受支持", sampleRate)
	}
}

func (a *App) renderASR(w io.Writer, resp *deepseek.ASRResponse) error {
	switch a.format {
	case "json", "jsonl":
		fields := a.fields
		if len(fields) == 0 {
			fields = asrFields
		}
		var o obj
		for _, f := range fields {
			switch f {
			case "request_id":
				o = append(o, kv{"request_id", resp.RequestID})
			case "text":
				o = append(o, kv{"text", resp.Text})
			case "duration_ms":
				o = append(o, kv{"duration_ms", resp.DurationMS})
			}
		}
		b, err := json.Marshal(o)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "%s\n", b)
		return err
	case "md":
		_, err := fmt.Fprintf(w, "> 时长 %.1fs\n\n%s\n", float64(resp.DurationMS)/1000, strings.TrimRight(resp.Text, "\n"))
		return err
	default:
		_, err := fmt.Fprintf(w, "%s\n", strings.TrimRight(resp.Text, "\n"))
		return err
	}
}
