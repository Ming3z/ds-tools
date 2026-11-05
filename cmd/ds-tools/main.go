// Command ds-tools wraps DeepSeek's Search and Fetch APIs for agents and scripts.
package main

import (
	"os"

	"dstools/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
