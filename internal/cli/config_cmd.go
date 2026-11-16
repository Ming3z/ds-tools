package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"dstools/internal/config"
)

func (a *App) newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "查看与修改配置文件",
		Long: `管理 ds-tools 配置文件（0600 权限），API Key 一律脱敏显示。

可设置的键：` + strings.Join(config.Keys, "、") + `

API Key 优先级：--api-key 选项 > ` + config.EnvAPIKey + ` 环境变量 > 配置文件。`,
	}
	cmd.AddCommand(
		a.newConfigShowCmd(),
		a.newConfigSetCmd(),
		a.newConfigUnsetCmd(),
	)
	return cmd
}

// configKeyView is one settable key: the effective value, where it came from,
// and the value stored in the config file (nil when the key is unset).
type configKeyView struct {
	Key    string
	Value  any
	Source string
	File   any
}

func (a *App) newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "显示当前生效的配置及来源（已脱敏）",
		Long: `逐项显示生效值、来源（flag/env/config/default）与配置文件中的原值。

未在配置文件中设置的项标记为 (未设置)。API Key 一律脱敏。`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if a.cfgErr != nil {
				return a.cfgErr
			}
			cacheDir, _ := a.cacheDir()
			exists, mode, warn := a.cfgFileState()
			keys := a.configKeyViews()

			if a.format == "json" || a.format == "jsonl" {
				rows := []kv{
					{"config_file", a.cfgPath},
					{"config_dir", filepath.Dir(a.cfgPath)},
					{"config_file_exists", exists},
					{"config_file_mode", modeOrNil(mode)},
					{"config_file_warn", warn},
					{"cache_dir", cacheDir},
					{"format", a.format},
				}
				for _, k := range keys {
					rows = append(rows,
						kv{k.Key, k.Value},
						kv{k.Key + "_source", k.Source},
						kv{k.Key + "_file", k.File},
					)
				}
				fmt.Fprintf(a.stdout, "%s\n", mustJSON(obj(rows)))
				return nil
			}

			modeText := mode
			if !exists {
				modeText = "(尚未创建)"
			}
			fmt.Fprintf(a.stdout, "%-20s %s\n", "config_file", a.cfgPath)
			fmt.Fprintf(a.stdout, "%-20s %s\n", "config_dir", filepath.Dir(a.cfgPath))
			fmt.Fprintf(a.stdout, "%-20s %t\n", "config_file_exists", exists)
			fmt.Fprintf(a.stdout, "%-20s %s\n", "config_file_mode", modeText)
			if warn != "" {
				fmt.Fprintf(a.stdout, "%-20s %s\n", "config_file_warn", warn)
			}
			fmt.Fprintf(a.stdout, "%-20s %s\n", "cache_dir", cacheDir)
			fmt.Fprintf(a.stdout, "%-20s %s\n", "format", a.format)

			tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw)
			for _, k := range keys {
				file := "(未设置)"
				if k.File != nil {
					file = fmt.Sprintf("%v", k.File)
				}
				fmt.Fprintf(tw, "%s\t%v\tsource=%s\tfile=%s\n", k.Key, k.Value, k.Source, file)
			}
			return tw.Flush()
		},
	}
}

// configKeyViews renders every settable key in config.Keys order.
func (a *App) configKeyViews() []configKeyView {
	fileOrNil := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	var apiKeyFile any
	if a.cfg.APIKey != "" {
		apiKeyFile = config.Mask(a.cfg.APIKey)
	}
	var retriesFile any
	if a.cfg.Retries != nil {
		retriesFile = *a.cfg.Retries
	}
	effectiveKey := "(未设置)"
	if a.apiKey != "" {
		effectiveKey = config.Mask(a.apiKey)
	}
	return []configKeyView{
		{"api_key", effectiveKey, a.keySrc, apiKeyFile},
		{"base_url", a.baseURL, a.baseSrc, fileOrNil(a.cfg.BaseURL)},
		{"timeout", a.timeout.String(), a.timeoutSrc, fileOrNil(a.cfg.Timeout)},
		{"retries", a.retries, a.retriesSrc, retriesFile},
		{"cache_ttl", a.cacheTTL.String(), a.cacheTTLSrc, fileOrNil(a.cfg.CacheTTL)},
	}
}

// cfgFileState reports whether the config file exists and, if so, its
// permission bits and a warning when they are too permissive.
func (a *App) cfgFileState() (exists bool, mode, warn string) {
	info, err := os.Stat(a.cfgPath)
	if err != nil {
		return false, "", ""
	}
	mode = fmt.Sprintf("%04o", info.Mode().Perm())
	if pw := config.PermWarning(a.cfgPath); pw != nil {
		warn = pw.Error()
	}
	return true, mode, warn
}

func modeOrNil(mode string) any {
	if mode == "" {
		return nil
	}
	return mode
}

func (a *App) newConfigSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "设置一个配置项",
		Args:  exactPositionalArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if a.cfgErr != nil {
				return a.cfgErr
			}
			if err := a.cfg.Set(args[0], args[1]); err != nil {
				return usageErrf("%v", err)
			}
			if err := a.cfg.Save(a.cfgPath); err != nil {
				return withCode(ExitUsage, fmt.Errorf("保存配置失败：%w", err))
			}
			fmt.Fprintf(a.stdout, "已更新 %s（%s）\n", args[0], a.cfgPath)
			return nil
		},
	}
}

func (a *App) newConfigUnsetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unset <key>",
		Short: "移除一个配置项",
		Args:  exactPositionalArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if a.cfgErr != nil {
				return a.cfgErr
			}
			if err := a.cfg.Unset(args[0]); err != nil {
				return usageErrf("%v", err)
			}
			if err := a.cfg.Save(a.cfgPath); err != nil {
				return withCode(ExitUsage, fmt.Errorf("保存配置失败：%w", err))
			}
			fmt.Fprintf(a.stdout, "已移除 %s（%s）\n", args[0], a.cfgPath)
			return nil
		},
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
