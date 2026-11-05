package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Build metadata, injected via -ldflags at release time.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

func (a *App) newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "显示版本信息",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if a.format == "json" || a.format == "jsonl" {
				fmt.Fprintf(a.stdout, `{"version":%q,"commit":%q,"date":%q,"go":%q,"os":%q,"arch":%q,"base_url":%q}`+"\n",
					Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH, a.baseURL)
				return nil
			}
			fmt.Fprintf(a.stdout, "ds-tools %s", Version)
			if Commit != "" {
				fmt.Fprintf(a.stdout, " (%s)", Commit)
			}
			if Date != "" {
				fmt.Fprintf(a.stdout, ", built %s", Date)
			}
			fmt.Fprintf(a.stdout, " %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}
