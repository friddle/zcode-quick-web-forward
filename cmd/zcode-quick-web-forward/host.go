// Local host helpers: chromium browser host bootstrap, glm runtime
// resolution, zcode.cjs script path / passthrough CLI execution and the
// Playwright browsers cache lookup.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/friddle/zcode-quick-web-forward/internal/browser"
	"github.com/friddle/zcode-quick-web-forward/internal/runtime"
)

// launchBrowser starts the headless chromium browser host, or returns nil when
// no chromium is available (browser tasks then report backend_unavailable).
func launchBrowser() *browser.Browser {
	if browser.FindChromium() == "" {
		fmt.Println("zcode: no chromium found; browser tasks unavailable (set PLAYWRIGHT_BROWSERS_PATH)")
		return nil
	}
	b, err := browser.Launch()
	if err != nil {
		fmt.Printf("zcode: browser launch failed: %v (browser tasks unavailable)\n", err)
		return nil
	}
	fmt.Printf("zcode: browser host ready (%s)\n", b.ID())
	return b
}

func resolveRuntime(runtimePath string) (dir string) {
	dir, _, err := runtime.NewFinder().Resolve(runtimePath)
	if err != nil {
		fatal("runtime resolve failed: %v", err)
	}
	fmt.Printf("zcode: runtime ready at %s\n", dir)
	return dir
}

func scriptPath(dir string) string {
	if strings.HasSuffix(dir, ".cjs") {
		return dir
	}
	return dir + "/zcode.cjs"
}

func runCLI(node, script string, args []string) error {
	cmd := exec.Command(node, append([]string{script}, args...)...)
	cmd.Dir = dirOf(script)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGINT)
		}
	}()
	defer signal.Stop(sig)
	return cmd.Run()
}

func dirOf(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[:i]
	}
	return "."
}

// defaultPlaywrightPath returns the Playwright browsers cache directory used
// for headless browser tooling, preferring an existing one on the machine.
func defaultPlaywrightPath() string {
	if v := os.Getenv("PLAYWRIGHT_BROWSERS_PATH"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil {
		cands := []string{
			filepath.Join(home, ".cache", "ms-playwright"),
			filepath.Join(home, "Library", "Caches", "ms-playwright"),
		}
		for _, c := range cands {
			if fi, err := os.Stat(c); err == nil && fi.IsDir() {
				return c
			}
		}
	}
	return ""
}
