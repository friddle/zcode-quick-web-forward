// Package oauth implements the Z.AI / ZCode OAuth authorization-code login
// that the desktop app performs: build the authorize URL, host a local
// callback server, then hand the returned {callbackUrl, state} to the runtime
// (which holds the appSecret and exchanges the code) by spawning:
//
//	node <runtime>/zcode.cjs   with env ZCODE_CLI_OAUTH_CALLBACK_STDIN=1
//
// and sending one JSON payload on stdin. Login success == that child exits 0.
package oauth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// AuthorizeEndpoint and ClientID are the ones the official Z.AI OAuth app uses
// (as embedded in the zcode-cli integration).
const (
	AuthorizeEndpoint = "https://chat.z.ai/api/oauth/authorize"
	ClientID          = "client_P8X5CMWmlaRO9O-KSqtg"
	SchemeCallback    = "zcode://zai-auth/callback"
)

// Result is the outcome of a completed browser login.
type Result struct {
	CallbackURL string
	State       string
	Code        string
	Confirmed   bool
}

// State generates a fresh 32-byte hex OAuth state.
func State() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// AuthorizeURL builds the browser login link for the given state and callback.
func AuthorizeURL(state, callback string) string {
	q := url.Values{}
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", callback)
	q.Set("response_type", "code")
	q.Set("state", state)
	return AuthorizeEndpoint + "?" + q.Encode()
}

// CallbackServer listens locally for the OAuth redirect, verifies the state,
// and emits the final Result.
type CallbackServer struct {
	state string
	host  string
	port  int
	ln    net.Listener
	srv   *http.Server
	done  chan Result
}

// StartCallback binds a local callback listener on host:port (port 0 = random)
// and serves /callback plus /login.html.
func StartCallback(host string, port int) (*CallbackServer, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &CallbackServer{host: host, ln: ln, done: make(chan Result, 1)}
	c.port = ln.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", c.handleCallback)
	mux.HandleFunc("/login.html", c.handleLogin)
	mux.HandleFunc("/", c.handleLogin)
	c.srv = &http.Server{Handler: mux}
	return c, nil
}

func (c *CallbackServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	au := AuthorizeURL(c.state, c.CallbackURI())
	_, _ = w.Write(LoginPage(au))
}

func (c *CallbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("error") != "" {
		writeHTML(w, "登录失败", q.Get("error_description"))
		c.done <- Result{Confirmed: false}
		return
	}
	state := q.Get("state")
	if state != c.state {
		writeHTML(w, "state 不匹配", "OAuth state mismatch, 请重试")
		c.done <- Result{Confirmed: false}
		return
	}
	code := q.Get("code")
	if code == "" {
		writeHTML(w, "缺少授权码", "no authorization code returned")
		c.done <- Result{Confirmed: false}
		return
	}
	writeHTML(w, "登录成功", "授权码已接收，正在完成 ZCode 登录…")
	c.done <- Result{CallbackURL: r.URL.String(), State: state, Code: code, Confirmed: true}
}

// Serve runs the callback HTTP server until Stop is called (blocking).
func (c *CallbackServer) Serve() error { return c.srv.Serve(c.ln) }

// Stop shuts the callback HTTP server down.
func (c *CallbackServer) Stop() { _ = c.srv.Close() }

// CallbackURI is the full redirect URI the browser is sent back to.
func (c *CallbackServer) CallbackURI() string {
	return fmt.Sprintf("http://%s:%d/callback", c.host, c.port)
}

// LoginURI is the local page containing the login button.
func (c *CallbackServer) LoginURI() string {
	return fmt.Sprintf("http://%s:%d/login.html", c.host, c.port)
}

// SetState records the expected OAuth state.
func (c *CallbackServer) SetState(state string) { c.state = state }

// Done returns the channel that emits the final login Result.
func (c *CallbackServer) Done() <-chan Result { return c.done }

func writeHTML(w http.ResponseWriter, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>%s</title><body style='font-family:sans-serif;text-align:center;padding-top:60px'><h1>%s</h1><p>%s</p><p><a href='javascript:history.back()'>返回</a></p></body>", title, title, msg)
}

// Complete hands {callbackUrl, state} as JSON on stdin to a runtime spawned
// with ZCODE_CLI_OAUTH_CALLBACK_STDIN=1, and reports login success (exit 0).
func Complete(node, runtimePath string, payload interface{}) (bool, error) {
	script := runtimePath
	if !strings.HasSuffix(script, ".cjs") {
		script = filepath.Join(script, "zcode.cjs")
	}
	cmd := exec.Command(node, script)
	cmd.Dir = filepath.Dir(script)
	cmd.Env = append(os.Environ(), "ZCODE_CLI_OAUTH_CALLBACK_STDIN=1")
	data, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	cmd.Stdin = strings.NewReader(string(data))
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return false, err
	}
	err = cmd.Wait()
	return err == nil, nil
}

// LoginPage renders a minimal page with a "登录" button that opens the URL.
func LoginPage(authorizeURL string) []byte {
	page := `<!doctype html><meta charset=utf-8><meta name="viewport" content="width=device-width,initial-scale=1">
<title>ZCode 登录</title>
<style>body{font-family:system-ui,sans-serif;background:#0f1115;color:#e6e6e6;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}
button{background:#3b82f6;color:#fff;border:0;padding:14px 28px;font-size:18px;border-radius:8px;cursor:pointer}a{color:#4ea1ff;display:block;margin-top:14px;word-break:break-all}</style>
<div style="text-align:center">
<h1>ZCode 登录</h1>
<button onclick="location.href='%s'">立刻登录</button>
<a href="%s" target="_blank">在浏览器打开登录链接</a>
</div>`
	return []byte(fmt.Sprintf(page, authorizeURL, authorizeURL))
}
