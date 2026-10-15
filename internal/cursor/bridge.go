package cursor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/packaged"
)

const (
	BridgeStatusStopped    = "stopped"
	BridgeStatusStarting   = "starting"
	BridgeStatusHealthy    = "healthy"
	BridgeStatusUnhealthy  = "unhealthy"
	BridgeStatusRestarting = "restarting"

	defaultProbeTimeout  = 2 * time.Second
	defaultWatchInterval = 45 * time.Second
	defaultStartTimeout  = 60 * time.Second
)

// Status is the live cursor-bridge subprocess snapshot (runtime-only).
type Status struct {
	Status    string `json:"status"`
	Port      int    `json:"port,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Message   string `json:"message,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
	CheckedAt string `json:"checkedAt,omitempty"`
}

// Bridge manages the local OpenAI-compatible Cursor gRPC bridge subprocess.
type Bridge struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	port      int
	tokenFile string
	repoRoot  string

	status    string
	message   string
	startedAt time.Time
	checkedAt time.Time
	exitCh    chan struct{}

	probeTimeout  time.Duration
	watchInterval time.Duration
	startTimeout  time.Duration
	httpClient    *http.Client

	watchOnce sync.Once
	watchStop chan struct{}
}

func NewBridge(repoRoot string) *Bridge {
	return &Bridge{
		repoRoot:      repoRoot,
		status:        BridgeStatusStopped,
		probeTimeout:  defaultProbeTimeout,
		watchInterval: defaultWatchInterval,
		startTimeout:  defaultStartTimeout,
	}
}

// DefaultTokenFilePath returns the user-level Cursor access-token path
// (~/Library/Application Support/llm-protocol-gateway/cursor/access-token on macOS).
// It does not create the directory.
func DefaultTokenFilePath() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir = os.TempDir()
	}
	return filepath.Join(configDir, "llm-protocol-gateway", "cursor", "access-token")
}

func (b *Bridge) TokenFilePath() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokenFilePathLocked()
}

func (b *Bridge) tokenFilePathLocked() string {
	if b.tokenFile != "" {
		return b.tokenFile
	}
	path := DefaultTokenFilePath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	b.tokenFile = path
	return path
}

// writeToken persists the access token, skipping the write when the file
// already holds exactly this value.
//
// EnsureRunning is called on every Cursor-routed request, so an unconditional
// write here meant one full file rewrite per request even in the steady state
// where the token has not changed. Reading first is cheap (the file is a few
// hundred bytes and stays in the page cache) and turns the common case into
// zero disk writes.
func (b *Bridge) writeToken(token string) error {
	path := b.TokenFilePath()
	token = strings.TrimSpace(token)
	if existing, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(existing)) == token {
		return nil
	}
	return os.WriteFile(path, []byte(token), 0o600)
}

func (b *Bridge) bridgeDir() string {
	if dir := packaged.CursorBridgeDir(); dir != "" {
		return dir
	}
	if b.repoRoot != "" {
		return filepath.Join(b.repoRoot, "scripts", "cursor-bridge")
	}
	return ""
}

// findBun / findNode are vars so tests can describe a machine that has one,
// both, or neither runtime without depending on what the host happens to
// install.
var (
	findBun  = packaged.Bun
	findNode = func() (string, error) { return exec.LookPath("node") }
)

func bunLookupError(err error) error {
	return fmt.Errorf("bun not found: %w", err)
}

// bridgeRuntime is the JS runtime plus the entry file the Cursor bridge runs
// on. bun comes first — the packaged macOS App ships its own — but the bridge
// also runs on plain node, so a machine without bun (Windows, or a bare
// checkout) is not left unable to serve Cursor.
//
// Two entry shapes exist: the TypeScript sources (a dev checkout), which node
// can only run with --experimental-transform-types because the generated
// protobuf module uses TS enums; and the prebuilt dist/cursor-bridge.mjs bundle
// (what deploy ships), which is plain JS and runs on any modern node.
type bridgeRuntime struct {
	path   string
	args   []string
	entry  string
	name   string
	bunErr error
}

func (b *Bridge) resolveRuntime(bridgeDir string) (bridgeRuntime, error) {
	tsEntry := filepath.Join(bridgeDir, "standalone.ts")
	bundleEntry := filepath.Join(bridgeDir, "dist", "cursor-bridge.mjs")
	hasTS := fileExists(tsEntry)
	hasBundle := fileExists(bundleEntry)

	bun, bunErr := findBun()
	node, nodeErr := findNode()

	switch {
	case hasTS && bunErr == nil:
		return bridgeRuntime{path: bun, args: []string{"run"}, entry: tsEntry, name: "bun"}, nil
	case hasTS && nodeErr == nil:
		return bridgeRuntime{
			path:   node,
			args:   []string{"--experimental-transform-types", "--no-warnings"},
			entry:  tsEntry,
			name:   "node",
			bunErr: bunLookupError(bunErr),
		}, nil
	case hasBundle && nodeErr == nil:
		return bridgeRuntime{path: node, entry: bundleEntry, name: "node"}, nil
	case hasBundle && bunErr == nil:
		return bridgeRuntime{path: bun, args: []string{"run"}, entry: bundleEntry, name: "bun"}, nil
	}
	if !hasTS && !hasBundle {
		return bridgeRuntime{}, fmt.Errorf(
			"cursor bridge missing at %s: expected standalone.ts (source checkout) or dist/cursor-bridge.mjs (built bundle)", bridgeDir)
	}
	return bridgeRuntime{}, fmt.Errorf(
		"cursor bridge needs a JS runtime but neither bun nor node was found: install node (https://nodejs.org) or bun (https://bun.sh); %v", bunLookupError(bunErr))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (b *Bridge) client() *http.Client {
	timeout := b.probeTimeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	if b.httpClient != nil {
		return b.httpClient
	}
	return &http.Client{Timeout: timeout}
}

// Snapshot returns the current bridge status for UI / diagnostics.
func (b *Bridge) Snapshot() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := Status{
		Status:  b.status,
		Port:    b.port,
		Message: b.message,
	}
	if out.Status == "" {
		out.Status = BridgeStatusStopped
	}
	if b.cmd != nil && b.cmd.Process != nil {
		out.PID = b.cmd.Process.Pid
	}
	if !b.startedAt.IsZero() {
		out.StartedAt = b.startedAt.Format(time.RFC3339)
	}
	if !b.checkedAt.IsZero() {
		out.CheckedAt = b.checkedAt.Format(time.RFC3339)
	}
	return out
}

// EnsureRunning writes the access token and starts the bridge if needed.
// If a process is already tracked, it is HTTP-probed first and restarted when unhealthy.
// started is true when a new bridge process was launched.
func (b *Bridge) EnsureRunning(accessToken string) (port int, started bool, err error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return 0, false, fmt.Errorf("cursor access token is empty")
	}
	if err := b.writeToken(accessToken); err != nil {
		return 0, false, err
	}

	b.mu.Lock()
	if b.processAliveLocked() {
		port = b.port
		b.mu.Unlock()
		if err := b.probePort(port); err == nil {
			b.mu.Lock()
			b.status = BridgeStatusHealthy
			b.message = ""
			b.checkedAt = time.Now()
			port = b.port
			b.mu.Unlock()
			return port, false, nil
		}
		b.mu.Lock()
		b.status = BridgeStatusRestarting
		b.message = "health probe failed; restarting"
		b.stopLocked()
		if err := b.startLocked(); err != nil {
			b.mu.Unlock()
			return 0, false, err
		}
		port = b.port
		b.mu.Unlock()
		return port, true, nil
	}

	if err := b.startLocked(); err != nil {
		b.mu.Unlock()
		return 0, false, err
	}
	port = b.port
	b.mu.Unlock()
	return port, true, nil
}

func (b *Bridge) processAliveLocked() bool {
	if b.cmd == nil || b.cmd.Process == nil || b.port <= 0 || b.exitCh == nil {
		return false
	}
	select {
	case <-b.exitCh:
		return false
	default:
		return true
	}
}

func (b *Bridge) probePort(port int) error {
	if port <= 0 {
		return fmt.Errorf("invalid bridge port")
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/models", port)
	resp, err := b.client().Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("bridge probe HTTP %d", resp.StatusCode)
	}
	return nil
}

func (b *Bridge) startLocked() error {
	bridgeDir := b.bridgeDir()
	if bridgeDir == "" {
		err := fmt.Errorf("cursor bridge missing (expected App Resources/cursor-bridge or scripts/cursor-bridge)")
		b.status = BridgeStatusUnhealthy
		b.message = err.Error()
		return err
	}
	runtime, err := b.resolveRuntime(bridgeDir)
	if err != nil {
		b.status = BridgeStatusUnhealthy
		b.message = err.Error()
		return err
	}

	b.stopLocked()
	b.status = BridgeStatusStarting
	b.message = ""

	cmd := exec.Command(runtime.path, append(runtime.args, runtime.entry)...)
	cmd.Dir = bridgeDir
	cmd.Env = append(os.Environ(),
		"CURSOR_TOKEN_FILE="+b.tokenFilePathLocked(),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		b.status = BridgeStatusUnhealthy
		b.message = err.Error()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		b.status = BridgeStatusUnhealthy
		b.message = err.Error()
		return err
	}
	if err := cmd.Start(); err != nil {
		b.status = BridgeStatusUnhealthy
		b.message = err.Error()
		return err
	}

	readyCh := make(chan int, 1)
	errCh := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var payload struct {
				Event   string `json:"event"`
				Port    int    `json:"port"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(line), &payload); err != nil {
				continue
			}
			if payload.Event == "ready" && payload.Port > 0 {
				readyCh <- payload.Port
				return
			}
			if payload.Event == "error" {
				errCh <- fmt.Errorf("%s", payload.Message)
				return
			}
		}
		if err := scanner.Err(); err != nil {
			errCh <- err
			return
		}
		errCh <- fmt.Errorf("cursor bridge exited before ready")
	}()
	go func() {
		data, _ := io.ReadAll(stderr)
		if len(strings.TrimSpace(string(data))) > 0 {
			_, _ = os.Stderr.WriteString("cursor-bridge: " + string(data))
		}
	}()

	startTimeout := b.startTimeout
	if startTimeout <= 0 {
		startTimeout = defaultStartTimeout
	}

	select {
	case port := <-readyCh:
		exitCh := make(chan struct{})
		b.cmd = cmd
		b.port = port
		b.exitCh = exitCh
		b.startedAt = time.Now()
		b.checkedAt = time.Now()
		b.status = BridgeStatusHealthy
		b.message = ""
		go b.reap(cmd, exitCh)
		return nil
	case err := <-errCh:
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		b.status = BridgeStatusUnhealthy
		b.message = err.Error()
		return err
	case <-time.After(startTimeout):
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		err := fmt.Errorf("timed out waiting for cursor bridge to start")
		b.status = BridgeStatusUnhealthy
		b.message = err.Error()
		return err
	}
}

func (b *Bridge) reap(cmd *exec.Cmd, exitCh chan struct{}) {
	_ = cmd.Wait()
	close(exitCh)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd == cmd {
		b.cmd = nil
		b.port = 0
		b.exitCh = nil
		if b.status != BridgeStatusRestarting && b.status != BridgeStatusStarting {
			b.status = BridgeStatusStopped
			b.message = "bridge process exited"
			b.checkedAt = time.Now()
		}
	}
}

func (b *Bridge) stopLocked() {
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
		// Wait is owned by reap goroutine; just clear refs after signaling kill.
	}
	b.cmd = nil
	b.port = 0
	b.exitCh = nil
}

func (b *Bridge) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopLocked()
	b.status = BridgeStatusStopped
	b.message = ""
	b.checkedAt = time.Now()
	if b.watchStop != nil {
		close(b.watchStop)
		b.watchStop = nil
	}
}

func (b *Bridge) BaseURL() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.port <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/v1", b.port)
}

// StartHealthWatch periodically probes a running bridge and restarts it when hung.
// Safe to call multiple times; only the first call starts the loop.
func (b *Bridge) StartHealthWatch(ctx context.Context) {
	b.watchOnce.Do(func() {
		b.mu.Lock()
		stop := make(chan struct{})
		b.watchStop = stop
		interval := b.watchInterval
		b.mu.Unlock()
		if interval <= 0 {
			interval = defaultWatchInterval
		}
		go b.watchLoop(ctx, stop, interval)
	})
}

func (b *Bridge) watchLoop(ctx context.Context, stop <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			b.healthCheckAndRepair()
		}
	}
}

func (b *Bridge) healthCheckAndRepair() {
	b.mu.Lock()
	if !b.processAliveLocked() {
		// Lazy-start: nothing to repair until EnsureRunning launches a process.
		if b.status == BridgeStatusHealthy || b.status == BridgeStatusUnhealthy || b.status == BridgeStatusRestarting {
			b.status = BridgeStatusStopped
			b.message = "bridge process not running"
			b.checkedAt = time.Now()
		}
		b.mu.Unlock()
		return
	}
	port := b.port
	tokenPath := b.tokenFilePathLocked()
	b.mu.Unlock()

	if err := b.probePort(port); err == nil {
		b.mu.Lock()
		b.status = BridgeStatusHealthy
		b.message = ""
		b.checkedAt = time.Now()
		b.mu.Unlock()
		return
	} else {
		b.mu.Lock()
		b.status = BridgeStatusRestarting
		b.message = fmt.Sprintf("health probe failed: %v", err)
		b.checkedAt = time.Now()
		b.stopLocked()
		b.mu.Unlock()
	}

	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil || strings.TrimSpace(string(tokenBytes)) == "" {
		b.mu.Lock()
		b.status = BridgeStatusUnhealthy
		b.message = "health probe failed and access token unavailable for restart"
		b.checkedAt = time.Now()
		b.mu.Unlock()
		return
	}
	if err := b.writeToken(string(tokenBytes)); err != nil {
		b.mu.Lock()
		b.status = BridgeStatusUnhealthy
		b.message = err.Error()
		b.checkedAt = time.Now()
		b.mu.Unlock()
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.startLocked(); err != nil {
		// startLocked already set unhealthy status/message
		return
	}
}

// FindRepoRoot locates the repository root containing scripts/cursor-bridge,
// or returns "" when running from a packaged App that embeds the bridge.
func FindRepoRoot() string {
	if root := packaged.RepoRoot(); root != "" {
		return root
	}
	return ""
}
