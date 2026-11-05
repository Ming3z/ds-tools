package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"dstools/internal/config"
	"dstools/internal/skill"
)

func (a *App) newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "输出或安装内置的 Agent Skill（SKILL.md）",
		Long: `二进制内嵌了 SKILL.md，它是给 Agent 的权威使用契约：何时用 search 何时用
fetch、字段投影、退出码语义与计费规则。

安装到 Agent 的 skills 目录后，本地任意 Agent 都能按需加载。`,
	}
	cmd.AddCommand(a.newSkillPrintCmd(), a.newSkillInstallCmd())
	return cmd
}

func (a *App) newSkillPrintCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "print",
		Short: "输出内嵌的 SKILL.md",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprint(a.stdout, skill.Markdown)
			return nil
		},
	}
}

func (a *App) newSkillInstallCmd() *cobra.Command {
	var dir string
	var force bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "把 SKILL.md 写入 Agent 的 skills 目录",
		Long: `把内嵌的 skill 复制到 <skills-dir>/ds-tools/SKILL.md。

默认目录：优先取 $DS_TOOLS_SKILL_DIR；否则取 ~/.claude/skills、
~/.pi/agent/skills、~/.agents/skills 中第一个存在的，都��存在时回退到
~/.claude/skills。`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			base := dir
			if base == "" {
				d, err := defaultSkillDir()
				if err != nil {
					return withCode(ExitUsage, err)
				}
				base = d
			}
			base = expandHome(base)
			target := filepath.Join(base, skill.Name)
			path := filepath.Join(target, "SKILL.md")

			if _, err := os.Stat(path); err == nil && !force {
				return usageErrf("%s 已存在，覆盖请加 --force", path)
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return withCode(ExitUsage, fmt.Errorf("创建目录 %s 失败：%w", target, err))
			}
			if err := os.WriteFile(path, []byte(skill.Markdown), 0o644); err != nil {
				return withCode(ExitUsage, fmt.Errorf("写入 %s 失败：%w", path, err))
			}
			if a.format == "json" || a.format == "jsonl" {
				fmt.Fprintf(a.stdout, `{"skill":"%s","path":%q}`+"\n", skill.Name, path)
				return nil
			}
			fmt.Fprintf(a.stdout, "已安装 %s\n", path)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "安装到的 skills 目录")
	cmd.Flags().BoolVar(&force, "force", false, "覆盖已存在的 SKILL.md")
	_ = cmd.MarkFlagDirname("dir")
	return cmd
}

func defaultSkillDir() (string, error) {
	if v := os.Getenv(config.EnvSkill); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户主目录失败：%w", err)
	}
	candidates := []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(home, ".pi", "agent", "skills"),
		filepath.Join(home, ".agents", "skills"),
	}
	for _, c := range candidates {
		if info, statErr := os.Stat(c); statErr == nil && info.IsDir() {
			return c, nil
		}
	}
	return candidates[0], nil
}

func expandHome(p string) string {
	if len(p) > 1 && p[0] == '~' && (p[1] == '/' || p[1] == filepath.Separator) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
