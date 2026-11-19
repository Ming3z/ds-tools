package cli

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeTTSServer answers /v1/tools/tts with a complete WAV body.
func fakeTTSServer(t *testing.T, audio []byte) *harness {
	t.Helper()
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/tts" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(audio)
	}
	return h
}

// TestTTSPipeStdoutRequiresOutputFlag: a pipe (or any non-regular-file writer)
// must not receive audio unless the user opts in with -o -.
func TestTTSPipeStdoutRequiresOutputFlag(t *testing.T) {
	h := fakeTTSServer(t, fakeWAV(4))
	if code := h.run("tts", "hello"); code != ExitUsage {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, ExitUsage, h.err.String())
	}
	if h.hits != 0 {
		t.Fatal("refused request hit the network")
	}
	if !strings.Contains(h.err.String(), "-o -") {
		t.Fatalf("stderr does not point at -o -: %s", h.err.String())
	}

	// A real OS pipe behaves like any other non-regular-file writer.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pr.Close() }()
	h.err.Reset()
	args := []string{"--base-url", h.srv.URL, "--retries", "0", "tts", "hello"}
	if code := Execute(args, strings.NewReader(""), pw, &h.err); code != ExitUsage {
		_ = pw.Close()
		t.Fatalf("pipe exit = %d, want %d (stderr=%s)", code, ExitUsage, h.err.String())
	}
	_ = pw.Close()
	if h.hits != 0 {
		t.Fatal("refused pipe request hit the network")
	}
}

// TestTTSPipeStdoutWithExplicitDash: -o - allows piping even though stdout is
// not a regular file.
func TestTTSPipeStdoutWithExplicitDash(t *testing.T) {
	audio := fakeWAV(4)
	h := fakeTTSServer(t, audio)
	if code := h.run("tts", "hello", "-o", "-"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	if string(h.out.Bytes()) != string(audio) {
		t.Fatalf("stdout is not the raw audio (%d bytes)", h.out.Len())
	}

	// The same through a real OS pipe.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pr.Close() }()
	readDone := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(pr)
		readDone <- b
	}()
	h.err.Reset()
	args := []string{"--base-url", h.srv.URL, "--retries", "0", "tts", "hello", "-o", "-"}
	if code := Execute(args, strings.NewReader(""), pw, &h.err); code != 0 {
		_ = pw.Close()
		t.Fatalf("pipe exit = %d, stderr=%s", code, h.err.String())
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	if got := <-readDone; string(got) != string(audio) {
		t.Fatalf("pipe received %d bytes, want %d", len(got), len(audio))
	}
}

// TestTTSRegularFileStdout: `> speech.wav` keeps working without -o.
func TestTTSRegularFileStdout(t *testing.T) {
	audio := fakeWAV(4)
	h := fakeTTSServer(t, audio)
	out, err := os.Create(filepath.Join(t.TempDir(), "speech.wav"))
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--base-url", h.srv.URL, "--retries", "0", "tts", "hello"}
	if code := Execute(args, strings.NewReader(""), out, &h.err); code != 0 {
		_ = out.Close()
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(audio) {
		t.Fatalf("file is not the raw audio (%d bytes)", len(got))
	}
}

// TestSignalExitCodes pins the 128+signal mapping and keeps timeouts separate.
func TestSignalExitCodes(t *testing.T) {
	cases := []struct {
		sig  os.Signal
		want int
	}{
		{os.Interrupt, ExitSIGINT},
		{syscall.SIGTERM, ExitSIGTERM},
	}
	for _, c := range cases {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(&SignalError{Sig: c.sig})
		got, ok := interruptedExitCode(ctx)
		if !ok || got != c.want {
			t.Fatalf("%v -> %d, %v; want %d", c.sig, got, ok, c.want)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if code, ok := interruptedExitCode(ctx); ok {
		t.Fatalf("deadline mapped to interruption exit %d", code)
	}
}

// TestTTSCancelDuringStreamCleansTemp cancels mid-stream: the request must
// unwind promptly, leave no .tts-*.tmp spool file, and report SIGINT.
func TestTTSCancelDuringStreamCleansTemp(t *testing.T) {
	h := newHarness(t)
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server cannot flush")
			return
		}
		flusher.Flush()
		// Send the WAV magic, then stall mid-body forever.
		_, _ = w.Write(fakeWAV(0)[:12])
		flusher.Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "speech.wav")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	done := make(chan int, 1)
	go func() {
		args := []string{"--base-url", h.srv.URL, "--retries", "0", "tts", "hello", "-o", out}
		done <- ExecuteContext(ctx, args, strings.NewReader(""), &h.out, &h.err)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the server")
	}

	// Wait until the command spooled its temp file, so the cleanup path is
	// genuinely exercised; the stream is still stalled at this point.
	spooled := false
	for deadline := time.Now().Add(5 * time.Second); !spooled && time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".tts-") {
				spooled = true
				break
			}
		}
	}
	if !spooled {
		t.Fatal("tts temp file never appeared; the stream never started")
	}

	cancel(&SignalError{Sig: os.Interrupt})

	var code int
	select {
	case code = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ExecuteContext did not return after cancellation")
	}
	if code != ExitSIGINT {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, ExitSIGINT, h.err.String())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("partial audio file was left behind: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tts-") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}
