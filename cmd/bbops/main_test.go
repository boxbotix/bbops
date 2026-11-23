package main

import (
	"bytes"
	"strings"
	"testing"
)

// spec: host-runtime / Host CLI version and flag entrypoint
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

	t.Run("zero args exits zero cleanly in M0.1 bootstrap mode", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(nil, &stdout, &stderr, "v0.1.0-dev", "abc1234")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		if stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("unexpected output: stdout=%q stderr=%q", stdout.String(), stderr.String())
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
}
