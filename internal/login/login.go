// Package login captures the ZCode OAuth login link printed by the app-server,
// surfaces it to the user, and waits for confirmation (auto-detection of an
// auth-success event, or the user pressing Enter to force a re-check).
package login

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/friddle/zcode-quick-web-forward/internal/appserver"
)

// linkWords are substrings that mark a line as the login URL.
var linkWords = []string{
	"login", "oauth", "authorize", "device", "signin", "sign-in",
	"auth", "callback", "account.zai", "/cli",
}

// successWords mark a line as an auth-success event.
var successWords = []string{
	"logged in", "login success", "login_success", "authenticated",
	"authentication successful", "auth.success", "welcome", "token",
	"signed in", "已登录", "登录成功", "授权成功",
}

// Handler accumulates app-server lines and surfaces the login link.
type Handler struct {
	server    *appserver.Server
	LoginURL  string
	Confirmed bool
	events    []string
}

// NewHandler wires an OnLine callback around an app-server.
func NewHandler(server *appserver.Server) *Handler {
	h := &Handler{server: server}
	server.OnLine = h.Line
	return h
}

// Line processes each app-server line, extracting the login URL and watching
// for an auth-success event.
func (h *Handler) Line(line string) {
	h.events = append(h.events, line)
	if h.LoginURL == "" {
		for _, u := range appserver.FindURLs(line) {
			if isLoginURL(u) {
				h.LoginURL = u
			}
		}
	}
	if h.Confirmed {
		return
	}
	lower := strings.ToLower(line)
	for _, w := range successWords {
		if strings.Contains(lower, w) {
			h.Confirmed = true
			return
		}
	}
}

func isLoginURL(u string) bool {
	lower := strings.ToLower(u)
	for _, w := range linkWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// Wait prints the login link (when known) and blocks until the user confirms
// login, either by an auto-detected success event or by pressing Enter.
func (h *Handler) Wait() bool {
	if h.LoginURL != "" {
		fmt.Println()
		fmt.Println("------------------------------------------")
		fmt.Println("  ZCode 登录")
		fmt.Println("  请用浏览器打开下面的链接并完成登录：")
		fmt.Println()
		fmt.Println("  " + h.LoginURL)
		fmt.Println()
		fmt.Println("  完成后按 Enter 确认（或等待自动检测到登录成功）。")
		fmt.Println("------------------------------------------")
		fmt.Println()
	}

	// Auto-detection goroutine: watch buffered events + a short poll loop.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(1500 * time.Millisecond):
				if h.Confirmed {
					return
				}
			}
		}
	}()

	// Re-evaluate past events at least once so a success already printed is seen.
	h.Confirmed = h.Confirmed || scanEvents(h.events)

	// Read stdin lines: an empty Enter means "confirm / recheck".
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 8*1024), 8*1024)
	for {
		if h.Confirmed {
			fmt.Println("zcode: 登录成功。")
			return true
		}
		if !h.server.Started {
			return false
		}
		if h.LoginURL == "" {
			fmt.Println("zcode: 尚未检测到登录链接，等待 app-server 输出……（Enter 退出）")
		}
		if !sc.Scan() {
			return false
		}
		h.Confirmed = h.Confirmed || scanEvents(h.events)
	}
}

func scanEvents(events []string) bool {
	for _, line := range events {
		lower := strings.ToLower(line)
		for _, w := range successWords {
			if strings.Contains(lower, w) {
				return true
			}
		}
	}
	return false
}
