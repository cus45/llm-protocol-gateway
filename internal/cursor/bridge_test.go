package cursor

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotDefaultStopped(t *testing.T) {
	t.Parallel()
	bridge := NewBridge("")
	snap := bridge.Snapshot()
	if snap.Status != BridgeStatusStopped {
		t.Fatalf("status = %q, want %q", snap.Status, BridgeStatusStopped)
	}
}

func TestProbePortSuccessAgainstServer(t *testing.T) {
	t.Parallel()
	ln := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"object":"list","data":[]}`)
	}))
	defer ln.Close()

	port := 0
	u := ln.URL
	for i := len(u) - 1; i >= 0; i-- {
		if u[i] == ':' {
			_, _ = fmt.Sscanf(u[i+1:], "%d", &port)
			break
		}
	}
	if port <= 0 {
		t.Fatalf("could not parse port from %s", ln.URL)
	}

	bridge := NewBridge("")
	bridge.httpClient = ln.Client()
	if err := bridge.probePort(port); err != nil {
		t.Fatalf("probePort: %v", err)
	}
}

func TestProbePortFailsOnClosedPort(t *testing.T) {
	t.Parallel()
	bridge := NewBridge("")
	bridge.httpClient = &http.Client{}
	if err := bridge.probePort(1); err == nil {
		t.Fatal("expected probe on closed port to fail")
	}
}

// withRuntimeLookups swaps the bun/node lookups for the duration of a test, so
// "does this machine have bun" never decides whether the test passes.
func withRuntimeLookups(t *testing.T, bun func() (string, error), node func() (string, error)) {
	t.Helper()
	oldBun, oldNode := findBun, findNode
	findBun, findNode = bun, node
	t.Cleanup(func() { findBun, findNode = oldBun, oldNode })
}

// bridgeDirWith lays out a fake cursor-bridge directory: "ts" drops
// standalone.ts (a source checkout), "bundle" drops dist/cursor-bridge.mjs
// (what deploy ships), both is a checkout that also carries a build.
func bridgeDirWith(t *testing.T, kinds ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, kind := range kinds {
		switch kind {
		case "ts":
			if err := os.WriteFile(filepath.Join(dir, "standalone.ts"), []byte("// ts"), 0o644); err != nil {
				t.Fatal(err)
			}
		case "bundle":
			dist := filepath.Join(dir, "dist")
			if err := os.MkdirAll(dist, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dist, "cursor-bridge.mjs"), []byte("// js"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

func TestResolveRuntimePrefersBunOnSources(t *testing.T) {
	withRuntimeLookups(t,
		func() (string, error) { return "/opt/bun", nil },
		func() (string, error) { return "/usr/bin/node", nil },
	)
	runtime, err := NewBridge("").resolveRuntime(bridgeDirWith(t, "ts"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if runtime.name != "bun" || runtime.path != "/opt/bun" {
		t.Fatalf("runtime = %+v, want bun", runtime)
	}
	// bun runs the sources directly: `bun run standalone.ts`
	if len(runtime.args) != 1 || runtime.args[0] != "run" {
		t.Fatalf("args = %v, want [run]", runtime.args)
	}
	if filepath.Base(runtime.entry) != "standalone.ts" {
		t.Fatalf("entry = %q, want the TS sources", runtime.entry)
	}
}

// The reason this exists: a Windows checkout has node but no bun, and used to
// report "bun not found" and refuse to serve Cursor at all.
func TestResolveRuntimeFallsBackToNodeOnSources(t *testing.T) {
	withRuntimeLookups(t,
		func() (string, error) { return "", errors.New("executable file not found") },
		func() (string, error) { return "/usr/bin/node", nil },
	)
	runtime, err := NewBridge("").resolveRuntime(bridgeDirWith(t, "ts"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if runtime.name != "node" || runtime.path != "/usr/bin/node" {
		t.Fatalf("runtime = %+v, want node", runtime)
	}
	// The generated protobuf module uses TS enums, which node's plain
	// type-stripping cannot run, so the transform flag is load-bearing.
	joined := strings.Join(runtime.args, " ")
	if !strings.Contains(joined, "--experimental-transform-types") {
		t.Fatalf("args = %v, want the transform-types flag", runtime.args)
	}
	if runtime.bunErr == nil {
		t.Fatal("want the bun failure kept for diagnostics when node is used")
	}
}

// The deployment case: a server carries only the prebuilt bundle, and its node
// may be older than type-stripping — plain JS needs no flags at all.
func TestResolveRuntimeRunsBundleWithoutFlags(t *testing.T) {
	withRuntimeLookups(t,
		func() (string, error) { return "", errors.New("no bun on the server") },
		func() (string, error) { return "/usr/bin/node", nil },
	)
	dir := bridgeDirWith(t, "bundle")
	runtime, err := NewBridge("").resolveRuntime(dir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if runtime.name != "node" || len(runtime.args) != 0 {
		t.Fatalf("runtime = %+v, want bare node", runtime)
	}
	if runtime.entry != filepath.Join(dir, "dist", "cursor-bridge.mjs") {
		t.Fatalf("entry = %q, want the bundle", runtime.entry)
	}
}

func TestResolveRuntimeReportsMissingSources(t *testing.T) {
	withRuntimeLookups(t,
		func() (string, error) { return "/opt/bun", nil },
		func() (string, error) { return "/usr/bin/node", nil },
	)
	_, err := NewBridge("").resolveRuntime(bridgeDirWith(t))
	if err == nil || !strings.Contains(err.Error(), "cursor bridge missing") {
		t.Fatalf("err = %v, want a missing-sources error", err)
	}
}

func TestResolveRuntimeReportsBothMissing(t *testing.T) {
	withRuntimeLookups(t,
		func() (string, error) { return "", errors.New("no bun") },
		func() (string, error) { return "", errors.New("no node") },
	)
	_, err := NewBridge("").resolveRuntime(bridgeDirWith(t, "ts", "bundle"))
	if err == nil {
		t.Fatal("want an error when neither runtime is available")
	}
	for _, want := range []string{"bun", "node"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
}
