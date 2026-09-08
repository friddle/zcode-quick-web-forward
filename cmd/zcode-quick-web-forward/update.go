// `update` command: refresh the ZCode engine runtime only. The runtime .deb
// is downloaded from the same release channel the desktop uses, re-extracted
// and installed into the versioned cache — the daemon picks it up on the next
// start (runtime.Resolve prefers the newest cached version). The bridge binary
// itself is NOT self-updated: it is deployed out-of-band (scp / install.sh).
//
//	usage: zcode-quick-web-forward update [--check]
package main

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"

	"github.com/friddle/zcode-quick-web-forward/internal/runtime"
)

func doUpdate(args []string) {
	check := hasFlag(args, "--check")

	fmt.Printf("zcode: current version %s (%s/%s)\n", version, goruntime.GOOS, goruntime.GOARCH)

	f := runtime.NewFinder()
	if check {
		// --check reports what would happen without touching the cache: the
		// resolve order (installed app → cached version → download) prints it.
		if dir, v, err := f.Resolve(""); err != nil {
			fmt.Printf("zcode: no usable runtime: %v\n", err)
		} else {
			fmt.Printf("zcode: runtime %s at %s (已最新)\n", v, dir)
		}
		return
	}
	v, err := f.DownloadLatest()
	if err != nil {
		fatal("engine runtime update failed: %v", err)
	}
	fmt.Printf("zcode: engine runtime updated -> %s\n", v)
	pruneOldRuntimes(f.CacheRoot, v)
	fmt.Println("zcode: 完成 — 重启 remote 后生效 (runtime.Resolve 自动选最新版本)")
}

// pruneOldRuntimes removes cached runtime versions other than keep so the next
// start resolves the freshly downloaded one (findCached returns the first
// match alphabetically and could otherwise stick to an old version).
func pruneOldRuntimes(cacheRoot, keep string) {
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keep || e.Name() == "update" || e.Name() == "extract" {
			continue
		}
		if _, err := os.Stat(filepath.Join(cacheRoot, e.Name(), runtime.RuntimeName)); err != nil {
			continue // not a runtime version dir (nodejs/, etc.)
		}
		if err := os.RemoveAll(filepath.Join(cacheRoot, e.Name())); err == nil {
			fmt.Printf("zcode: removed old runtime %s\n", e.Name())
		}
	}
}
