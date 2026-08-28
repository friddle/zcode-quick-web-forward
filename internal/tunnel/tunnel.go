// Package tunnel forwards the local web hub out to mobile / another machine.
// It supports a pluggable tunnel command (piko / ssh -R) and a plain mode.
package tunnel

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var urlRe = regexp.MustCompile(`https?://[A-Za-z0-9._/\-?&=:%#~+{}$]+`)

// Options configures a tunnel start.
type Options struct {
	LocalURL string // local address to forward, e.g. http://127.0.0.1:PORT
	Mode     string // "none" | "piko" | "ssh" | "auto"
	Command  []string
	Host     string // ssh jump host when Mode=="ssh"
}

// Start opens the tunnel and returns the public URL to print for mobile.
// In "none" mode it returns the local URL unchanged.
func Start(opts Options) (string, string, error) {
	switch opts.Mode {
	case "none", "local", "":
		return opts.LocalURL, "local", nil
	case "ssh":
		hostPort := localHost(opts.LocalURL)
		jump := opts.Host
		if jump == "" {
			jump = "localhost"
		}
		cmd := exec.Command("ssh", "-N", "-R", "localhost:"+hostPort, jump)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			return "", "", err
		}
		fmt.Println("tunnel(ssh): reverse-forward", hostPort, "over", jump, "started.")
		return opts.LocalURL, "ssh", nil
	default: // piko / auto
		bin := opts.Command
		if len(bin) == 0 {
			bin = []string{"piko"}
		}
		path, err := exec.LookPath(bin[0])
		if err != nil || path == "" {
			fmt.Printf("tunnel: %s not found; falling back to local/LAN URL.\n", bin[0])
			return opts.LocalURL, "local", nil
		}
		cmd := exec.Command(path, bin[1:]...)
		pipe, _ := cmd.StdoutPipe()
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			return "", "", err
		}
		url := <-scanURLs(pipe)
		if url == "" {
			return opts.LocalURL, "piko", fmt.Errorf("tunnel started but no public URL printed")
		}
		return url, "piko", nil
	}
}

func localHost(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.Index(u, "/"); i >= 0 {
		u = u[:i]
	}
	return u
}

// scanURLs reads a tunnel child's stdout until it finds a URL, then returns it.
func scanURLs(r interface{}) chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		br, ok := r.(interface{ Read([]byte) (int, error) })
		if !ok {
			ch <- ""
			return
		}
		sc := bufio.NewScanner(br)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			for _, u := range urlRe.FindAllString(line, -1) {
				if strings.Contains(u, "://") {
					ch <- u
					return
				}
			}
		}
		ch <- ""
	}()
	return ch
}

// Print shows the given public / local URL prominently.
func Print(kind, url string) {
	fmt.Println()
	fmt.Println("------------------------------------------")
	fmt.Println("  " + kind + " 访问链接：")
	fmt.Println()
	fmt.Println("  " + url)
	fmt.Println()
	fmt.Println("  在手机上打开上面的链接即可访问 ZCode。")
	fmt.Println("------------------------------------------")
	fmt.Println()
	_ = os.Stdout.Sync()
}
