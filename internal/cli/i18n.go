package cli

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
)

// zhUsageTemplate and zhHelpTemplate replace cobra's default English templates.
// Children inherit them from the root command.
const zhUsageTemplate = `用法：{{if and .Runnable (not .HasAvailableSubCommands)}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

别名：
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

示例：
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}

可用命令：{{commandList .}}{{end}}{{if .HasAvailableLocalFlags}}

选项：
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

全局选项：
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

更多帮助主题：{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

使用 "{{.CommandPath}} [command] --help" 查看某条命令的详细用法。{{end}}
`

const zhHelpTemplate = `{{with (or .Long .Short)}}{{. | trimTrailingWhitespaces}}

{{end}}{{if or .Runnable .HasSubCommands}}{{.UsageString}}{{end}}`

// localizeTree makes every cobra-generated string Chinese: templates, the
// built-in help/version flags and the auto-generated help command.
func localizeTree(root *cobra.Command) {
	root.SetUsageTemplate(zhUsageTemplate)
	root.SetHelpTemplate(zhHelpTemplate)
	root.SetUsageFunc(zhUsageFunc)
	root.SetHelpFunc(zhHelpFunc)

	// helpCommand 已由 SetHelpCommand 指定为本包的中文实现，这里提前挂上，
	// 好让下面的遍历把它自带的 --help 选项也一起本地化。
	root.InitDefaultHelpCmd()

	walkCommands(root, func(c *cobra.Command) {
		c.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
			return usageErrf("%s", translateFlagError(err))
		})
		c.InitDefaultHelpFlag()
		if f := c.Flags().Lookup("help"); f != nil {
			f.Usage = "显示此命令的帮助信息"
		}
		if c.Name() == "help" {
			c.Short = "显示任何命令的帮助信息"
		}
	})

	root.InitDefaultVersionFlag()
	if f := root.Flags().Lookup("version"); f != nil {
		f.Usage = "显示版本信息"
	}
}

func walkCommands(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, child := range c.Commands() {
		walkCommands(child, fn)
	}
}

// translateFlagError rewrites pflag's English flag errors.
func translateFlagError(err error) string {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "unknown flag: "):
		return "未知选项 " + strings.TrimPrefix(msg, "unknown flag: ")
	case strings.HasPrefix(msg, "unknown shorthand flag: "):
		rest := strings.TrimPrefix(msg, "unknown shorthand flag: ")
		if i := strings.Index(rest, " in "); i > 0 {
			return "未知短选项 " + rest[i+len(" in "):]
		}
		return "未知短选项 " + rest
	case strings.HasPrefix(msg, "flag needs an argument: "):
		name := strings.Trim(strings.TrimPrefix(msg, "flag needs an argument: "), `"`)
		return "选项 " + name + " 缺少参数值"
	case strings.HasPrefix(msg, "invalid argument "):
		rest := strings.TrimPrefix(msg, "invalid argument ")
		if i := strings.Index(rest, " for "); i > 0 {
			value := rest[:i]
			tail := rest[i+len(" for "):]
			if j := strings.Index(tail, " flag:"); j > 0 {
				return "选项 " + tail[:j] + " 的参数值 " + value + " 无效"
			}
		}
		return "参数值无效：" + rest
	}
	return msg
}

// translateCobraError rewrites cobra's own error strings.
func translateCobraError(err error) error {
	msg := err.Error()
	if strings.HasPrefix(msg, "unknown command ") {
		name := ""
		if i := strings.Index(msg, `"`); i >= 0 {
			if j := strings.Index(msg[i+1:], `"`); j >= 0 {
				name = msg[i+1 : i+1+j]
			}
		}
		return usageErrf("未知命令 %q", name)
	}
	return err
}

// noPositionalArgs rejects any positional argument.
func noPositionalArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageErrf("命令 %q 不接受位置参数，却收到 %d 个", cmd.CommandPath(), len(args))
	}
	return nil
}

// exactPositionalArgs requires exactly n positional arguments.
func exactPositionalArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return usageErrf("命令 %q 需要 %d 个参数，却收到 %d 个", cmd.CommandPath(), n, len(args))
		}
		return nil
	}
}

// rootArgsZh rejects unknown subcommands with a Chinese message and exit code 4.
func rootArgsZh(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return usageErrf("未知命令 %q", args[0])
}

// newHelpCmd replaces cobra's built-in help command, whose "Unknown help topic"
// message is English and cannot be templated.
func newHelpCmd(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:     "help [命令]",
		Short:   "显示任何命令的帮助信息",
		Long:    "帮助提供任何命令的详细用法。",
		Example: "  ds-tools help\n  ds-tools help search",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 {
				return usageErrf("help 最多接受一个命令名")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				cmd.Root().Help()
				return nil
			}
			target, _, err := root.Find(args)
			if err != nil || target == nil || target == root {
				return usageErrf("没有 %q 这个命令", args[0])
			}
			target.InitDefaultHelpFlag()
			target.Help()
			return nil
		},
	}
}

// templateFuncs mirrors the helpers cobra registers for its own templates.
var templateFuncs = template.FuncMap{
	"rpad":                    func(s string, padding int) string { return fmt.Sprintf("%-*s", padding, s) },
	"trimTrailingWhitespaces": func(s string) string { return strings.TrimRight(s, " \t\n\r") },
	"commandList":             commandList,
}

// helpCommandOrder fixes the root help listing. Each inner slice is one block,
// rendered with a blank line between blocks; unknown commands fall through to
// a final alphabetical block so adding a command never hides it.
var helpCommandOrder = [][]string{
	{"fetch", "search", "ocr", "asr", "tts"},
	{"init", "config", "doctor"},
	{"cache", "skill"},
	{"help", "version"},
}

// commandList renders the "可用命令" block. The root uses helpCommandOrder;
// subcommands keep cobra's alphabetical order.
func commandList(c *cobra.Command) string {
	var cmds []*cobra.Command
	for _, sub := range c.Commands() {
		if sub.IsAvailableCommand() || sub.Name() == "help" {
			cmds = append(cmds, sub)
		}
	}
	if len(cmds) == 0 {
		return ""
	}

	line := func(sub *cobra.Command) string {
		return fmt.Sprintf("  %-*s %s", c.NamePadding(), sub.Name(), sub.Short)
	}
	if c.Parent() != nil {
		lines := make([]string, 0, len(cmds))
		for _, sub := range cmds {
			lines = append(lines, line(sub))
		}
		return "\n" + strings.Join(lines, "\n")
	}

	byName := make(map[string]*cobra.Command, len(cmds))
	for _, sub := range cmds {
		byName[sub.Name()] = sub
	}
	seen := make(map[string]bool, len(cmds))
	var groups []string
	for _, names := range helpCommandOrder {
		var lines []string
		for _, name := range names {
			if sub := byName[name]; sub != nil {
				lines = append(lines, line(sub))
				seen[name] = true
			}
		}
		if len(lines) > 0 {
			groups = append(groups, strings.Join(lines, "\n"))
		}
	}
	var rest []string
	for _, sub := range cmds {
		if !seen[sub.Name()] {
			rest = append(rest, line(sub))
		}
	}
	if len(rest) > 0 {
		groups = append(groups, strings.Join(rest, "\n"))
	}
	return "\n" + strings.Join(groups, "\n\n")
}

var defaultMarker = regexp.MustCompile(` \(default (.+)\)$`)

// localizeDefaults rewrites pflag's hardcoded ` (default X)` suffix.
func localizeDefaults(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if m := defaultMarker.FindStringSubmatch(line); m != nil {
			lines[i] = defaultMarker.ReplaceAllString(line, "（默认 "+strings.ReplaceAll(m[1], "$", "$$")+"）")
		}
	}
	return strings.Join(lines, "\n")
}

func renderTemplate(w io.Writer, c *cobra.Command, name, text string) error {
	tmpl, err := template.New(name).Funcs(templateFuncs).Parse(text)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, c); err != nil {
		return err
	}
	_, err = io.WriteString(w, localizeDefaults(buf.String()))
	return err
}

func zhUsageFunc(c *cobra.Command) error {
	return renderTemplate(c.OutOrStderr(), c, "usage", c.UsageTemplate())
}

func zhHelpFunc(c *cobra.Command, _ []string) {
	if err := renderTemplate(c.OutOrStdout(), c, "help", c.HelpTemplate()); err != nil {
		fmt.Fprintln(c.ErrOrStderr(), err)
	}
}
