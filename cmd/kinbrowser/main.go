// kinbrowser — markdown-native browser for AI agents.
//
// Subcommands:
//
//	open <url>            read URL via 3-layer chain, print markdown
//	archive <url>         read + save to KinBrain notes/<date>/web/
//	version               print version + build info
//
// Flags (apply to open/archive):
//
//	--lightpanda WS_URL   enable Layer 2 via Lightpanda CDP endpoint
//	--no-chrome           disable Layer 3 (chromedp/Chrome)
//	--quiet               suppress layer/timing info on stderr
//	--cache N             session LRU size (default 128)
//
// Examples:
//
//	kinbrowser open https://arxiv.org/abs/2510.00001
//	kinbrowser open --lightpanda ws://localhost:9222 https://react.dev
//	kinbrowser archive https://example.com/paper.html
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/LocalKinAI/kinbrowser/pkg/kinbrowser"
)

const usage = `kinbrowser — markdown-native browser for AI agents

USAGE:
  kinbrowser <command> [flags] <url>

COMMANDS:
  open <url>           Read URL via 3-layer chain (HTTP→Lightpanda→chromedp),
                       extract main content, return markdown.
  archive <url>        open + write to KinBrain notes/ (opt-in archiving).
                       Requires kinbrain CLI on PATH.
  version              Print version + build info.

FLAGS:
  --lightpanda URL     Enable Layer 2 via Lightpanda CDP endpoint
                       (e.g. ws://localhost:9222). Default: disabled.
  --no-chrome          Disable Layer 3 (chromedp). Useful on servers.
  --quiet              Suppress layer/timing info on stderr.
  --cache N            Session LRU size (default 128).
  --timeout SECONDS    Per-backend timeout (default 30 for CDP, 20 for HTTP).

THREE-LAYER ESCALATION:
  Layer 1   HTTP + readability + html→markdown      ~100 ms   ~80% of sites
  Layer 2   Lightpanda (CDP, no rendering)          ~500 ms   +15% (SPA)
  Layer 3   chromedp (full Chrome)                  ~2 s      +5%  (stubborn)

OUTPUT:
  Always markdown to stdout. Layer + title + timing go to stderr.

DOCS:
  https://github.com/LocalKinAI/kinbrowser
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Print(usage)
	case "version", "-v", "--version":
		printVersion()
	case "open":
		runOpen(os.Args[2:], false)
	case "archive":
		runOpen(os.Args[2:], true)
	case "daemon":
		runDaemon(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "kinbrowser: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func runOpen(args []string, archive bool) {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	lightpanda := fs.String("lightpanda", "", "Lightpanda CDP WebSocket URL (enables Layer 2)")
	noChrome := fs.Bool("no-chrome", false, "disable Layer 3 (chromedp)")
	quiet := fs.Bool("quiet", false, "suppress stderr layer/timing info")
	cacheSize := fs.Int("cache", 128, "session LRU size")
	timeoutSec := fs.Int("timeout", 30, "per-backend timeout in seconds")
	forceLayer := fs.Int("force-layer", 0, "force a specific backend (1=HTTP, 2=Lightpanda, 3=chromedp) — debug only")
	_ = fs.Parse(args)

	rest := fs.Args()
	if len(rest) == 0 {
		die(`usage: kinbrowser ` + map[bool]string{true: "archive", false: "open"}[archive] + ` <url>`)
	}
	url := rest[0]

	opts := []kinbrowser.Option{
		kinbrowser.WithCacheSize(*cacheSize),
	}
	if *lightpanda != "" {
		opts = append(opts, kinbrowser.WithLightpanda(*lightpanda))
	}
	if *noChrome {
		opts = append(opts, kinbrowser.WithoutChromedp())
	}
	if *forceLayer != 0 {
		opts = append(opts, kinbrowser.WithForceLayer(*forceLayer))
	}

	b, err := kinbrowser.New(opts...)
	if err != nil {
		die("init: " + err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec*3)*time.Second)
	defer cancel()

	start := time.Now()
	var r kinbrowser.Result
	if archive {
		r, err = b.Archive(ctx, url)
	} else {
		r, err = b.Open(ctx, url)
	}
	elapsed := time.Since(start)

	if err != nil {
		die(err.Error())
	}

	if !*quiet {
		layerName := layerLabel(r.Layer, r.FromArchive)
		fmt.Fprintf(os.Stderr, "[kinbrowser] %s | %s | %d chars | %s\n",
			layerName, truncate(r.Title, 60), len(r.Markdown), elapsed)
	}
	fmt.Println(r.Markdown)
}

func layerLabel(layer int, fromArchive bool) string {
	if fromArchive {
		return "L0 archive"
	}
	switch layer {
	case 0:
		return "L0 cache"
	case 1:
		return "L1 http"
	case 2:
		return "L2 lightpanda"
	case 3:
		return "L3 chromedp"
	default:
		return "L" + strconv.Itoa(layer)
	}
}

func printVersion() {
	info, ok := debug.ReadBuildInfo()
	version := "dev"
	commit, date := "", ""
	if ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				commit = s.Value
			case "vcs.time":
				date = s.Value
			}
		}
	}
	fmt.Printf("kinbrowser %s\n", version)
	if commit != "" {
		if len(commit) > 12 {
			commit = commit[:12]
		}
		fmt.Printf("  commit: %s\n", commit)
	}
	if date != "" {
		fmt.Printf("  built:  %s\n", date)
	}
	if ok {
		fmt.Printf("  go:     %s\n", info.GoVersion)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, "kinbrowser: "+msg)
	os.Exit(1)
}

// runDaemon handles Lightpanda daemon lifecycle from the CLI:
//
//	kinbrowser daemon status   show pid + listening + uptime
//	kinbrowser daemon start    explicit start (auto-detect normally does this)
//	kinbrowser daemon stop     kill the running daemon
//
// Auto-spawn in normal `kinbrowser open` calls covers 99% of usage —
// these subcommands are for operators who want explicit control:
// debugging weird behavior, freeing the port, or pre-warming the
// daemon before a batch run.
func runDaemon(args []string) {
	if len(args) == 0 {
		die("daemon requires subcommand: status | start | stop")
	}
	switch args[0] {
	case "status":
		daemonStatus()
	case "start":
		daemonStart()
	case "stop":
		daemonStop()
	default:
		die("daemon: unknown subcommand " + args[0] + " (use status|start|stop)")
	}
}

func daemonStatus() {
	resp, err := http.Get("http://127.0.0.1:9222/json/version")
	if err != nil {
		fmt.Println("not running (no CDP on 127.0.0.1:9222)")
		os.Exit(1)
	}
	defer resp.Body.Close()
	var v struct {
		Browser              string `json:"Browser"`
		Protocol             string `json:"Protocol-Version"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	fmt.Printf("running\n  browser:  %s\n  protocol: %s\n  ws:       %s\n",
		v.Browser, v.Protocol, v.WebSocketDebuggerURL)

	// Try to find the pid via lsof so the user knows what to kill if needed.
	if out, err := exec.Command("lsof", "-iTCP:9222", "-sTCP:LISTEN", "-t").Output(); err == nil {
		pid := strings.TrimSpace(string(out))
		if pid != "" {
			fmt.Printf("  pid:      %s\n", pid)
		}
	}
}

func daemonStart() {
	if resp, err := http.Get("http://127.0.0.1:9222/json/version"); err == nil {
		resp.Body.Close()
		fmt.Println("already running on 127.0.0.1:9222 (no-op)")
		return
	}
	if _, err := exec.LookPath("lightpanda"); err != nil {
		die("lightpanda not on PATH. Install: brew install lightpanda-io/browser/lightpanda")
	}
	cmd := exec.Command("lightpanda", "serve", "--host", "127.0.0.1", "--port", "9222")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		die("start lightpanda: " + err.Error())
	}
	fmt.Printf("started lightpanda (pid %d) on 127.0.0.1:9222\n", cmd.Process.Pid)
	// Wait for CDP to actually answer (up to 3s) before reporting success.
	for i := 0; i < 30; i++ {
		if resp, err := http.Get("http://127.0.0.1:9222/json/version"); err == nil {
			resp.Body.Close()
			fmt.Println("CDP ready")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Println("warning: process started but CDP never opened — try `kinbrowser daemon status`")
}

func daemonStop() {
	out, err := exec.Command("lsof", "-iTCP:9222", "-sTCP:LISTEN", "-t").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		fmt.Println("no daemon running on 127.0.0.1:9222")
		return
	}
	pid := strings.TrimSpace(string(out))
	if err := exec.Command("kill", pid).Run(); err != nil {
		die("kill " + pid + ": " + err.Error())
	}
	fmt.Printf("stopped lightpanda (pid %s)\n", pid)
}
