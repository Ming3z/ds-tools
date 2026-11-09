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
	cfg      *config.Config
	cfgPath  string
	cfgErr   error
	apiKey   string
	keySrc   string
	baseURL  string
	baseSrc  string
	timeout  time.Duration
	retries  int
	cacheTTL time.Duration
	format   string
	fields   []string
	color    bool
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
		Short: "面向 Agent 与脚本的 DeepSeek 网页搜索与抓取",
		Long: "ds-tools 把 DeepSeek 的 Search / Fetch 接口封装成单个静态二进制。\n" +
			"\n" +
			"  POST /v1/tools/search   ->  ds-tools search <query>\n" +
			"  POST /v1/tools/fetch    ->  ds-tools fetch <url>...\n" +
			"\n" +
			"计费按「成功且有结果」的请求计次，count 与 --content 不影响单价；因此\n" +
			"search --content 一次请求即可连带正文，fetch 只用于抓已知 URL。\n" +
			"\n" +
			"退出码：0 成功，1 服务端/请求错误，2 鉴权失败，3 网络错误，4 用法错误。\n" +
			"Agent 请运行 \"ds-tools skill print\" 获取完整使用契约。",
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
	pf.StringVar(&a.fFormat, "format", "", "输出格式：text|json|jsonl|md（终端默认 text，管道默认 json）")
	pf.StringVar(&a.fFields, "fields", "", "字段投影，逗号分隔，例如 title,url,summary")
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
	_ = root.RegisterFlagCompletionFunc("fields", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return canonicalFields, cobra.ShellCompDirectiveNoFileComp
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
		a.newDoctorCmd(),
		a.newCacheCmd(),
		a.newSkillCmd(),
		a.newVersionCmd(),
	)

	localizeTree(root)
	return root
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
	if f := cmd.Flags().Lookup("timeout"); f != nil && f.Changed {
		a.timeout = a.fTimeout
	}
	if f := cmd.Flags().Lookup("retries"); f != nil && f.Changed {
		a.retries = a.fRetries
	}
	if a.retries < 0 {
		return usageErrf("--retries 必须 >= 0")
	}
	if a.timeout <= 0 {
		return usageErrf("--timeout 必须 > 0")
	}

	// Output format.
	format := strings.TrimSpace(a.fFormat)
	if format == "" {
		format = strings.TrimSpace(os.Getenv(config.EnvFormat))
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

	// Field projection.
	fields, err := parseFields(a.fFields)
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
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
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
