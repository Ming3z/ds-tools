package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"dstools/internal/config"
	"dstools/internal/deepseek"
)

func (a *App) newInitCmd() *cobra.Command {
	var (
		flagKey   string
		fromStdin bool
		baseURL   string
		noVerify  bool
		force     bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "把 DeepSeek API Key 写入配置文件",
		Long: `交互式写入 API Key，先探活校验，再以 0600 权限写入配置文件。

非交互方式（CI、脚本、Agent）：
  ds-tools init --api-key sk-...
  ds-tools init --api-key-stdin < key.txt      # 不进入 shell 历史
  echo sk-... | ds-tools init

非交互覆盖已有 Key 时必须加 --force。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runInit(cmd.Context(), initOpts{
				flagKey:   flagKey,
				fromStdin: fromStdin,
				baseURL:   baseURL,
				noVerify:  noVerify,
				force:     force,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&flagKey, "api-key", "", "要保存的 API Key（会留在 shell 历史里，建议改用 --api-key-stdin）")
	f.BoolVar(&fromStdin, "api-key-stdin", false, "从 stdin 读取 API Key")
	f.StringVar(&baseURL, "base-url", "", "同时保存 API 基础地址")
	f.BoolVar(&noVerify, "no-verify", false, "跳过校验请求")
	f.BoolVar(&force, "force", false, "覆盖已有配置")
	return cmd
}

type initOpts struct {
	flagKey   string
	fromStdin bool
	baseURL   string
	noVerify  bool
	force     bool
}

func (a *App) runInit(ctx context.Context, o initOpts) error {
	if a.cfgErr != nil {
		return a.cfgErr
	}
	cfg, path := a.cfg, a.cfgPath

	existing := cfg.APIKey
	interactive := !a.stdinPiped && o.flagKey == "" && !o.fromStdin

	var key string
	switch {
	case strings.TrimSpace(o.flagKey) != "":
		key = strings.TrimSpace(o.flagKey)
	case o.fromStdin || (!interactive && a.stdinPiped):
		b, err := io.ReadAll(a.stdin)
		if err != nil {
			return withCode(ExitUsage, fmt.Errorf("读取 stdin 失败：%w", err))
		}
		key = strings.TrimSpace(string(b))
	default:
		envKey := strings.TrimSpace(os.Getenv(config.EnvAPIKey))
		if existing != "" {
			fmt.Fprintf(a.stderr, "已配置 API Key（%s）\n", config.Mask(existing))
		} else if envKey != "" {
			fmt.Fprintf(a.stderr, "在环境变量 %s 中发现 Key（%s）\n", config.EnvAPIKey, config.Mask(envKey))
		}
		prompt := "DeepSeek API Key："
		if existing != "" || envKey != "" {
			prompt = "DeepSeek API Key（留空表示沿用当前 Key）："
		}
		entered, err := readSecret(a.stdin, a.stderr, prompt)
		if err != nil {
			return withCode(ExitUsage, err)
		}
		switch {
		case entered != "":
			key = entered
		case existing != "":
			key = existing
		case envKey != "":
			key = envKey
		}
	}

	if key == "" {
		return usageErrf("没有提供 API Key（请用 --api-key、--api-key-stdin，或交互运行）")
	}
	if !strings.HasPrefix(key, "sk-") {
		fmt.Fprintf(a.stderr, "警告：DeepSeek 的 Key 通常以 \"sk-\" 开头\n")
	}
	if existing != "" && key != existing && !o.force && !interactive {
		return usageErrf("已存在 API Key（%s），覆盖请加 --force", config.Mask(existing))
	}

	targetBase := a.baseURL
	if strings.TrimSpace(o.baseURL) != "" {
		targetBase = strings.TrimRight(strings.TrimSpace(o.baseURL), "/")
		if err := cfg.Set("base_url", targetBase); err != nil {
			return usageErrf("%v", err)
		}
	}

	if !o.noVerify {
		client := deepseek.NewClient(targetBase, key, a.timeout, a.retries)
		vctx, cancel := context.WithTimeout(ctx, a.timeout+time.Second)
		defer cancel()
		bal, err := client.Balance(vctx)
		switch {
		case err == nil:
			fmt.Fprintf(a.stderr, "校验通过：%s\n", describeBalance(bal))
		case isAuthError(err):
			return withCode(ExitAuth, fmt.Errorf("DeepSeek 拒绝了该 Key：%v", err))
		default:
			fmt.Fprintf(a.stderr, "警告：无法校验该 Key：%v\n", err)
		}
	}

	cfg.APIKey = key
	if err := cfg.Save(path); err != nil {
		return withCode(ExitUsage, fmt.Errorf("保存配置失败：%w", err))
	}

	if a.format == "json" || a.format == "jsonl" {
		fmt.Fprintf(a.stdout, `{"config_file":%q,"api_key":%q,"base_url":%q,"verified":%v}`+"\n",
			path, config.Mask(key), targetBase, !o.noVerify)
		return nil
	}
	fmt.Fprintf(a.stdout, "已保存 %s\n", path)
	fmt.Fprintf(a.stdout, "  api_key  %s\n", config.Mask(key))
	fmt.Fprintf(a.stdout, "  base_url %s\n", targetBase)
	return nil
}

func describeBalance(b *deepseek.BalanceResponse) string {
	if b == nil || len(b.BalanceInfos) == 0 {
		return "Key 有效"
	}
	var parts []string
	for _, info := range b.BalanceInfos {
		parts = append(parts, fmt.Sprintf("%s %s", info.TotalBalance, info.Currency))
	}
	status := "可用"
	if !b.IsAvailable {
		status = "不可用"
	}
	return fmt.Sprintf("余额 %s（%s）", strings.Join(parts, "，"), status)
}

func isAuthError(err error) bool {
	var ae *deepseek.APIError
	return errors.As(err, &ae) && ae.Status == 401
}

// readSecret reads a password without echo when stdin is a terminal.
func readSecret(stdin io.Reader, prompt io.Writer, label string) (string, error) {
	fmt.Fprint(prompt, label)
	if f, ok := stdin.(*os.File); ok && isTTY(f) {
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(prompt)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	br := bufio.NewReader(stdin)
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	fmt.Fprintln(prompt)
	return strings.TrimSpace(line), nil
}
