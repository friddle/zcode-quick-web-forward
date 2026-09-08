package main

// `chrome open [url]` — drive the chrome-driverless container directly from
// the CLI: launch it, navigate, and save a screenshot. Handy for verifying the
// install without pairing a phone.

import (
	"encoding/base64"
	"fmt"
	"os"
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/browser"
)

func doChrome(args []string) {
	if len(args) == 0 || args[0] != "open" {
		fmt.Println("usage: zcode-quick-web-forward chrome open [url] [--out PATH]")
		fmt.Println("  url defaults to https://www.baidu.com; screenshot lands next to it")
		os.Exit(2)
	}
	url, out := parseChromeArgs(args[1:])

	fmt.Printf("launching chrome-driverless (%s)…\n", browser.DefaultDockerImage)
	b, err := browser.LaunchDocker(browser.DefaultDockerImage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chrome: %v\n", err)
		os.Exit(1)
	}
	defer b.Close()

	fmt.Println("navigating to " + url)
	if err := b.Navigate(url); err != nil {
		fmt.Fprintf(os.Stderr, "chrome: navigate: %v\n", err)
		os.Exit(1)
	}
	time.Sleep(3 * time.Second) // let the page paint before the shot

	data, err := b.Screenshot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "chrome: screenshot: %v\n", err)
		os.Exit(1)
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chrome: decode screenshot: %v\n", err)
		os.Exit(1)
	}
	if out == "" {
		out = "chrome-open.png"
	}
	if err := os.WriteFile(out, raw, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "chrome: write %s: %v\n", out, err)
		os.Exit(1)
	}
	state := b.Execute(map[string]any{"method": "getState"})
	title := ""
	if st, ok := state["state"].(map[string]any); ok {
		title, _ = st["title"].(string)
	}
	fmt.Printf("done ✓  title=%q screenshot=%s (%d bytes)\n", title, out, len(raw))
}

func parseChromeArgs(args []string) (url, out string) {
	url = "https://www.baidu.com"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--out":
			if i+1 < len(args) {
				out = args[i+1]
				i++
			}
		default:
			url = args[i]
		}
	}
	return url, out
}
