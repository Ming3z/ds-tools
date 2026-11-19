// Command ds-tools wraps DeepSeek's Search, Fetch, OCR, ASR and TTS APIs for
// agents and scripts.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"dstools/internal/cli"
)

func main() {
	// A signal must not kill the process mid-write: cancel the context instead
	// so in-flight requests stop and every defer (temp-file removal, upload
	// spool cleanup, stream close) still runs. The cause records which signal
	// arrived so the CLI can exit with 130 (SIGINT) or 143 (SIGTERM).
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig, ok := <-sigs
		if !ok {
			return
		}
		// Restore the default behaviour so a second Ctrl-C kills immediately.
		signal.Stop(sigs)
		cancel(&cli.SignalError{Sig: sig})
	}()

	os.Exit(cli.ExecuteContext(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
