package main

// `install chrome` — set up docker + the chrome-driverless image that backs
// the headless in-app browser (internal/browser). `--dry-run` only prints the
// commands; `--doctor` diagnoses an existing setup.

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/browser"
)

type installOpts struct {
	dryRun bool
	doctor bool
}

func doInstall(args []string) {
	if len(args) == 0 || args[0] != "chrome" {
		fmt.Println("usage: zcode-quick-web-forward install chrome [--dry-run] [--doctor]")
		os.Exit(2)
	}
	var opts installOpts
	for _, a := range args[1:] {
		switch a {
		case "--dry-run", "-n":
			opts.dryRun = true
		case "--doctor":
			opts.doctor = true
		default:
			fmt.Printf("install chrome: unknown flag %q\n", a)
			os.Exit(2)
		}
	}
	if opts.doctor {
		os.Exit(doctorChrome())
	}
	if err := installChrome(opts); err != nil {
		fmt.Fprintf(os.Stderr, "install chrome: %v\n", err)
		os.Exit(1)
	}
}

// run prints (or executes) a command; dry-run never touches the system.
func run(dryRun bool, cmd *exec.Cmd) error {
	fmt.Printf("+ %s\n", strings.Join(cmd.Args, " "))
	if dryRun {
		return nil
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func dockerInstalled() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

func dockerAlive() bool {
	if !dockerInstalled() {
		return false
	}
	return exec.Command("docker", "info", "--format", "{{.ServerVersion}}").Run() == nil
}

func imagePresent(image string) bool {
	return exec.Command("docker", "image", "inspect", image).Run() == nil
}

func installChrome(opts installOpts) error {
	// 1. docker engine itself.
	if !dockerInstalled() {
		fmt.Println("docker not found; installing…")
		switch runtime.GOOS {
		case "linux":
			if _, err := exec.LookPath("apt-get"); err == nil {
				if err := run(opts.dryRun, exec.Command("sudo", "apt-get", "update")); err != nil {
					return err
				}
				if err := run(opts.dryRun, exec.Command("sudo", "apt-get", "install", "-y",
					"docker.io", "docker-compose-v2")); err != nil {
					return err
				}
			} else {
				// Official convenience script covers other distros.
				if err := run(opts.dryRun, exec.Command("sh", "-c",
					"curl -fsSL https://get.docker.com | sh")); err != nil {
					return err
				}
			}
		case "darwin":
			if err := run(opts.dryRun, exec.Command("brew", "install", "--cask", "docker")); err != nil {
				return err
			}
			fmt.Println("launch Docker Desktop once to start the daemon")
		default:
			return fmt.Errorf("unsupported OS %s: install docker manually", runtime.GOOS)
		}
	} else {
		fmt.Println("ok: docker found at " + dockerPath())
	}

	if opts.dryRun {
		fmt.Printf("+ docker pull %s\n", browser.DefaultDockerImage)
		fmt.Println("dry-run complete — nothing was changed")
		return nil
	}

	// 2. daemon must be up before pulling.
	if !dockerAlive() {
		return fmt.Errorf("docker daemon not reachable; start docker (systemctl start docker / Docker Desktop) and retry")
	}
	fmt.Println("ok: docker daemon running")

	// 3. the chrome-driverless image.
	if imagePresent(browser.DefaultDockerImage) {
		fmt.Println("ok: image already present")
	} else {
		fmt.Printf("pulling %s (this can take a few minutes)…\n", browser.DefaultDockerImage)
		if err := run(false, exec.Command("docker", "pull", browser.DefaultDockerImage)); err != nil {
			return fmt.Errorf("docker pull: %w", err)
		}
	}

	// 4. smoke test: boot the container, warm it, take a screenshot, tear down.
	fmt.Println("smoke test: launching chrome-driverless…")
	b, err := browser.LaunchDocker(browser.DefaultDockerImage)
	if err != nil {
		return fmt.Errorf("smoke test failed: %w", err)
	}
	defer b.Close()
	if err := b.Navigate("about:blank"); err != nil {
		return fmt.Errorf("smoke test navigate: %w", err)
	}
	if _, err := b.Screenshot(); err != nil {
		return fmt.Errorf("smoke test screenshot: %w", err)
	}
	fmt.Println("install chrome: done ✓  (try: zcode-quick-web-forward chrome open https://www.baidu.com)")
	return nil
}

func dockerPath() string {
	p, _ := exec.LookPath("docker")
	return p
}

// doctorChrome reports the health of each layer without changing anything.
func doctorChrome() int {
	ok := 0
	check := func(label string, pass bool, detail string) {
		mark := "✗"
		if pass {
			mark = "✓"
			ok++
		}
		fmt.Printf("%s %s: %s\n", mark, label, detail)
	}
	check("docker binary", dockerInstalled(), dockerPath())
	alive := dockerAlive()
	check("docker daemon", alive, func() string {
		if !alive {
			return "not reachable — try: systemctl start docker"
		}
		out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output()
		if err != nil {
			return "running"
		}
		return "running, server v" + strings.TrimSpace(string(out))
	}())
	check("chrome-driverless image", imagePresent(browser.DefaultDockerImage), browser.DefaultDockerImage)

	if alive {
		start := time.Now()
		b, err := browser.LaunchDocker(browser.DefaultDockerImage)
		if err != nil {
			check("container boot", false, err.Error())
		} else {
			defer b.Close()
			check("container boot", true, fmt.Sprintf("cdp on port %s (%.1fs)", b.CDPPort(), time.Since(start).Seconds()))
		}
	}
	fmt.Printf("\n%d/4 checks passed\n", ok)
	if ok == 4 {
		fmt.Println("all good — `zcode-quick-web-forward remote` will report the docker browser to the phone")
	}
	return 0
}
