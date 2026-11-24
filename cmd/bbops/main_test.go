package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boxbotix/bbops/configs"
	"github.com/boxbotix/bbops/internal/host"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// spec: host-runtime / Host CLI version and flag entrypoint
// spec: host-runtime / Embedded NATS JetStream and SemEngine SKG pipeline lifecycle
// spec: host-runtime / ADR-115 GraphQL front door and dual output/websocket streaming planes
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

		stdout.Reset()
		stderr.Reset()
		code = run([]string{"-validate", "-mavlink-udp-port", "80"}, &stdout, &stderr, "v0.1.0-dev", "abc1234")
		if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "mavlink-udp-port") {
			t.Fatalf("mavlink-udp-port=80: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})

	t.Run("validate flag rejects disabled mandatory component-manager or missing graph-gateway", func(t *testing.T) {
		disabledCompMgr := writeMutatedFlowFile(t, func(doc map[string]any) {
			services := doc["services"].(map[string]any)
			services["component-manager"] = map[string]any{"enabled": false, "config": map[string]any{}}
		})
		var stdout, stderr bytes.Buffer
		code := run([]string{"-validate", "-config", disabledCompMgr}, &stdout, &stderr, "v0.1.0-dev", "abc1234")
		if code != 1 {
			t.Fatalf("exit code = %d, want 1 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		if !strings.Contains(stderr.String(), "mandatory service component-manager cannot be disabled") {
			t.Fatalf("stderr = %q, want mandatory service diagnostic", stderr.String())
		}

		disabledGateway := writeMutatedFlowFile(t, func(doc map[string]any) {
			components := doc["components"].(map[string]any)
			gw := components["graph-gateway"].(map[string]any)
			gw["enabled"] = false
		})
		stdout.Reset()
		stderr.Reset()
		code = run([]string{"-validate", "-config", disabledGateway}, &stdout, &stderr, "v0.1.0-dev", "abc1234")
		if code != 1 {
			t.Fatalf("exit code = %d, want 1 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
		if !strings.Contains(stderr.String(), "graph-gateway") {
			t.Fatalf("stderr = %q, want graph-gateway diagnostic", stderr.String())
		}
	})

	t.Run("validate and boot accept explicit legacy M0.2 flow file in memory while preserving operator settings and file on disk", func(t *testing.T) {
		legacyPath := writeMutatedFlowFile(t, func(doc map[string]any) {
			components := doc["components"].(map[string]any)
			delete(components, host.MAVLinkIngestComponent)
			telWS := components[host.TelemetryWSComponent].(map[string]any)
			telWS["config"].(map[string]any)["path"] = "/legacy-file-ws"
		})
		diskBefore, err := os.ReadFile(legacyPath)
		if err != nil {
			t.Fatalf("ReadFile before: %v", err)
		}

		var valStdout, valStderr bytes.Buffer
		if code := run([]string{"-validate", "-config", legacyPath}, &valStdout, &valStderr, "v0.3.0", "abc1234"); code != 0 || valStdout.String() != "bbops configuration valid\n" {
			t.Fatalf("legacy -validate: code=%d stdout=%q stderr=%q", code, valStdout.String(), valStderr.String())
		}

		// Customized valid legacy files (bumped operator version 1.0.1, and extra disabled graph-index instance) also pass -validate.
		bumpedLegacyPath := writeMutatedFlowFile(t, func(doc map[string]any) {
			doc["version"] = "1.0.1"
			components := doc["components"].(map[string]any)
			delete(components, host.MAVLinkIngestComponent)
		})
		valStdout.Reset()
		valStderr.Reset()
		if code := run([]string{"-validate", "-config", bumpedLegacyPath}, &valStdout, &valStderr, "v0.3.0", "abc1234"); code != 0 || valStdout.String() != "bbops configuration valid\n" {
			t.Fatalf("bumped 1.0.1 legacy -validate: code=%d stdout=%q stderr=%q", code, valStdout.String(), valStderr.String())
		}

		extraDisabledLegacyPath := writeMutatedFlowFile(t, func(doc map[string]any) {
			components := doc["components"].(map[string]any)
			delete(components, host.MAVLinkIngestComponent)
			components["graph-index-canary"] = map[string]any{
				"type":    "processor",
				"name":    "graph-index",
				"enabled": false,
				"config":  map[string]any{},
			}
		})
		valStdout.Reset()
		valStderr.Reset()
		if code := run([]string{"-validate", "-config", extraDisabledLegacyPath}, &valStdout, &valStderr, "v0.3.0", "abc1234"); code != 0 || valStdout.String() != "bbops configuration valid\n" {
			t.Fatalf("extra disabled legacy -validate: code=%d stdout=%q stderr=%q", code, valStdout.String(), valStderr.String())
		}

		// Malformed legacy flows (missing graph-ingest, or rogue enabled component) still fail closed.
		malformedLegacyPath := writeMutatedFlowFile(t, func(doc map[string]any) {
			components := doc["components"].(map[string]any)
			delete(components, host.MAVLinkIngestComponent)
			delete(components, "graph-ingest")
		})
		var badStdout, badStderr bytes.Buffer
		if code := run([]string{"-validate", "-config", malformedLegacyPath}, &badStdout, &badStderr, "v0.3.0", "abc1234"); code != 1 || !strings.Contains(badStderr.String(), "graph-ingest") {
			t.Fatalf("malformed legacy -validate: code=%d stdout=%q stderr=%q, want exit 1 naming graph-ingest", code, badStdout.String(), badStderr.String())
		}

		rogueLegacyPath := writeMutatedFlowFile(t, func(doc map[string]any) {
			components := doc["components"].(map[string]any)
			delete(components, host.MAVLinkIngestComponent)
			components["rogue-extra"] = map[string]any{"type": "input", "name": "websocket", "enabled": true, "config": map[string]any{}}
		})
		badStdout.Reset()
		badStderr.Reset()
		if code := run([]string{"-validate", "-config", rogueLegacyPath}, &badStdout, &badStderr, "v0.3.0", "abc1234"); code != 1 || !strings.Contains(badStderr.String(), "rogue-extra") {
			t.Fatalf("rogue legacy -validate: code=%d stdout=%q stderr=%q, want exit 1 naming rogue-extra", code, badStdout.String(), badStderr.String())
		}

		ports, err := host.AllocateLoopbackPorts(4)
		if err != nil {
			t.Fatalf("AllocateLoopbackPorts: %v", err)
		}
		udpPorts, err := host.AllocateLoopbackUDPPorts(1)
		if err != nil {
			t.Fatalf("AllocateLoopbackUDPPorts: %v", err)
		}
		uiDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(uiDir, "index.html"), []byte("<html>legacy-cfg</html>"), 0o600); err != nil {
			t.Fatalf("write index.html: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var runStdout, runStderr syncBuffer
		done := make(chan int, 1)
		go func() {
			done <- runContext(ctx, []string{
				"-config", legacyPath,
				"-data-dir", filepath.Join(t.TempDir(), "data"),
				"-ui-dir", uiDir,
				"-bind-host", "127.0.0.1",
				"-http-port", strconv.Itoa(ports[0]),
				"-telemetry-ws-port", strconv.Itoa(ports[1]),
				"-graph-ws-port", strconv.Itoa(ports[2]),
				"-metrics-port", strconv.Itoa(ports[3]),
				"-mavlink-udp-port", strconv.Itoa(udpPorts[0]),
			}, &runStdout, &runStderr, "v0.3.0", "abc1234")
		}()

		deadline := time.Now().Add(15 * time.Second)
		ready := false
		for time.Now().Before(deadline) {
			if strings.Contains(runStderr.String(), "bbops host ready") {
				ready = true
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		if !ready {
			cancel()
			<-done
			t.Fatalf("legacy -config daemon did not reach ready; stderr=%s", runStderr.String())
		}
		if !strings.Contains(runStderr.String(), "/legacy-file-ws") ||
			!strings.Contains(runStderr.String(), "mavlink_udp=127.0.0.1:"+strconv.Itoa(udpPorts[0])) {
			cancel()
			<-done
			t.Fatalf("legacy -config ready log missing preserved /legacy-file-ws or mavlink_udp: %s", runStderr.String())
		}
		resp, getErr := http.Get("http://127.0.0.1:" + strconv.Itoa(ports[0]) + "/readyz") //nolint:noctx
		if getErr != nil {
			cancel()
			<-done
			t.Fatalf("GET /readyz: %v", getErr)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			cancel()
			<-done
			t.Fatalf("/readyz status = %d, want 200", resp.StatusCode)
		}
		cancel()
		if exitCode := <-done; exitCode != 0 {
			t.Fatalf("legacy -config daemon exit = %d, want 0", exitCode)
		}

		diskAfter, err := os.ReadFile(legacyPath)
		if err != nil {
			t.Fatalf("ReadFile after: %v", err)
		}
		if !bytes.Equal(diskAfter, diskBefore) {
			t.Fatal("legacy flow file on disk was modified")
		}
	})

	t.Run("validate flag accepts null service configs with CLI port overrides without panicking", func(t *testing.T) {
		nullSvcPath := writeMutatedFlowFile(t, func(doc map[string]any) {
			services := doc["services"].(map[string]any)
			services["service-manager"] = map[string]any{"enabled": true, "config": nil}
			services["metrics"] = map[string]any{"enabled": true, "config": nil}
		})
		var stdout, stderr bytes.Buffer
		code := run([]string{
			"-validate",
			"-config", nullSvcPath,
			"-http-port", "18080",
			"-metrics-port", "19090",
		}, &stdout, &stderr, "v0.1.0-dev", "abc1234")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr.String())
		}
		if got, want := stdout.String(), "bbops configuration valid\n"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	})

	t.Run("validate and startup share identical decision and diagnostic when override masks base-config error", func(t *testing.T) {
		maskedCollisionPath := writeMutatedFlowFile(t, func(doc map[string]any) {
			services := doc["services"].(map[string]any)
			metrics := services["metrics"].(map[string]any)
			metricsCfg := metrics["config"].(map[string]any)
			metricsCfg["port"] = 8080
		})
		dataDir := filepath.Join(t.TempDir(), "data")

		var valStdout, valStderr bytes.Buffer
		valCode := run([]string{
			"-validate",
			"-config", maskedCollisionPath,
			"-metrics-port", "19090",
		}, &valStdout, &valStderr, "v0.1.0-dev", "abc1234")

		startCtx, startCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer startCancel()
		var startStdout, startStderr bytes.Buffer
		startCode := runContext(startCtx, []string{
			"-config", maskedCollisionPath,
			"-data-dir", dataDir,
			"-metrics-port", "19090",
		}, &startStdout, &startStderr, "v0.1.0-dev", "abc1234")

		if valCode != 1 || startCode != 1 {
			t.Fatalf("codes = (validate=%d, startup=%d), want (1, 1) (valStderr=%q startStderr=%q)",
				valCode, startCode, valStderr.String(), startStderr.String())
		}
		if valStdout.Len() != 0 || startStdout.Len() != 0 {
			t.Fatalf("stdout = (validate=%q, startup=%q), want both empty", valStdout.String(), startStdout.String())
		}
		valDiag := strings.TrimPrefix(valStderr.String(), "validation error: ")
		startDiag := strings.TrimPrefix(startStderr.String(), "startup failed: ")
		if valDiag != startDiag {
			t.Fatalf("underlying diagnostics diverged: validate=%q vs startup=%q", valDiag, startDiag)
		}
		if !strings.Contains(valDiag, "port 8080 claimed by both service-manager and metrics") {
			t.Fatalf("diagnostic = %q, want base-config port collision message", valDiag)
		}
		if _, statErr := os.Stat(dataDir); !os.IsNotExist(statErr) {
			t.Fatalf("startup touched dataDir %q despite offline base-config rejection (statErr=%v)", dataDir, statErr)
		}
	})

	t.Run("validate accepts SemEngine day-duration ENTITY max_age and rejects zero or negative durations", func(t *testing.T) {
		oneDayPath := writeMutatedFlowFile(t, func(doc map[string]any) {
			streams := doc["streams"].(map[string]any)
			entity := streams["ENTITY"].(map[string]any)
			entity["max_age"] = "1d"
		})
		var stdout, stderr bytes.Buffer
		code := run([]string{"-validate", "-config", oneDayPath}, &stdout, &stderr, "v0.1.0-dev", "abc1234")
		if code != 0 || stdout.String() != "bbops configuration valid\n" {
			t.Fatalf("1d max_age: code=%d stdout=%q stderr=%q, want 0 valid", code, stdout.String(), stderr.String())
		}

		for _, badMaxAge := range []string{"0d", "-1d", "bogus"} {
			badPath := writeMutatedFlowFile(t, func(doc map[string]any) {
				streams := doc["streams"].(map[string]any)
				entity := streams["ENTITY"].(map[string]any)
				entity["max_age"] = badMaxAge
			})
			stdout.Reset()
			stderr.Reset()
			code = run([]string{"-validate", "-config", badPath}, &stdout, &stderr, "v0.1.0-dev", "abc1234")
			if code != 1 || stdout.Len() != 0 {
				t.Fatalf("max_age=%q: code=%d stdout=%q stderr=%q, want exit 1", badMaxAge, code, stdout.String(), stderr.String())
			}
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

	t.Run("daemon mode gates UI admission and ready log on actual ADR-109 readiness", func(t *testing.T) {
		ports, err := host.AllocateLoopbackPorts(4)
		if err != nil {
			t.Fatalf("allocate loopback ports: %v", err)
		}
		udpPorts, err := host.AllocateLoopbackUDPPorts(1)
		if err != nil {
			t.Fatalf("allocate loopback UDP port: %v", err)
		}
		uiDir := t.TempDir()
		const wantIndex = "<html>cli-cop</html>"
		if err := os.WriteFile(filepath.Join(uiDir, "index.html"), []byte(wantIndex), 0o600); err != nil {
			t.Fatalf("write index.html: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var stdout, stderr syncBuffer
		done := make(chan int, 1)
		go func() {
			done <- runContext(ctx, []string{
				"-data-dir", t.TempDir(),
				"-ui-dir", uiDir,
				"-bind-host", "127.0.0.1",
				"-http-port", strconv.Itoa(ports[0]),
				"-telemetry-ws-port", strconv.Itoa(ports[1]),
				"-graph-ws-port", strconv.Itoa(ports[2]),
				"-metrics-port", strconv.Itoa(ports[3]),
				"-mavlink-udp-port", strconv.Itoa(udpPorts[0]),
			}, &stdout, &stderr, "v0.1.0-dev", "abc1234")
		}()

		baseURL := "http://127.0.0.1:" + strconv.Itoa(ports[0])
		// Wait until HTTP listener is bound (/healthz 200 OK).
		deadline := time.Now().Add(10 * time.Second)
		live := false
		for time.Now().Before(deadline) {
			resp, reqErr := http.Get(baseURL + "/healthz") //nolint:noctx
			if reqErr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					live = true
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !live {
			cancel()
			t.Fatalf("CLI daemon never reached /healthz 200 (stderr=%s)", stderr.String())
		}

		// During cold-start interval before graph-websocket catches up on ENTITY_STATES,
		// /readyz is 503, / is 503 NOT READY, and "bbops host ready" must not be logged yet.
		readyResp, err := http.Get(baseURL + "/readyz") //nolint:noctx
		if err != nil {
			t.Fatalf("GET /readyz: %v", err)
		}
		_ = readyResp.Body.Close()
		if readyResp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("cold-start /readyz status = %d, want 503", readyResp.StatusCode)
		}
		rootResp, err := http.Get(baseURL + "/") //nolint:noctx
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		rootBody, _ := io.ReadAll(rootResp.Body)
		_ = rootResp.Body.Close()
		if rootResp.StatusCode != http.StatusServiceUnavailable || string(rootBody) != "NOT READY" {
			t.Fatalf("cold-start GET / = %d %q, want 503 NOT READY", rootResp.StatusCode, string(rootBody))
		}
		if strings.Contains(stderr.String(), "bbops host ready") {
			t.Fatalf("CLI logged 'bbops host ready' before /readyz reached 200: %s", stderr.String())
		}

		// Wait with a bounded check for transition to 200 READY, SPA admission, and ready log.
		deadline = time.Now().Add(15 * time.Second)
		ready := false
		for time.Now().Before(deadline) {
			select {
			case code := <-done:
				t.Fatalf("CLI daemon exited early with code %d (stderr=%s)", code, stderr.String())
			default:
			}
			resp, reqErr := http.Get(baseURL + "/readyz") //nolint:noctx
			if reqErr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK && strings.Contains(stderr.String(), "bbops host ready") {
					ready = true
					break
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		if !ready {
			cancel()
			t.Fatalf("CLI daemon never transitioned to /readyz 200 + ready log (stderr=%s)", stderr.String())
		}
		wantUDPField := "mavlink_udp=127.0.0.1:" + strconv.Itoa(udpPorts[0])
		if !strings.Contains(stderr.String(), wantUDPField) {
			t.Fatalf("expected %s in ready stderr, got:\n%s", wantUDPField, stderr.String())
		}

		rootResp2, err := http.Get(baseURL + "/") //nolint:noctx
		if err != nil {
			t.Fatalf("post-ready GET /: %v", err)
		}
		rootBody2, _ := io.ReadAll(rootResp2.Body)
		_ = rootResp2.Body.Close()
		if rootResp2.StatusCode != http.StatusOK || string(rootBody2) != wantIndex {
			t.Fatalf("post-ready GET / = %d %q, want 200 %q", rootResp2.StatusCode, string(rootBody2), wantIndex)
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

	t.Run("external nats-url credentials never leak to stderr for token or user-password auth", func(t *testing.T) {
		const (
			tokenFixture = "tok_secret_fixture_918273"
			userFixture  = "op_user_fixture_4411"
			passFixture  = "op_pass_fixture_8822"
		)

		authCases := []struct {
			name      string
			srvOpts   *natsserver.Options
			buildURL  func(hostPort string) string
			forbidden []string
		}{
			{
				name: "token-only userinfo",
				srvOpts: &natsserver.Options{
					Host:          "127.0.0.1",
					Port:          -1,
					NoLog:         true,
					NoSigs:        true,
					JetStream:     true,
					Authorization: tokenFixture,
				},
				buildURL: func(hostPort string) string {
					return "nats://" + tokenFixture + "@" + hostPort
				},
				forbidden: []string{tokenFixture},
			},
			{
				name: "username and password userinfo",
				srvOpts: &natsserver.Options{
					Host:      "127.0.0.1",
					Port:      -1,
					NoLog:     true,
					NoSigs:    true,
					JetStream: true,
					Username:  userFixture,
					Password:  passFixture,
				},
				buildURL: func(hostPort string) string {
					return "nats://" + userFixture + ":" + passFixture + "@" + hostPort
				},
				forbidden: []string{userFixture, passFixture},
			},
		}

		for _, tc := range authCases {
			t.Run(tc.name, func(t *testing.T) {
				tc.srvOpts.StoreDir = t.TempDir()
				srv, err := natsserver.NewServer(tc.srvOpts)
				if err != nil {
					t.Fatalf("new external NATS server: %v", err)
				}
				srv.Start()
				t.Cleanup(func() {
					srv.Shutdown()
					srv.WaitForShutdown()
				})
				if !srv.ReadyForConnections(10 * time.Second) {
					t.Fatal("external NATS server not ready")
				}

				hostPort := strings.TrimPrefix(srv.ClientURL(), "nats://")
				credURL := tc.buildURL(hostPort)
				wantRedacted := "nats://" + hostPort

				ports, err := host.AllocateLoopbackPorts(4)
				if err != nil {
					t.Fatalf("allocate loopback ports: %v", err)
				}
				udpPorts, err := host.AllocateLoopbackUDPPorts(1)
				if err != nil {
					t.Fatalf("allocate loopback UDP port: %v", err)
				}

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				var stdout, stderr syncBuffer
				done := make(chan int, 1)
				go func() {
					done <- runContext(ctx, []string{
						"-nats-url", credURL,
						"-ui-dir", t.TempDir(),
						"-bind-host", "127.0.0.1",
						"-http-port", strconv.Itoa(ports[0]),
						"-telemetry-ws-port", strconv.Itoa(ports[1]),
						"-graph-ws-port", strconv.Itoa(ports[2]),
						"-metrics-port", strconv.Itoa(ports[3]),
						"-mavlink-udp-port", strconv.Itoa(udpPorts[0]),
					}, &stdout, &stderr, "v0.1.0-dev", "abc1234")
				}()

				deadline := time.Now().Add(15 * time.Second)
				ready := false
				for time.Now().Before(deadline) {
					if strings.Contains(stderr.String(), "bbops host ready") {
						ready = true
						break
					}
					time.Sleep(25 * time.Millisecond)
				}
				cancel()
				code := <-done
				if !ready || code != 0 {
					t.Fatalf("daemon ready=%v exit=%d stderr=%s", ready, code, stderr.String())
				}

				combined := stdout.String() + "\n" + stderr.String()
				if !strings.Contains(stderr.String(), "nats="+wantRedacted) {
					t.Fatalf("expected redacted nats=%s in stderr, got:\n%s", wantRedacted, stderr.String())
				}
				for _, secret := range tc.forbidden {
					if strings.Contains(combined, secret) {
						t.Fatalf("credential fixture %q leaked into CLI output:\n%s", secret, combined)
					}
				}
			})
		}
	})
}

func writeMutatedFlowFile(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(configs.DefaultFlowJSON, &doc); err != nil {
		t.Fatalf("unmarshal default flow: %v", err)
	}
	mutate(doc)
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal mutated flow: %v", err)
	}
	path := filepath.Join(t.TempDir(), "flow.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
