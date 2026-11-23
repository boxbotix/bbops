package main

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/boxbotix/bbops/internal/host"
)

// spec: host-runtime / Host CLI version and flag entrypoint
// spec: host-runtime / Embedded NATS JetStream and SemEngine SKG pipeline lifecycle
func TestRunCLIEntrypoint(t *testing.T) {
	t.Run("version flag prints deterministic banner and exits zero", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"-version"}, &stdout, &stderr, "v0.1.0-dev", "abc1234")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr.String())
		}
		if got, want := stdout.String(), "bbops v0.1.0-dev (abc1234)\n"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
	})

	t.Run("validate flag checks embedded SemEngine composition offline and exits zero", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"-validate"}, &stdout, &stderr, "v0.1.0-dev", "abc1234")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr.String())
		}
		if got, want := stdout.String(), "bbops configuration valid\n"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
	})

	t.Run("validate flag rejects privileged port override with exit code one", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"-validate", "-http-port", "80"}, &stdout, &stderr, "v0.1.0-dev", "abc1234")
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		if !strings.Contains(stderr.String(), "http-port") {
			t.Fatalf("stderr = %q, want diagnostic naming http-port", stderr.String())
		}
	})

	t.Run("unknown flag writes diagnostic to stderr and exits two", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"-bogus"}, &stdout, &stderr, "v0.1.0-dev", "abc1234")
		if code != 2 {
			t.Fatalf("exit code = %d, want 2", code)
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		if !strings.Contains(stderr.String(), "bogus") {
			t.Fatalf("stderr = %q, want diagnostic naming unknown flag", stderr.String())
		}
	})

	t.Run("unexpected positional argument writes diagnostic to stderr and exits two", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"extra"}, &stdout, &stderr, "v0.1.0-dev", "abc1234")
		if code != 2 {
			t.Fatalf("exit code = %d, want 2", code)
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		if !strings.Contains(stderr.String(), "extra") {
			t.Fatalf("stderr = %q, want diagnostic naming unexpected argument", stderr.String())
		}
	})

	t.Run("daemon mode boots embedded NATS and SemEngine until context cancels", func(t *testing.T) {
		ports, err := host.AllocateLoopbackPorts(4)
		if err != nil {
			t.Fatalf("allocate loopback ports: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var stdout, stderr bytes.Buffer
		done := make(chan int, 1)
		go func() {
			done <- runContext(ctx, []string{
				"-data-dir", t.TempDir(),
				"-ui-dir", t.TempDir(),
				"-bind-host", "127.0.0.1",
				"-http-port", strconv.Itoa(ports[0]),
				"-telemetry-ws-port", strconv.Itoa(ports[1]),
				"-graph-ws-port", strconv.Itoa(ports[2]),
				"-metrics-port", strconv.Itoa(ports[3]),
			}, &stdout, &stderr, "v0.1.0-dev", "abc1234")
		}()

		readyURL := "http://127.0.0.1:" + strconv.Itoa(ports[0]) + "/readyz"
		deadline := time.Now().Add(15 * time.Second)
		ready := false
		for time.Now().Before(deadline) {
			select {
			case code := <-done:
				t.Fatalf("CLI daemon exited early with code %d (stderr=%s)", code, stderr.String())
			default:
			}
			resp, reqErr := http.Get(readyURL) //nolint:noctx
			if reqErr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					ready = true
					break
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		if !ready {
			cancel()
			t.Fatalf("CLI daemon never reached /readyz 200 (stderr=%s)", stderr.String())
		}

		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr=%s)", code, stderr.String())
			}
		case <-time.After(15 * time.Second):
			t.Fatal("CLI daemon did not stop after context cancellation")
		}
	})
}
