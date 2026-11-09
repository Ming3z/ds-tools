package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

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
		a.newConfigPathCmd(),
		a.newConfigShowCmd(),
		a.newConfigSetCmd(),
		a.newConfigUnsetCmd(),
		a.newConfigClearKeyCmd(),
	)
	return cmd
}

func (a *App) newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "打印配置文件路径",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if a.cfgErr != nil {
				return a.cfgErr
			}
			fmt.Fprintln(a.stdout, a.cfgPath)
			return nil
		},
	}
}

func (a *App) newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "显示当前生效的配置（已脱敏）",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if a.cfgErr != nil {
				return a.cfgErr
			}
			cacheDir, _ := a.cacheDir()
			rows := []kv{
				{"config_file", a.cfgPath},
				{"config_dir", filepath.Dir(a.cfgPath)},
				{"api_key", config.Mask(a.apiKey)},
				{"api_key_source", a.keySrc},
				{"base_url", a.baseURL},
				{"base_url_source", a.baseSrc},
				{"timeout", a.timeout.String()},
				{"retries", a.retries},
				{"cache_ttl", a.cacheTTL.String()},
				{"cache_dir", cacheDir},
				{"format", a.format},
			}
			if a.format == "json" || a.format == "jsonl" {
				fmt.Fprintf(a.stdout, "%s\n", mustJSON(obj(rows)))
				return nil
			}
			for _, r := range rows {
				fmt.Fprintf(a.stdout, "%-16s %v\n", r.K, r.V)
			}
			fmt.Fprintf(a.stdout, "%-16s %s\n", "config_keys", strings.Join(a.cfg.NonZeroKeys(), ","))
			return nil
		},
	}
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

func (a *App) newConfigClearKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear-key",
		Short: "只删除已保存的 API Key",
		Args:  noPositionalArgs,
		RunE: func(*cobra.Command, []string) error {
			if a.cfgErr != nil {
				return a.cfgErr
			}
			if a.cfg.APIKey == "" {
				fmt.Fprintln(a.stdout, "当前没有保存 Key")
				return nil
			}
			a.cfg.APIKey = ""
			if err := a.cfg.Save(a.cfgPath); err != nil {
				return withCode(ExitUsage, fmt.Errorf("保存配置失败：%w", err))
			}
			fmt.Fprintf(a.stdout, "已从 %s 删除 API Key\n", a.cfgPath)
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
