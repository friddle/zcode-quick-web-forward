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

// keepBrowserAlive parks a browser on CDP port 9333 (the desktop in-app
// browser's default) for the lifetime of the daemon, so the engine's
// browser-use fallback always finds a healthy endpoint there. Two backends:
// ZCODE_BROWSER_DOCKER_IMAGE runs the chrome-driverless container with a
// standard-CDP façade on 9333 (Chromium inside the container only binds
// container-localhost, so the raw port is unreachable); otherwise a local
// Playwright chromium is launched pinned to the port. Crashes restart after
// a short pause; giving up entirely when no backend exists keeps a broken
// box from spinning.
func keepBrowserAlive(port string, done <-chan struct{}) {
	failures := 0
	for {
		image := os.Getenv("ZCODE_BROWSER_DOCKER_IMAGE")
		if image == "" && browser.FindChromium() == "" {
			image = browser.DefaultDockerImage
		}
		if image != "" {
			b, err := browser.LaunchDocker(image)
			if err != nil {
				failures++
				fmt.Printf("zcode: docker browser launch failed: %v\n", err)
				if failures >= 3 {
					fmt.Printf("zcode: docker browser giving up after %d failures\n", failures)
					return
				}
				if !sleepUntil(done, time.Duration(failures)*3*time.Second) {
					return
				}
				continue
			}
			failures = 0
			fmt.Printf("zcode: browser parked via docker %s (%s), CDP facade on %s\n", image, b.ID(), port)
			errCh := make(chan error, 1)
			go func() { errCh <- b.ServeCDP(port) }()
			exit := make(chan struct{})
			go func() { b.Wait(); close(exit) }()
			select {
			case <-exit:
			case <-errCh:
				fmt.Printf("zcode: CDP facade on %s failed; browser unreachable for the engine\n", port)
			case <-done:
				b.Close()
				return
			}
			b.Close()
			fmt.Printf("zcode: docker browser exited; restarting\n")
		} else {
			b, err := browser.LaunchPinned(port)
			if err != nil {
				failures++
				fmt.Printf("zcode: browser park on %s failed: %v\n", port, err)
				if failures >= 3 {
					fmt.Printf("zcode: browser park giving up after %d failures\n", failures)
					return
				}
				if !sleepUntil(done, time.Duration(failures)*3*time.Second) {
					return
				}
				continue
			}
			failures = 0
			fmt.Printf("zcode: browser parked on CDP %s (%s)\n", port, b.ID())
			exit := make(chan struct{})
			go func() { b.Wait(); close(exit) }()
			select {
			case <-exit:
			case <-done:
				b.Close()
				return
			}
			b.Close()
			fmt.Printf("zcode: browser parked on %s exited; restarting\n", port)
		}
		if !sleepUntil(done, 2*time.Second) {
			return
		}
	}
}

// sleepUntil waits for d or until done closes; false means done.
func sleepUntil(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return false
	case <-time.After(d):
		return true
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
