package mavcodec_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// spec: mavlink-ingest / Clean-room common.xml MAVLink v2 frame codec and zero-GPLv3 dependency guard
func TestCleanRoomDependencyAndImportGuard(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	bannedTokens := []string{
		"ardupilotmega",
		"ardupilot",
		"betaflight",
		"inav",
		"cleanflight",
		"semlink",
		"semops",
	}

	// 1. Check go.mod and go.sum manifests.
	for _, manifest := range []string{"go.mod", "go.sum"} {
		raw, err := os.ReadFile(filepath.Join(repoRoot, manifest))
		if err != nil {
			t.Fatalf("read %s: %v", manifest, err)
		}
		lower := strings.ToLower(string(raw))
		for _, banned := range bannedTokens {
			if strings.Contains(lower, banned) {
				t.Fatalf("%s contains prohibited token %q", manifest, banned)
			}
		}
	}

	// 2. Check resolved build dependency graph (`go list -deps ./...`) and enforce
	// gomavlib package allowlist (strictly minimal/standard/common + framing).
	allowedGomavlibPackages := map[string]bool{
		"github.com/bluenviron/gomavlib/v3/pkg/dialect":           true,
		"github.com/bluenviron/gomavlib/v3/pkg/dialects/common":   true,
		"github.com/bluenviron/gomavlib/v3/pkg/dialects/minimal":  true,
		"github.com/bluenviron/gomavlib/v3/pkg/dialects/standard": true,
		"github.com/bluenviron/gomavlib/v3/pkg/frame":             true,
		"github.com/bluenviron/gomavlib/v3/pkg/message":           true,
		"github.com/bluenviron/gomavlib/v3/pkg/x25":               true,
	}

	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./... failed: %v\n%s", err, string(out))
	}

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		pkg := strings.TrimSpace(line)
		if pkg == "" {
			continue
		}
		lower := strings.ToLower(pkg)
		for _, banned := range bannedTokens {
			if strings.Contains(lower, banned) {
				t.Fatalf("build dependency %q matches prohibited token %q", pkg, banned)
			}
		}
		if strings.HasPrefix(pkg, "github.com/bluenviron/gomavlib/") && !allowedGomavlibPackages[pkg] {
			t.Fatalf("disallowed gomavlib package %q in build graph (only minimal/standard/common framing allowed)", pkg)
		}
	}

	// 3. AST-scan all repository Go files to ensure zero banned imports and that
	// gomavlib is encapsulated strictly inside internal/mavcodec (`D1` adopter seam).
	fset := token.NewFileSet()
	mavcodecDir := filepath.Join(repoRoot, "internal", "mavcodec")
	err = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".worktrees", "node_modules", "bin", ".bbops":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		inMavcodec := filepath.Dir(path) == mavcodecDir
		for _, imp := range parsed.Imports {
			impPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			lower := strings.ToLower(impPath)
			for _, banned := range bannedTokens {
				if strings.Contains(lower, banned) {
					t.Fatalf("%s imports prohibited package %q", path, impPath)
				}
			}
			if strings.HasPrefix(impPath, "github.com/bluenviron/gomavlib/") && !inMavcodec {
				t.Fatalf("%s imports %q outside internal/mavcodec adapter boundary", path, impPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("AST import scan failed: %v", err)
	}
}
