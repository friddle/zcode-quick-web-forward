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
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/browser"
	"github.com/friddle/zcode-quick-web-forward/internal/runtime"
)

// launchBrowser starts the headless chromium browser host, or returns nil when
// no chromium is available (browser tasks then report backend_unavailable).
// On servers without a Playwright chromium install it falls back to the
// chrome-driverless docker image (ZCODE_BROWSER_DOCKER_IMAGE overrides).
func launchBrowser() *browser.Browser {
	if image := os.Getenv("ZCODE_BROWSER_DOCKER_IMAGE"); image != "" {
		b, err := browser.LaunchDocker(image)
		if err != nil {
			fmt.Printf("zcode: docker browser launch failed: %v (browser tasks unavailable)\n", err)
			return nil
		}
		fmt.Printf("zcode: browser host ready via docker %s (%s)\n", image, b.ID())
		return b
	}
	if browser.FindChromium() == "" {
		if _, err := exec.LookPath("docker"); err == nil {
			b, err := browser.LaunchDocker(browser.DefaultDockerImage)
			if err != nil {
				fmt.Printf("zcode: docker browser launch failed: %v (browser tasks unavailable)\n", err)
				return nil
			}
			fmt.Printf("zcode: browser host ready via docker %s (%s)\n", browser.DefaultDockerImage, b.ID())
			return b
		}
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

// keepBrowserAlive parks a headless chromium on the given CDP port (9333 —
// the desktop in-app browser's default) for the lifetime of the daemon. The
// engine's browser-use plugin probes that endpoint directly when no host
// browser service exists, so a healthy browser there IS the browser backend.
// Crashes are restarted after a short pause; giving up entirely when no
// chromium is available keeps a broken box from spinning.
func keepBrowserAlive(port string) {
	failures := 0
	for {
		if image := os.Getenv("ZCODE_BROWSER_DOCKER_IMAGE"); image != "" || browser.FindChromium() == "" {
			b := launchBrowser()
			if b == nil {
				return
			}
			b.Wait()
			b.Close()
		} else {
			b, err := browser.LaunchPinned(port)
			if err != nil {
				failures++
				fmt.Printf("zcode: browser park on %s failed: %v\n", port, err)
				if failures >= 3 {
					fmt.Printf("zcode: browser park giving up after %d failures\n", failures)
					return
				}
				time.Sleep(time.Duration(failures) * 3 * time.Second)
				continue
			}
			failures = 0
			fmt.Printf("zcode: browser parked on CDP %s (%s)\n", port, b.ID())
			b.Wait()
			b.Close()
			fmt.Printf("zcode: browser parked on %s exited; restarting\n", port)
		}
		time.Sleep(2 * time.Second)
	}
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
