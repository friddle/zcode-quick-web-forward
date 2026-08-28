// Package bridge serves a small local web page that shows the ZCode state
// (login link, local address, mobile link) and relays status to a browser,
// so the running app-server can be reached from the local machine. The tunnel
// package then forwards this local address out to mobile.
package bridge

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strings"
	"sync"
)

// State is the current run state shown on the hub page.
type State struct {
	LoginURL  string `json:"login_url"`
	Confirmed bool   `json:"confirmed"`
	LocalURL  string `json:"local_url"`
	MobileURL string `json:"mobile_url"`
	Version   string `json:"zcode_version"`
	Runtime   string `json:"runtime_dir"`
	Message   string `json:"message"`
}

// Hub owns a local web server and the shared mutable state.
type Hub struct {
	mu    sync.RWMutex
	state State
	ln    net.Listener
	svr   *http.Server
}

// NewHub creates a hub bound to 127.0.0.1.<port> (or 0.0.0.0 with --host).
func NewHub(host string, port int) (*Hub, error) {
	h := &Hub{}
	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	actual := ln.Addr().String()
	if strings.HasPrefix(actual, "[::]") {
		actual = "0.0.0.0" + actual[len("[::]"):]
	}
	h.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.root)
	mux.HandleFunc("/api/state", h.apiState)
	h.svr = &http.Server{Handler: mux}
	return h, nil
}

// Set updates the shared state.
func (h *Hub) Set(upd func(*State)) {
	h.mu.Lock()
	upd(&h.state)
	h.mu.Unlock()
}

// Snapshot returns a copy of the shared state.
func (h *Hub) Snapshot() State {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.state
}

// Addr returns the local listener address (host:port).
func (h *Hub) Addr() string { return h.ln.Addr().String() }

// Serve runs the hub web server until Stop is called (blocking).
func (h *Hub) Serve() error {
	return h.svr.Serve(h.ln)
}

// Stop shuts the web server down.
func (h *Hub) Stop() {
	_ = h.svr.Close()
}

func (h *Hub) apiState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.Snapshot())
}

var pageTmpl = template.Must(template.New("hub").Parse(`<!doctype html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ZCode Quick Web Forward</title>
<style>
 body{font-family:system-ui,sans-serif;background:#0f1115;color:#e6e6e6;margin:0;padding:24px;line-height:1.6}
 .card{max-width:720px;margin:0 auto;background:#16191f;border:1px solid #2a2f3a;border-radius:12px;padding:24px}
 h1{font-size:20px;margin-top:0} .row{margin:14px 0}.lbl{color:#8b93a3;font-size:12px}
 a{color:#4ea1ff}.big{font-size:15px;word-break:break-all}
 .ok{color:#3ddc84}.wait{color:#f2c94c}.err{color:#ff6b6b}
 pre{background:#0a0c10;padding:12px;border-radius:8px;overflow:auto}
</style>
</head>
<body>
<div class="card">
<h1>ZCode Quick Web Forward</h1>
<div class="row"><span class="lbl">ZCode 版本</span><div class="big" id="ver">{{.Version}}</div></div>
<div class="row"><span class="lbl">Runtime</span><pre id="rt">{{.Runtime}}</pre></div>
<div class="row"><span class="lbl">登录状态</span>
 <div class="big"><span id="conf" class="wait">{{if .Confirmed}}已登录<span class="ok">✓</span>{{else}}等待登录{{end}}</span></div></div>
<div class="row"><span class="lbl">登录链接</span><div class="big" id="login"><a href="{{.LoginURL}}">{{.LoginURL}}</a></div></div>
<div class="row"><span class="lbl">本机地址</span><div class="big" id="local">{{.LocalURL}}</div></div>
<div class="row"><span class="lbl">手机 / 远程地址</span><div class="big" id="mobile">{{.MobileURL}}</div></div>
<div class="row"><span class="lbl">消息</span><div class="big" id="msg">{{.Message}}</div></div>
</div>
<script>
function refresh(){
 fetch('/api/state').then(r=>r.json()).then(s=>{
  document.getElementById('ver').textContent=s.zcode_version;
  document.getElementById('rt').textContent=s.runtime_dir;
  const c=document.getElementById('conf');
  if(s.confirmed){c.className='ok';c.textContent='已登录 ✓';}
  else {c.className='wait';c.textContent='等待登录';}
  const lg=document.getElementById('login');
  if(s.login_url){lg.innerHTML='<a href="'+s.login_url+'">'+s.login_url+'</a>';}
  else lg.textContent='——';
  document.getElementById('local').textContent=s.local_url;
  document.getElementById('mobile').textContent=s.mobile_url||'——';
  document.getElementById('msg').textContent=s.message;
 });
}
refresh();setInterval(refresh,2000);
</script>
</body>
</html>`))

func (h *Hub) root(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTmpl.Execute(w, h.Snapshot())
}
