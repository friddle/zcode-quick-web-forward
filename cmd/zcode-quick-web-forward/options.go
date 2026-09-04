// CLI options shared by all subcommands: common flags (--runtime-path,
// --node, --region, --workspace), interactive region/login prompts and
// workspace resolution (explicit flags, startup dir, task index, stored).

package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/friddle/zcode-quick-web-forward/internal/nodejs"
	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
)

var stdinReader = bufio.NewReader(os.Stdin)

func askRegion() string {
	fmt.Println("zcode: 请选择区域:")
	fmt.Println("  1) 国内 china  (BigModel 登录 / 阿里云下载镜像)")
	fmt.Println("  2) 国际 global (Z.AI 登录 / 官方下载源)")
	fmt.Print("选择 [1]: ")
	line, _ := stdinReader.ReadString('\n')
	switch strings.TrimSpace(line) {
	case "2", "2)", "global", "g", "国际":
		return "global"
	default:
		return "china"
	}
}

func askLoginMethod() string {
	fmt.Println("zcode: 请选择登录方式:")
	fmt.Println("  1) 登录链接 (浏览器打开授权链接, CLI 等待回调)")
	fmt.Println("  2) API Key  (BigModel API Key, https://open.bigmodel.cn/apikeys)")
	fmt.Print("选择 [1]: ")
	line, _ := stdinReader.ReadString('\n')
	switch strings.TrimSpace(line) {
	case "2", "2)", "key", "api", "apikey", "api key":
		return "key"
	default:
		return "link"
	}
}

type commonOpts struct {
	runtimePath string
	node        string
	region      string
	workspaces  multiFlag
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func parseCommon(args []string) commonOpts {
	var o commonOpts
	fs := flag.NewFlagSet("zqf", flag.ContinueOnError)
	fs.StringVar(&o.runtimePath, "runtime-path", "", "glm runtime dir; auto if empty")
	fs.StringVar(&o.node, "node", "", "node binary; auto-downloaded when missing/old")
	fs.StringVar(&o.region, "region", "", "china|global; auto-detected when empty")
	fs.Var(&o.workspaces, "workspace", "workspace path (repeatable; env ZCODE_WORKSPACE)")
	_ = fs.Parse(args)
	if o.node == "" {
		o.node = os.Getenv("ZCODE_NODE")
	}
	if o.region == "" {
		o.region = os.Getenv("ZCODE_REGION")
	}
	if v := os.Getenv("ZCODE_RUNTIME_PATH"); v != "" && o.runtimePath == "" {
		o.runtimePath = v
	}
	return o
}

func (o commonOpts) regionName() string { return nodejs.Normalize(o.region) }

func (o commonOpts) ensureNode() string {
	if o.node != "" {
		return o.node
	}
	node, err := nodejs.Ensure(o.region)
	if err != nil {
		fatal("node resolve failed: %v", err)
	}
	if node != "node" {
		fmt.Printf("zcode: using node %s\n", node)
	}
	return node
}

func (o commonOpts) resolveWorkspaces() []string {
	// explicit --workspace / ZCODE_WORKSPACE win
	explicit := append([]string{}, o.workspaces...)
	if len(explicit) == 0 {
		if v := os.Getenv("ZCODE_WORKSPACE"); v != "" {
			for _, p := range strings.Split(v, string(os.PathListSeparator)) {
				if p = strings.TrimSpace(p); p != "" {
					explicit = append(explicit, p)
				}
			}
		}
	}
	out := []string{}
	if len(explicit) > 0 {
		out = append(out, explicit...)
	} else if cwd, err := os.Getwd(); err == nil {
		out = append(out, cwd) // startup directory is the default workspace
	}
	// merge in real workspaces from the task index (dedup)
	seen := map[string]bool{}
	for _, w := range out {
		seen[filepath.Clean(w)] = true
	}
	if ws, err := zcode.Workspaces(); err == nil {
		for _, w := range ws {
			p := filepath.Clean(w.WorkspacePath)
			if !seen[p] {
				out = append(out, w.WorkspacePath)
				seen[p] = true
			}
		}
	}
	// merge in workspaces registered via `workspace add`
	for _, p := range zcode.StoredWorkspaces() {
		c := filepath.Clean(p)
		if !seen[c] {
			out = append(out, c)
			seen[c] = true
		}
	}
	return out
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "zcode: ERROR: "+format+"\n", a...)
	os.Exit(1)
}
