package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"dstools/internal/config"
)

func (a *App) newCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "查看或清空本地响应缓存",
		Long: `响应会缓存在配置文件旁的目录中，避免完全相同的请求被重复计费。
用 --no-cache 跳过读取缓存。`,
	}
	cmd.AddCommand(
		a.newCacheClearCmd(),
		a.newCachePathCmd(),
	)
	return cmd
}

func (a *App) newCachePathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "打印缓存目录",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			dir, err := config.CacheDir()
			if err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, dir)
			return nil
		},
	}
}

func (a *App) newCacheClearCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Short: "删除全部缓存响应",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			c := a.cacheStore()
			files, bytes, err := c.Clear()
			if err != nil {
				return withCode(ExitUsage, fmt.Errorf("清空缓存失败：%w", err))
			}
			if a.format == "json" || a.format == "jsonl" {
				fmt.Fprintf(a.stdout, `{"dir":%q,"removed":%d,"bytes":%d}`+"\n", c.Dir, files, bytes)
				return nil
			}
			fmt.Fprintf(a.stdout, "已从 %s 删除 %d 个条目（%s）\n", c.Dir, files, humanBytes(bytes))
			return nil
		},
	}
}
