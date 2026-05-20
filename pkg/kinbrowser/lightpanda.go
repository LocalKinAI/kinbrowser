package kinbrowser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// lightpandaManager — autodetect, lazy-spawn, and own the lifecycle of
// a Lightpanda daemon for Layer 2.
//
// Behavior chain:
//
//  1. If $KINBROWSER_LIGHTPANDA_URL is set, treat it as the existing
//     CDP endpoint and skip everything below (caller manages daemon).
//  2. If `lightpanda` binary is on PATH, lazy-spawn it on first L2 need.
//     Health-checks via /json/version. Caches the WebSocket URL.
//  3. If neither, L2 stays off — kinbrowser falls through to L3.
//
// The daemon is shared across all Browser instances in the process and
// is reaped on os.Exit (Go closes child stdin/stdout, lightpanda exits).
// We don't try harder than that — leaking a daemon if kinbrowser is
// SIGKILL'd is acceptable: brew installed it, it's just a 60MB process,
// and the next kinbrowser invocation will see the existing port and
// reuse.
type lightpandaManager struct {
	mu       sync.Mutex
	wsURL    string    // populated once daemon is up
	pid      int       // 0 if we didn't spawn (existing daemon or env override)
	probedAt time.Time // last successful /json/version probe
}

var defaultLightpanda = &lightpandaManager{}

// detectLightpanda is the entry point called from Open() when L2 might
// be needed. Returns ("", false) when L2 isn't available; ("ws://...", true)
// when it is.
//
// Strategy:
//  1. env override: $KINBROWSER_LIGHTPANDA_URL — caller-managed daemon
//  2. existing daemon on default port (127.0.0.1:9222) — reuse it
//  3. lightpanda binary on PATH → spawn daemon → use it
//  4. otherwise → ("", false), kinbrowser falls through to L3
func (m *lightpandaManager) detect() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if v := os.Getenv("KINBROWSER_LIGHTPANDA_URL"); v != "" {
		m.wsURL = v
		return v, true
	}

	// If we've already discovered/spawned a daemon, re-probe quickly
	// (avoids re-spawning if user runs `kinbrowser` in tight loop).
	if m.wsURL != "" && time.Since(m.probedAt) < 30*time.Second {
		return m.wsURL, true
	}

	// Probe well-known port: lightpanda's default + Chrome's default.
	if url := probeCDP("http://127.0.0.1:9222/json/version"); url != "" {
		m.wsURL = url
		m.probedAt = time.Now()
		return url, true
	}

	// No daemon running. Try to spawn one if the binary exists.
	if _, err := exec.LookPath("lightpanda"); err != nil {
		return "", false
	}

	if err := m.spawn(); err != nil {
		// Spawn failed — log via stderr (visible when not --quiet), but
		// don't fail the whole call. L3 will pick up.
		fmt.Fprintf(os.Stderr, "kinbrowser: lightpanda binary present but spawn failed: %v\n", err)
		return "", false
	}
	return m.wsURL, true
}

// spawn starts lightpanda serve in the background and waits for it to
// answer /json/version. Caller holds m.mu.
func (m *lightpandaManager) spawn() error {
	cmd := exec.Command("lightpanda", "serve", "--host", "127.0.0.1", "--port", "9222")
	// Detach stdio — the daemon's logs would otherwise pollute kinbrowser's
	// stdout. If users want them, they can run lightpanda directly.
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting lightpanda: %w", err)
	}
	m.pid = cmd.Process.Pid

	// Wait up to 3s for the CDP endpoint to come up. Lightpanda starts
	// in <100ms typically, but give it room.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if url := probeCDP("http://127.0.0.1:9222/json/version"); url != "" {
			m.wsURL = url
			m.probedAt = time.Now()
			return nil
		}
		time.Sleep(80 * time.Millisecond)
	}

	// Spawn succeeded but never answered. Kill it; report failure.
	_ = cmd.Process.Kill()
	m.pid = 0
	return fmt.Errorf("lightpanda spawned (pid=%d) but never opened CDP on :9222", cmd.Process.Pid)
}

// probeCDP hits /json/version and returns the webSocketDebuggerUrl if
// the response shape matches Chrome DevTools Protocol. Returns "" on
// any failure.
func probeCDP(httpURL string) string {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(httpURL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return ""
	}
	// Lightpanda returns "ws://127.0.0.1:9222/" without a target id;
	// Chrome returns "ws://localhost:9222/devtools/browser/<uuid>".
	// chromedp.NewRemoteAllocator accepts both.
	if v.WebSocketDebuggerURL == "" {
		return ""
	}
	if !strings.HasPrefix(v.WebSocketDebuggerURL, "ws://") &&
		!strings.HasPrefix(v.WebSocketDebuggerURL, "wss://") {
		return ""
	}
	return v.WebSocketDebuggerURL
}
