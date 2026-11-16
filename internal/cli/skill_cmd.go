package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"dstools/internal/config"
	"dstools/internal/skill"
)

const skillFileName = "SKILL.md"

// skillDir is one Agent skills directory ds-tools knows about.
type skillDir struct {
	Agent string
	Dir   string
}

func (a *App) newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "安装或检查内置的 Agent Skill（SKILL.md）",
		Long: `二进制内嵌了 SKILL.md，它是给 Agent 的决策契约：何时用 search 何时用
fetch、字段投影、退出码动作。参数语法以各命令 --help 为准。

安装到 Agent 的 skills 目录后，本地任意 Agent 都能按需加载。`,
	}
	cmd.AddCommand(a.newSkillInstallCmd(), a.newSkillStatusCmd())
	return cmd
}

// skillDirs lists the Agent skill directories ds-tools knows about.
func skillDirs() ([]skillDir, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("获取用户主目录失败：%w", err)
	}
	return []skillDir{
		{"claude", filepath.Join(home, ".claude", "skills")},
		{"pi", filepath.Join(home, ".pi", "agent", "skills")},
		{"agents", filepath.Join(home, ".agents", "skills")},
	}, nil
}

// envSkillDir is the explicit override shared by install and status.
func envSkillDir() string {
	return expandHome(strings.TrimSpace(os.Getenv(config.EnvSkill)))
}

func (a *App) newSkillInstallCmd() *cobra.Command {
	var claude, pi, agents, all bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "把 SKILL.md 写入 Agent skills 目录",
		Long: `把内嵌的 SKILL.md 写入 <skills-dir>/ds-tools/SKILL.md，已存在的文件会被覆盖。

用 --claude、--pi、--agents 指定目录，或用 --all 写入全部三个目录：
~/.claude/skills、~/.pi/agent/skills、~/.agents/skills。
` + config.EnvSkill + ` 可在不使用上述选项时指定任意目录。`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if all && (claude || pi || agents) {
				return usageErrf("--all 不能与 --claude、--pi、--agents 同时使用")
			}
			bases, err := installDirs(claude, pi, agents, all)
			if err != nil {
				return usageErrf("%v", err)
			}
			results := make([]installResult, 0, len(bases))
			for _, base := range bases {
				file := filepath.Join(base, skill.Name, skillFileName)
				action, err := writeSkill(file)
				if err != nil {
					return err
				}
				results = append(results, installResult{Path: file, Action: action})
			}
			return a.printInstallResult(results)
		},
	}
	cmd.Flags().BoolVar(&claude, "claude", false, "安装到 ~/.claude/skills")
	cmd.Flags().BoolVar(&pi, "pi", false, "安装到 ~/.pi/agent/skills")
	cmd.Flags().BoolVar(&agents, "agents", false, "安装到 ~/.agents/skills")
	cmd.Flags().BoolVar(&all, "all", false, "安装到全部三个目录")
	return cmd
}

// installDirs resolves the base skills directories for one install invocation.
func installDirs(claude, pi, agents, all bool) ([]string, error) {
	if all || claude || pi || agents {
		dirs, err := skillDirs()
		if err != nil {
			return nil, err
		}
		var out []string
		for _, d := range dirs {
			selected := all ||
				(d.Agent == "claude" && claude) ||
				(d.Agent == "pi" && pi) ||
				(d.Agent == "agents" && agents)
			if selected {
				out = append(out, d.Dir)
			}
		}
		return out, nil
	}
	if env := envSkillDir(); env != "" {
		return []string{env}, nil
	}
	return nil, errors.New("请用 --claude、--pi、--agents 或 --all 指定安装目录")
}

// writeSkill installs SKILL.md and reports what changed on disk.
func writeSkill(file string) (string, error) {
	action := "installed"
	if b, err := os.ReadFile(file); err == nil {
		if bytes.Equal(b, []byte(skill.Markdown)) {
			return "unchanged", nil
		}
		action = "updated"
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", withCode(ExitUsage, fmt.Errorf("读取 %s 失败：%w", file, err))
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", withCode(ExitUsage, fmt.Errorf("创建目录 %s 失败：%w", filepath.Dir(file), err))
	}
	if err := os.WriteFile(file, []byte(skill.Markdown), 0o644); err != nil {
		return "", withCode(ExitUsage, fmt.Errorf("写入 %s 失败：%w", file, err))
	}
	return action, nil
}

func (a *App) printInstallResult(results []installResult) error {
	switch a.format {
	case "json":
		return a.printJSON(installOutput{Skill: skill.Name, Results: results})
	case "jsonl":
		for _, result := range results {
			line := installLine{Skill: skill.Name, Path: result.Path, Action: result.Action}
			if err := a.printJSON(line); err != nil {
				return err
			}
		}
		return nil
	default:
		for _, result := range results {
			fmt.Fprintf(a.stdout, "%s %s\n", installActionLabel(result.Action), result.Path)
		}
		return nil
	}
}

// installActionLabel renders an install action for human-readable output.
func installActionLabel(action string) string {
	switch action {
	case "installed":
		return "已安装"
	case "updated":
		return "已更新"
	default:
		return "已是最新"
	}
}

// installOutput is the JSON document emitted for an install run.
type installOutput struct {
	Skill   string          `json:"skill"`
	Results []installResult `json:"results"`
}

// installResult is one target's outcome, shared by JSON and JSONL output.
type installResult struct {
	Path   string `json:"path"`
	Action string `json:"action"`
}

// installLine is one JSONL record for an install run.
type installLine struct {
	Skill  string `json:"skill"`
	Path   string `json:"path"`
	Action string `json:"action"`
}

func (a *App) newSkillStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "检查各 Agent skills 目录中的安装状态",
		Long: `逐个检查已知 Agent skills 目录中的 SKILL.md 是否安装、是否与当前二进制内嵌的内容一致。

state 取值：ok（逐字节一致）、changed（内容不同，可能是旧版本或本地修改）、
missing（未安装）、unreadable（无法读取）。

设置 ` + config.EnvSkill + ` 时只检查该目录。发现 changed 或缺失不算命令失败，退出码始终为 0。`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			targets, err := statusTargets()
			if err != nil {
				return withCode(ExitUsage, err)
			}
			expected := sha256.Sum256([]byte(skill.Markdown))
			out := skillStatusOutput{Skill: skill.Name, Version: Version}
			for _, target := range targets {
				out.Targets = append(out.Targets, inspectSkill(target, expected))
			}

			switch a.format {
			case "json":
				return a.printJSON(out)
			case "jsonl":
				for _, target := range out.Targets {
					if err := a.printJSON(target); err != nil {
						return err
					}
				}
				return nil
			}

			fmt.Fprintf(a.stdout, "skill      %s\n", out.Skill)
			fmt.Fprintf(a.stdout, "version    %s\n\n", out.Version)
			changed := false
			tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "agent\tstate\tpath")
			for _, target := range out.Targets {
				if target.State == "changed" {
					changed = true
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", target.Agent, target.State, target.Path)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if changed {
				fmt.Fprint(a.stdout, "\n提示：changed 表示安装内容与当前二进制不一致；运行\n"+
					"      ds-tools skill install --all\n"+
					"      可覆盖同步（会覆盖本地修改）。\n")
			}
			return nil
		},
	}
}

// statusTargets checks the override directory, or every known Agent directory.
func statusTargets() ([]skillDir, error) {
	if env := envSkillDir(); env != "" {
		return []skillDir{{Agent: "env", Dir: env}}, nil
	}
	return skillDirs()
}

// skillStatusOutput is the JSON document emitted by skill status.
type skillStatusOutput struct {
	Skill   string              `json:"skill"`
	Version string              `json:"version"`
	Targets []skillTargetStatus `json:"targets"`
}

// skillTargetStatus is one directory's installation state.
type skillTargetStatus struct {
	Agent           string `json:"agent"`
	Path            string `json:"path"`
	State           string `json:"state"`
	InstalledDigest string `json:"installed_digest"`
	ExpectedDigest  string `json:"expected_digest"`
}

func inspectSkill(target skillDir, expected [sha256.Size]byte) skillTargetStatus {
	path := filepath.Join(target.Dir, skill.Name, skillFileName)
	status := skillTargetStatus{
		Agent:          target.Agent,
		Path:           path,
		State:          "missing",
		ExpectedDigest: hex.EncodeToString(expected[:4]),
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			status.State = "unreadable"
		}
		return status
	}
	sum := sha256.Sum256(b)
	status.InstalledDigest = hex.EncodeToString(sum[:4])
	if sum == expected {
		status.State = "ok"
	} else {
		status.State = "changed"
	}
	return status
}

// printJSON writes one JSON document followed by a newline.
func (a *App) printJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s\n", b)
	return nil
}

func expandHome(p string) string {
	if len(p) > 1 && p[0] == '~' && (p[1] == '/' || p[1] == filepath.Separator) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
