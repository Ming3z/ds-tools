package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"dstools/internal/config"
)

// uploadInput is a local file source that can be reopened for retries and is
// identified by its content hash for the response cache.
type uploadInput struct {
	Name string
	Size int64
	Hash string
	Open func() (io.ReadCloser, error)

	cleanup func()
}

func (u *uploadInput) Close() {
	if u.cleanup != nil {
		u.cleanup()
	}
}

// ctxReader fails before a read once ctx is cancelled, so spooling a large
// stdin and hashing a large file unwind instead of finishing after a signal.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// openUpload prepares a file (or spools stdin to a temp file) and hashes its
// content. maxBytes is enforced before hashing a regular file. ctx cancellation
// removes any spool file before returning.
func (a *App) openUpload(ctx context.Context, path, stdinName string, maxBytes int64) (*uploadInput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "-" {
		if !a.stdinPiped {
			return nil, usageErrf("从 stdin 读取需要管道输入")
		}
		tmp, err := os.CreateTemp("", "ds-tools-upload-*")
		if err != nil {
			return nil, withCode(ExitUsage, fmt.Errorf("创建临时文件失败：%w", err))
		}
		tmpName := tmp.Name()
		h := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(ctxReader{ctx: ctx, r: a.stdin}, maxBytes+1))
		closeErr := tmp.Close()
		if copyErr != nil {
			_ = os.Remove(tmpName)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, withCode(ExitUsage, fmt.Errorf("读取 stdin 失败：%w", copyErr))
		}
		if closeErr != nil {
			_ = os.Remove(tmpName)
			return nil, withCode(ExitUsage, fmt.Errorf("写入临时文件失败：%w", closeErr))
		}
		if n == 0 {
			_ = os.Remove(tmpName)
			return nil, usageErrf("stdin 为空")
		}
		if n > maxBytes {
			_ = os.Remove(tmpName)
			return nil, usageErrf("stdin 大小超过上限 %s", humanBytes(maxBytes))
		}
		name := strings.TrimSpace(stdinName)
		if name == "" {
			name = "stdin"
		}
		return &uploadInput{
			Name:    name,
			Size:    n,
			Hash:    hex.EncodeToString(h.Sum(nil)),
			Open:    func() (io.ReadCloser, error) { return os.Open(tmpName) },
			cleanup: func() { _ = os.Remove(tmpName) },
		}, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, withCode(ExitUsage, fmt.Errorf("读取 %s 失败：%w", path, err))
	}
	if !info.Mode().IsRegular() {
		return nil, usageErrf("%s 不是普通文件", path)
	}
	if info.Size() == 0 {
		return nil, usageErrf("文件 %s 为空", path)
	}
	if info.Size() > maxBytes {
		return nil, usageErrf("文件大小 %s 超过上限 %s", humanBytes(info.Size()), humanBytes(maxBytes))
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, withCode(ExitUsage, fmt.Errorf("打开 %s 失败：%w", path, err))
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, ctxReader{ctx: ctx, r: f})
	closeErr := f.Close()
	if copyErr != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, withCode(ExitUsage, fmt.Errorf("读取 %s 失败：%w", path, copyErr))
	}
	if closeErr != nil {
		return nil, withCode(ExitUsage, fmt.Errorf("关闭 %s 失败：%w", path, closeErr))
	}
	return &uploadInput{
		Name: filepath.Base(path),
		Size: info.Size(),
		Hash: hex.EncodeToString(h.Sum(nil)),
		Open: func() (io.ReadCloser, error) { return os.Open(path) },
	}, nil
}

// statUpload validates a local path for --dry-run without reading stdin. It
// returns size -1 when the input is stdin.
func (a *App) statUpload(path, stdinName string, maxBytes int64) (string, int64, error) {
	if path == "-" {
		if !a.stdinPiped {
			return "", 0, usageErrf("从 stdin 读取需要管道输入")
		}
		name := strings.TrimSpace(stdinName)
		if name == "" {
			name = "stdin"
		}
		return name, -1, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, withCode(ExitUsage, fmt.Errorf("读取 %s 失败：%w", path, err))
	}
	if !info.Mode().IsRegular() {
		return "", 0, usageErrf("%s 不是普通文件", path)
	}
	if info.Size() == 0 {
		return "", 0, usageErrf("文件 %s 为空", path)
	}
	if info.Size() > maxBytes {
		return "", 0, usageErrf("文件大小 %s 超过上限 %s", humanBytes(info.Size()), humanBytes(maxBytes))
	}
	return filepath.Base(path), info.Size(), nil
}

func (a *App) printDryRunMultipart(path string, fields [][2]string, name string, size int64) error {
	fmt.Fprintf(a.stdout, "POST %s%s\n", a.baseURL, path)
	fmt.Fprintf(a.stdout, "Authorization: Bearer %s\n", config.Mask(a.apiKey))
	fmt.Fprintf(a.stdout, "Content-Type: multipart/form-data\n\n")
	for _, f := range fields {
		fmt.Fprintf(a.stdout, "%s=%s\n", f[0], f[1])
	}
	if size >= 0 {
		fmt.Fprintf(a.stdout, "file=%s（%s）\n", name, humanBytes(size))
	} else {
		fmt.Fprintf(a.stdout, "file=%s（大小未知）\n", name)
	}
	return nil
}
