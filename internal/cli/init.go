package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"dstools/internal/config"
	"dstools/internal/deepseek"
	"dstools/internal/skill"
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
		Long: `在终端且不带参数时进入向导：设置 API Key（留空沿用）→ 设置 API 基础地址
（留空沿用，输入 - 重置）→ 探活校验 → 选择要安装的 Agent Skill 目录 →
汇总确认 → 以 0600 权限写入配置文件。

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

	if a.useInitWizard(o) {
		return a.runInitWizard(ctx, cfg, path)
	}

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

// errInitCancelled aborts the wizard without writing the config file.
var errInitCancelled = errors.New("init cancelled")

// useInitWizard reports whether init should run its interactive flow. Flags
// (including --no-verify and --force) keep the scripted behavior.
func (a *App) useInitWizard(o initOpts) bool {
	return !a.stdinPiped && isTTY(a.stdout) && a.format == "text" &&
		o.flagKey == "" && !o.fromStdin && o.baseURL == "" && !o.noVerify && !o.force
}

func (a *App) runInitWizard(ctx context.Context, cfg *config.Config, path string) error {
	key, err := a.promptInitKey(cfg)
	if err != nil {
		return err
	}
	targetBase, err := a.promptInitBaseURL(cfg)
	if err != nil {
		return err
	}
	if err := a.verifyInitKey(ctx, targetBase, key); err != nil {
		if errors.Is(err, errInitCancelled) {
			fmt.Fprintln(a.stderr, "已取消，未写入配置")
			return nil
		}
		return err
	}
	selected, err := a.promptSkillDirs()
	if err != nil {
		return err
	}
	if !a.confirmInit(path, key, targetBase, selected) {
		fmt.Fprintln(a.stderr, "已取消，未写入配置")
		return nil
	}

	cfg.APIKey = key
	if err := cfg.Save(path); err != nil {
		return withCode(ExitUsage, fmt.Errorf("保存配置失败：%w", err))
	}
	a.printInitSaved(path, key, targetBase)
	return a.installSelectedSkills(selected)
}

func (a *App) promptInitKey(cfg *config.Config) (string, error) {
	existing := cfg.APIKey
	envKey := strings.TrimSpace(os.Getenv(config.EnvAPIKey))
	current, source := existing, "config"
	if current == "" && envKey != "" {
		current, source = envKey, "env"
	}
	if current != "" {
		fmt.Fprintf(a.stderr, "当前 API Key：%s（来源=%s）\n", config.Mask(current), source)
	}
	prompt := "DeepSeek API Key："
	if current != "" {
		prompt = "DeepSeek API Key（留空沿用当前）："
	}
	entered, err := readSecret(a.stdin, a.stderr, prompt)
	if err != nil {
		return "", withCode(ExitUsage, err)
	}
	key := entered
	if key == "" {
		key = existing
	}
	if key == "" {
		key = envKey
	}
	if key == "" {
		return "", usageErrf("没有提供 API Key")
	}
	if !strings.HasPrefix(key, "sk-") {
		fmt.Fprintf(a.stderr, "警告：DeepSeek 的 Key 通常以 \"sk-\" 开头\n")
	}
	return key, nil
}

func (a *App) promptInitBaseURL(cfg *config.Config) (string, error) {
	fmt.Fprintln(a.stderr)
	for {
		current, source := resolvedBaseURL(cfg)
		fmt.Fprintf(a.stderr, "当前 API 基础地址：%s（来源=%s）\n", current, source)
		entered, err := readLine(a.stdin, a.stderr, "API 基础地址（回车保持不变，输入则重置）：")
		if err != nil {
			return "", withCode(ExitUsage, err)
		}
		switch {
		case entered == "":
			return current, nil
		case entered == "-":
			if err := cfg.Unset("base_url"); err != nil {
				return "", usageErrf("%v", err)
			}
			reset, _ := resolvedBaseURL(cfg)
			return reset, nil
		default:
			if err := cfg.Set("base_url", entered); err != nil {
				fmt.Fprintf(a.stderr, "无效地址：%v\n", err)
				continue
			}
			return cfg.BaseURL, nil
		}
	}
}

func (a *App) verifyInitKey(ctx context.Context, base, key string) error {
	fmt.Fprintln(a.stderr)
	client := deepseek.NewClient(base, key, a.timeout, a.retries)
	for {
		vctx, cancel := context.WithTimeout(ctx, a.timeout+time.Second)
		bal, err := client.Balance(vctx)
		cancel()
		switch {
		case err == nil:
			fmt.Fprintf(a.stderr, "校验通过：%s\n\n", describeBalance(bal))
			return nil
		case isAuthError(err):
			return withCode(ExitAuth, fmt.Errorf("DeepSeek 拒绝了该 Key：%v", err))
		}
		fmt.Fprintf(a.stderr, "警告：无法校验该 Key：%v\n", err)
		choice, readErr := readLine(a.stdin, a.stderr, "[r] 重试 / [s] 仍然保存 / [c] 取消：")
		if readErr != nil {
			return withCode(ExitUsage, readErr)
		}
		switch strings.ToLower(choice) {
		case "s", "save":
			return nil
		case "c", "cancel":
			return errInitCancelled
		}
	}
}

func (a *App) promptSkillDirs() ([]skillDir, error) {
	dirs, err := skillDirs()
	if err != nil {
		return nil, withCode(ExitUsage, err)
	}
	fmt.Fprintln(a.stderr, "安装 Agent Skill 到：")
	for i, d := range dirs {
		mark := ""
		if _, statErr := os.Stat(filepath.Join(d.Dir, skill.Name, skillFileName)); statErr == nil {
			mark = "  [已安装]"
		}
		fmt.Fprintf(a.stderr, "  %d) %-6s %s%s\n", i+1, d.Agent, d.Dir, mark)
	}
	fmt.Fprintln(a.stderr, "  回车跳过；输入编号（如 1,3）或 all 全选")
	for {
		line, err := readLine(a.stdin, a.stderr, "选择：")
		if err != nil {
			return nil, withCode(ExitUsage, err)
		}
		indexes, err := parseSkillSelection(line, len(dirs))
		if err != nil {
			fmt.Fprintf(a.stderr, "%v\n", err)
			continue
		}
		selected := make([]skillDir, 0, len(indexes))
		for _, i := range indexes {
			selected = append(selected, dirs[i])
		}
		return selected, nil
	}
}

// parseSkillSelection maps "all", "1,3" or "1 3" to zero-based indexes.
func parseSkillSelection(line string, n int) ([]int, error) {
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return nil, nil
	}
	if line == "all" {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out, nil
	}
	seen := make(map[int]bool, n)
	var out []int
	parts := strings.FieldsFunc(line, func(r rune) bool {
		return r == ',' || r == '，' || r == ' ' || r == '\t'
	})
	for _, part := range parts {
		v, err := strconv.Atoi(part)
		if err != nil || v < 1 || v > n {
			return nil, fmt.Errorf("无效选择 %q（可选 1-%d，或 all，回车跳过）", part, n)
		}
		if !seen[v-1] {
			seen[v-1] = true
			out = append(out, v-1)
		}
	}
	sort.Ints(out)
	return out, nil
}

func (a *App) confirmInit(path, key, base string, selected []skillDir) bool {
	fmt.Fprintf(a.stderr, "\n即将写入 %s\n  api_key  %s\n  base_url %s\n", path, config.Mask(key), base)
	if len(selected) == 0 {
		fmt.Fprintln(a.stderr, "  skill    跳过")
	} else {
		names := make([]string, 0, len(selected))
		for _, d := range selected {
			names = append(names, d.Agent)
		}
		fmt.Fprintf(a.stderr, "  skill    %s\n", strings.Join(names, "、"))
	}
	line, err := readLine(a.stdin, a.stderr, "确认写入？[Y/n] ")
	if err != nil {
		return false
	}
	switch strings.ToLower(line) {
	case "", "y", "yes", "是":
		return true
	default:
		return false
	}
}

func (a *App) printInitSaved(path, key, base string) {
	fmt.Fprintf(a.stdout, "\n已保存 %s\n", path)
	fmt.Fprintf(a.stdout, "  api_key  %s\n", config.Mask(key))
	fmt.Fprintf(a.stdout, "  base_url %s\n", base)
}

func (a *App) installSelectedSkills(selected []skillDir) error {
	var failures []string
	for _, d := range selected {
		file := filepath.Join(d.Dir, skill.Name, skillFileName)
		action, err := writeSkill(file)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		fmt.Fprintf(a.stdout, "%s %s\n", installActionLabel(action), file)
	}
	if len(failures) > 0 {
		return withCode(ExitUsage, fmt.Errorf("配置已保存，但 skill 安装失败：%s", strings.Join(failures, "；")))
	}
	return nil
}

// resolvedBaseURL returns the effective base URL and its source without CLI flags.
func resolvedBaseURL(cfg *config.Config) (value, source string) {
	if env := strings.TrimRight(strings.TrimSpace(os.Getenv(config.EnvBaseURL)), "/"); env != "" {
		return env, "env"
	}
	if cfg.BaseURL != "" {
		return cfg.BaseURL, "config"
	}
	return config.DefaultBaseURL, "default"
}

// readLine reads one line for wizard prompts.
func readLine(r io.Reader, w io.Writer, prompt string) (string, error) {
	fmt.Fprint(w, prompt)
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
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
