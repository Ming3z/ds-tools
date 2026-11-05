package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"dstools/internal/cache"
	"dstools/internal/config"
	"dstools/internal/deepseek"
)

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail | skip
	Detail string `json:"detail"`
}

func (a *App) newDoctorCmd() *cobra.Command {
	var probe bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "自检配置、鉴权、连通性与余额",
		Long: `执行本地诊断：配置文件、Key 来源、基础地址、鉴权、余额、缓存。

--probe 会真实调用余额接口，用于校验 Key 并读取剩余额度；该调用免费
（不属于 Search / Fetch 计费）。

退出码：0 健康（允许有警告），2 Key 无效，1 其他失败。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runDoctor(cmd.Context(), probe)
		},
	}
	cmd.Flags().BoolVar(&probe, "probe", false, "真实请求校验 Key 并读取余额")
	return cmd
}

func (a *App) runDoctor(ctx context.Context, probe bool) error {
	var checks []check
	authFailed := false

	// 1. Config file.
	if a.cfgErr != nil {
		checks = append(checks, check{"config", "fail", a.cfgErr.Error()})
	} else if _, err := os.Stat(a.cfgPath); err != nil {
		checks = append(checks, check{"config", "warn", a.cfgPath + "（尚未创建，请运行 'ds-tools init'）"})
	} else if err := config.PermWarning(a.cfgPath); err != nil {
		checks = append(checks, check{"config", "warn", err.Error()})
	} else {
		checks = append(checks, check{"config", "ok", a.cfgPath + "（0600）"})
	}

	// 2. API key.
	switch a.keySrc {
	case "none":
		checks = append(checks, check{"api_key", "fail", "未配置（请运行 'ds-tools init' 或设置 " + config.EnvAPIKey + "）"})
		authFailed = true
	default:
		checks = append(checks, check{"api_key", "ok", fmt.Sprintf("%s（来源=%s）", config.Mask(a.apiKey), a.keySrc)})
	}

	// 3. Base URL.
	checks = append(checks, check{"base_url", "ok", fmt.Sprintf("%s（来源=%s）", a.baseURL, a.baseSrc)})

	// 4. Auth + balance.
	if a.keySrc == "none" {
		checks = append(checks, check{"auth", "skip", "没有 Key 可校验"})
	} else if !probe {
		checks = append(checks, check{"auth", "skip", "未校验（加 --probe 重新运行）"})
	} else if a.cfgErr != nil {
		checks = append(checks, check{"auth", "skip", "配置有错误"})
	} else {
		client := deepseek.NewClient(a.baseURL, a.apiKey, a.timeout, a.retries)
		pctx, cancel := context.WithTimeout(ctx, a.timeout)
		bal, err := client.Balance(pctx)
		cancel()
		switch {
		case err == nil:
			checks = append(checks, check{"auth", "ok", describeBalance(bal)})
		case isAuthError(err):
			checks = append(checks, check{"auth", "fail", err.Error()})
			authFailed = true
		default:
			var ae *deepseek.APIError
			if asAPIError(err, &ae) && ae.Status == 402 {
				checks = append(checks, check{"auth", "ok", "Key 有效，但账号余额不足（402）"})
				checks = append(checks, check{"balance", "fail", "余额不足，请到 platform.deepseek.com 充值"})
			} else {
				checks = append(checks, check{"auth", "fail", err.Error()})
			}
		}
	}

	// 5. Cache.
	if dir, err := config.CacheDir(); err == nil {
		c := cache.New(dir, a.cacheTTL)
		files, bytes := c.Stats()
		checks = append(checks, check{"cache", "ok", fmt.Sprintf("%s（%d 个条目，%s，TTL %s）", dir, files, humanBytes(bytes), a.cacheTTL)})
	}

	// 6. Runtime.
	checks = append(checks, check{"runtime", "ok", fmt.Sprintf("ds-tools %s，%s %s/%s", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)})

	if a.format == "json" || a.format == "jsonl" {
		payload := struct {
			OK     bool    `json:"ok"`
			Checks []check `json:"checks"`
		}{OK: true, Checks: checks}
		for _, c := range checks {
			if c.Status == "fail" {
				payload.OK = false
			}
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "%s\n", b)
		return exitForChecks(payload.OK, authFailed)
	}

	failed := false
	for _, c := range checks {
		symbol := "✔"
		switch c.Status {
		case "warn":
			symbol = "!"
		case "fail":
			symbol = "✖"
			failed = true
		case "skip":
			symbol = "-"
		}
		line := fmt.Sprintf("%s %-10s %s", symbol, c.Name, c.Detail)
		if c.Status == "fail" {
			line = a.bold(line)
		}
		fmt.Fprintln(a.stdout, line)
	}
	return exitForChecks(!failed, authFailed)
}

func exitForChecks(ok, authFailed bool) error {
	if ok {
		return nil
	}
	if authFailed {
		return withCode(ExitAuth, fmt.Errorf("鉴权检查未通过"))
	}
	return withCode(ExitServer, fmt.Errorf("存在未通过的检查项"))
}

func asAPIError(err error, target **deepseek.APIError) bool {
	if ae, ok := err.(*deepseek.APIError); ok {
		*target = ae
		return true
	}
	return false
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
