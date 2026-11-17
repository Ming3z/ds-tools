// Package cli implements the ds-tools command surface.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"dstools/internal/cache"
	"dstools/internal/config"
	"dstools/internal/deepseek"
)

// App holds resolved flags, configuration, and I/O streams.
type App struct {
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	stdinPiped bool
	stdoutTTY  bool

	root     *cobra.Command
	executed string

	// raw global flag values
	fAPIKey      string
	fBaseURL     string
	fConfig      string
	fFormat      string
	fFields      string
	fTimeout     time.Duration
	fRetries     int
	fNoCache     bool
	fDryRun      bool
	fQuiet       bool
	fVerbose     bool
	fNoColor     bool
	fErrorFormat string

	// resolved state
	cfg         *config.Config
	cfgPath     string
	cfgErr      error
	apiKey      string
	keySrc      string
	baseURL     string
	baseSrc     string
	timeout     time.Duration
	retries     int
	cacheTTL    time.Duration
	timeoutSrc  string
	retriesSrc  string
	cacheTTLSrc string
	format      string
	fields      []string
	color       bool

	// formatSet reports whether the user chose the output format (flag or env);
	// timeoutSet reports whether --timeout was given explicitly. Tool commands
	// use them to apply their own defaults instead of the generic 30s /
	// pipe-json ones. A config-file timeout is not an explicit tool choice:
	// it may raise a tool's own default but never lowers it.
	formatSet  bool
	timeoutSet bool
}

// Execute runs the CLI and returns the process exit code.
func Execute(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	app := &App{
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		stdinPiped: isPipe(stdin),
		stdoutTTY:  isTTY(stdout),
	}
	root := app.rootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	app.color = !app.fNoColor && app.stdoutTTY && os.Getenv(config.EnvNoColor) == ""

	// 仅用于把 "run 'ds-tools <cmd> --help'" 提示指向正确的子命令。
	if cmd, _, findErr := root.Find(args); findErr == nil && cmd != root {
		app.executed = strings.TrimPrefix(cmd.CommandPath(), "ds-tools ")
	}

	if err := root.Execute(); err != nil {
		translated := translateCobraError(err)
		app.printError(translated)
		return codeOf(translated)
	}
	return ExitOK
}

func (a *App) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ds-tools",
		Short: "面向 Agent 与脚本的 DeepSeek 搜索、抓取与多模态工具",
		Long: "ds-tools 把 DeepSeek 的 Search / Fetch / OCR / ASR / TTS 接口封装成单个静态二进制。\n" +
			"\n" +
			"  POST /v1/tools/search   ->  ds-tools search <query>\n" +
			"  POST /v1/tools/fetch    ->  ds-tools fetch <url>...\n" +
			"  POST /v1/tools/ocr      ->  ds-tools ocr <file>\n" +
			"  POST /v1/tools/asr      ->  ds-tools asr <file>\n" +
			"  POST /v1/tools/tts      ->  ds-tools tts <text>\n" +
			"\n" +
			"Search/Fetch 计费按「成功且有结果」的请求计次，count 与 --content 不影响单价；\n" +
			"OCR 按成功页数、ASR 按音频时长、TTS 按计费字符计费，详见各命令帮助。\n" +
			"\n" +
			"退出码：0 成功（含零结果），1 服务端/请求错误（不要重试；402 余额不足，403 抓取被拦截；\n" +
			"ocr/asr 上传完成后断连也在此列，避免重复计费），\n" +
			"2 鉴权失败（运行 init），3 网络错误（可重试），4 用法错误（未计费）。\n" +
			"Agent 请运行 \"ds-tools skill install --all\" 安装决策契约。",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// 根命令可执行：否则 cobra 会在校验参数前就返回 help，
		// 未知子命令就无法给出退出码 4。
		Args: rootArgsZh,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			a.root = cmd.Root()
			a.executed = strings.TrimPrefix(cmd.CommandPath(), "ds-tools ")
			return a.load(cmd)
		},
	}
	root.SetVersionTemplate("ds-tools {{.Version}}\n")
	// 不提供 completion 命令，同时阻止 cobra 注入英文默认实现。
	root.CompletionOptions.DisableDefaultCmd = true

	pf := root.PersistentFlags()
	pf.StringVar(&a.fAPIKey, "api-key", "", "DeepSeek API Key（优先级高于环境变量与配置文件）")
	pf.StringVar(&a.fBaseURL, "base-url", "", "API 基础地址（默认 "+config.DefaultBaseURL+"）")
	pf.StringVar(&a.fConfig, "config", "", "配置文件路径")
	pf.StringVar(&a.fFormat, "format", "", "输出格式：text|json|jsonl|md（md 为带标题与来源 URL 的可读文档；search/fetch 终端 text、管道 json；ocr/asr 默认 text）")
	pf.StringVar(&a.fFields, "fields", "", "字段投影，逗号分隔（字段集随命令不同，见各命令帮助）")
	pf.DurationVar(&a.fTimeout, "timeout", config.DefaultTimeout, "单次请求超时时间")
	pf.IntVar(&a.fRetries, "retries", config.DefaultRetries, "408/429/5xx 的重试次数，0 表示不重试")
	pf.BoolVar(&a.fNoCache, "no-cache", false, "跳过读取本地响应缓存")
	pf.BoolVar(&a.fDryRun, "dry-run", false, "只校验并打印请求体，不实际发送")
	pf.BoolVarP(&a.fQuiet, "quiet", "q", false, "抑制非错误输出")
	pf.BoolVarP(&a.fVerbose, "verbose", "v", false, "把请求细节打印到 stderr")
	pf.BoolVar(&a.fNoColor, "no-color", false, "禁用 ANSI 颜色")
	pf.StringVar(&a.fErrorFormat, "error-format", "text", "stderr 错误格式：text|json")

	_ = root.MarkPersistentFlagFilename("config")
	_ = root.RegisterFlagCompletionFunc("format", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"text", "json", "jsonl", "md"}, cobra.ShellCompDirectiveNoFileComp
	})
	_ = root.RegisterFlagCompletionFunc("fields", func(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return completionFields(cmd), cobra.ShellCompDirectiveNoFileComp
	})
	_ = root.RegisterFlagCompletionFunc("error-format", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"text", "json"}, cobra.ShellCompDirectiveNoFileComp
	})

	// cobra 在 ExecuteC 里会强制 RemoveCommand+AddCommand 自己的英文 help；
	// 只有通过 SetHelpCommand 注入才能替换它。
	root.SetHelpCommand(newHelpCmd(root))

	root.AddCommand(
		a.newInitCmd(),
		a.newConfigCmd(),
		a.newSearchCmd(),
		a.newFetchCmd(),
		a.newOCRCmd(),
		a.newASRCmd(),
		a.newTTSCmd(),
		a.newDoctorCmd(),
		a.newCacheCmd(),
		a.newSkillCmd(),
		a.newVersionCmd(),
	)

	localizeTree(root)
	return root
}

// completionFields returns the --fields candidates for the executing command.
func completionFields(cmd *cobra.Command) []string {
	switch cmd.Name() {
	case "ocr":
		return ocrAllFields
	case "asr":
		return asrFields
	case "tts":
		return nil
	default:
		return canonicalFields
	}
}

// load resolves configuration and flags. Missing config is not an error.
func (a *App) load(cmd *cobra.Command) error {
	cfg, path, err := config.Load()
	if err != nil {
		a.cfgErr = withCode(ExitServer, err)
		cfg = config.New()
	}
	if a.fConfig != "" {
		abs := a.fConfig
		if strings.HasPrefix(abs, "~/") {
			if home, hErr := os.UserHomeDir(); hErr == nil {
				abs = home + abs[1:]
			}
		}
		cfg, path, err = config.LoadFrom(abs)
		if err != nil {
			a.cfgErr = withCode(ExitUsage, err)
			cfg = config.New()
		} else {
			a.cfgErr = nil
		}
	}
	a.cfg, a.cfgPath = cfg, path

	// API key: flag > env > config file.
	envKey := strings.TrimSpace(os.Getenv(config.EnvAPIKey))
	switch {
	case strings.TrimSpace(a.fAPIKey) != "":
		a.apiKey, a.keySrc = strings.TrimSpace(a.fAPIKey), "flag"
	case envKey != "":
		a.apiKey, a.keySrc = envKey, "env"
	case cfg.APIKey != "":
		a.apiKey, a.keySrc = cfg.APIKey, "config"
	default:
		a.apiKey, a.keySrc = "", "none"
	}

	// Base URL: flag > env > config > default.
	envBase := strings.TrimRight(strings.TrimSpace(os.Getenv(config.EnvBaseURL)), "/")
	switch {
	case strings.TrimSpace(a.fBaseURL) != "":
		a.baseURL, a.baseSrc = strings.TrimRight(strings.TrimSpace(a.fBaseURL), "/"), "flag"
	case envBase != "":
		a.baseURL, a.baseSrc = envBase, "env"
	case cfg.BaseURL != "":
		a.baseURL, a.baseSrc = strings.TrimRight(cfg.BaseURL, "/"), "config"
	default:
		a.baseURL, a.baseSrc = config.DefaultBaseURL, "default"
	}

	timeout, retries, ttl := cfg.ResolvedTimeouts()
	a.timeout, a.retries, a.cacheTTL = timeout, retries, ttl
	a.timeoutSrc, a.retriesSrc, a.cacheTTLSrc = "default", "default", "default"
	if f := cmd.Flags().Lookup("timeout"); f != nil && f.Changed {
		a.timeout = a.fTimeout
		a.timeoutSrc = "flag"
		a.timeoutSet = true
	} else if cfg.Timeout != "" {
		// A config-file timeout is reported as the source, but it is not an
		// explicit tool choice: toolTimeout() may only raise a command's own
		// default with it. Keeping timeoutSet false lets 'config set timeout 1s'
		// from shrinking OCR/ASR/TTS and triggering re-uploads.
		if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
			a.timeoutSrc = "config"
		}
	}
	if cfg.Retries != nil {
		a.retriesSrc = "config"
	}
	if f := cmd.Flags().Lookup("retries"); f != nil && f.Changed {
		a.retries = a.fRetries
		a.retriesSrc = "flag"
	}
	if cfg.CacheTTL != "" {
		if _, err := time.ParseDuration(cfg.CacheTTL); err == nil {
			a.cacheTTLSrc = "config"
		}
	}
	if a.retries < 0 {
		return usageErrf("--retries 必须 >= 0")
	}
	if a.timeout <= 0 {
		return usageErrf("--timeout 必须 > 0")
	}

	// Output format.
	format := strings.TrimSpace(a.fFormat)
	if format != "" {
		a.formatSet = true
	}
	if format == "" {
		if env := strings.TrimSpace(os.Getenv(config.EnvFormat)); env != "" {
			format = env
			a.formatSet = true
		}
	}
	if format == "" {
		if a.stdoutTTY {
			format = "text"
		} else {
			format = "json"
		}
	}
	switch format {
	case "text", "json", "jsonl", "md":
		a.format = format
	default:
		return usageErrf("--format %q 无效（可选：text、json、jsonl、md）", format)
	}

	// Field projection. The global flag is validated against the executing
	// command's schema; OCR/ASR add their own format-specific checks later.
	if cmd.Name() == "tts" && strings.TrimSpace(a.fFields) != "" {
		return usageErrf("tts 不支持 --fields")
	}
	allowed, aliases := fieldsForCommand(cmd.Name())
	fields, err := parseFields(a.fFields, allowed, aliases)
	if err != nil {
		return err
	}
	a.fields = fields

	switch a.fErrorFormat {
	case "text", "json":
	default:
		return usageErrf("--error-format %q 无效（可选：text、json）", a.fErrorFormat)
	}

	if cmd.Flags().Changed("no-color") {
		a.color = !a.fNoColor && a.stdoutTTY && os.Getenv(config.EnvNoColor) == ""
	}
	return nil
}

// toolTimeout resolves the timeout for a long-running tool command. An explicit
// --timeout always wins. Otherwise the resolved config-or-default timeout is a
// floor: it may raise the command's own default (OCR/ASR/TTS never shrink below
// it), so a small config timeout cannot cut a long upload short.
func (a *App) toolTimeout(def time.Duration) time.Duration {
	if a.timeoutSet {
		return a.timeout
	}
	if a.timeout > def {
		return a.timeout
	}
	return def
}

// client builds an API client, failing with an auth exit code when no key exists.
func (a *App) client() (*deepseek.Client, error) {
	if a.cfgErr != nil {
		return nil, a.cfgErr
	}
	if a.apiKey == "" {
		return nil, fmt.Errorf("%w，请运行 'ds-tools init' 或设置 %s", deepseek.ErrNoAPIKey, config.EnvAPIKey)
	}
	return deepseek.NewClient(a.baseURL, a.apiKey, a.timeout, a.retries), nil
}

// cacheDir returns the cache directory rooted next to the resolved config
// file. Deriving from cfgPath (not a.fConfig) handles relative paths, "~/…"
// and config files that are not named config.json.
func (a *App) cacheDir() (string, error) {
	if a.cfgPath != "" {
		return filepath.Join(filepath.Dir(a.cfgPath), "cache"), nil
	}
	return config.CacheDir()
}

// cacheStore returns the on-disk cache rooted next to the config file.
func (a *App) cacheStore() *cache.Cache {
	dir, err := a.cacheDir()
	if err != nil {
		return cache.New("", 0)
	}
	return cache.New(dir, a.cacheTTL)
}

// cacheGet honours --no-cache.
func (a *App) cacheGet(key string, out any) bool {
	if a.fNoCache {
		return false
	}
	return a.cacheStore().Get(key, out)
}

// cacheSet always writes: --no-cache skips reads only, so later runs still benefit.
func (a *App) cacheSet(key string, v any) {
	a.cacheStore().Set(key, v)
}

func (a *App) verbosef(format string, args ...any) {
	if a.fVerbose {
		fmt.Fprintf(a.stderr, format+"\n", args...)
	}
}

// emit writes to a file when path is set, otherwise to stdout.
func (a *App) emit(path string, fn func(io.Writer) error) error {
	if path == "" {
		return fn(a.stdout)
	}
	f, err := os.Create(path)
	if err != nil {
		return withCode(ExitUsage, fmt.Errorf("打开 %s 失败：%w", path, err))
	}
	defer func() { _ = f.Close() }()
	if err := fn(f); err != nil {
		return err
	}
	if a.fVerbose {
		fmt.Fprintf(a.stderr, "已写入 %s\n", path)
	}
	return nil
}

// readInput joins positional args, or reads stdin when the arg is "-" / absent.
func (a *App) readInput(args []string, what string) (string, error) {
	joined := strings.TrimSpace(strings.Join(args, " "))
	if joined != "" && joined != "-" {
		return joined, nil
	}
	if !a.stdinPiped {
		return "", usageErrf("缺少%s（请作为参数传入，或用 '-' 从 stdin 读取）", what)
	}
	b, err := io.ReadAll(a.stdin)
	if err != nil {
		return "", withCode(ExitUsage, fmt.Errorf("读取 stdin 失败：%w", err))
	}
	return strings.TrimSpace(string(b)), nil
}

func (a *App) printDryRun(method, path string, body any) error {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.MarshalIndent(body, "", "  "); err != nil {
			return err
		}
	}
	fmt.Fprintf(a.stdout, "%s %s%s\n", method, a.baseURL, path)
	fmt.Fprintf(a.stdout, "Authorization: Bearer %s\n", config.Mask(a.apiKey))
	if body != nil {
		fmt.Fprint(a.stdout, "Content-Type: application/json\n\n")
		fmt.Fprintf(a.stdout, "%s\n", b)
	}
	return nil
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	// A char device is not necessarily a terminal: /dev/null is one too.
	return term.IsTerminal(int(f.Fd()))
}

func isPipe(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return true
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }
