package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/boxbotix/bbops/internal/e2eharness"
)

type commandRequest struct {
	ID       string                          `json:"id"`
	Action   string                          `json:"action"`
	SysID    uint8                           `json:"sysid,omitempty"`
	Severity uint8                           `json:"severity,omitempty"`
	Text     string                          `json:"text,omitempty"`
	Contact  *e2eharness.ExternalContactSeed `json:"contact,omitempty"`
}

type commandReply struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func main() {
	var uiDir string
	flag.StringVar(&uiDir, "ui-dir", "", "Path to compiled SvelteKit ui/build directory")
	flag.Parse()

	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	stack, err := e2eharness.StartStack(ctx, e2eharness.Options{
		UIDir: uiDir,
	})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "e2eharness start failed: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = stack.Stop(stopCtx)
	}()

	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(stack.Info()); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "e2eharness write info: %v\n", err)
		os.Exit(1)
	}

	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var req commandRequest
			if decErr := json.Unmarshal(line, &req); decErr != nil {
				_ = enc.Encode(commandReply{OK: false, Error: decErr.Error()})
				continue
			}
			reply := commandReply{ID: req.ID, OK: true}
			switch req.Action {
			case "pulseCrsfOverride":
				if actErr := stack.PulseCRSFOverride(req.SysID); actErr != nil {
					reply.OK = false
					reply.Error = actErr.Error()
				}
			case "emitStatusText":
				if actErr := stack.EmitStatusText(req.SysID, req.Severity, req.Text); actErr != nil {
					reply.OK = false
					reply.Error = actErr.Error()
				}
			case "upsertExternalContact":
				if req.Contact == nil {
					reply.OK = false
					reply.Error = "contact payload required"
				} else if actErr := stack.UpsertExternalContact(ctx, *req.Contact); actErr != nil {
					reply.OK = false
					reply.Error = actErr.Error()
				}
			case "shutdown":
				_ = enc.Encode(reply)
				return
			default:
				reply.OK = false
				reply.Error = fmt.Sprintf("unknown action %q", req.Action)
			}
			_ = enc.Encode(reply)
		}
	}()

	select {
	case <-ctx.Done():
	case <-stdinDone:
	}
}
