// Interactive login flow (run/logincli): region + login-method prompts,
// official `login --no-browser` link flow, and the BigModel API-key path
// (key validation, ~/.zcode v2+cli config writes).

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runLoginCLI drives the interactive entry: region, login method, then remote.
func runLoginCLI(args []string) {
	o := parseCommon(args)
	rt := resolveRuntime(o.runtimePath)
	node := o.ensureNode()

	if o.region == "" && os.Getenv("ZCODE_REGION") == "" {
		o.region = askRegion()
	}
	region := o.regionName()

	method := "link"
	switch {
	case os.Getenv("BIGMODEL_API_KEY") != "":
		method = "key"
	case region == "china":
		method = askLoginMethod()
	}

	switch method {
	case "key":
		fmt.Println("zcode: 登录方式: BigModel API Key")
		bigmodelLogin()
	default:
		fmt.Println("zcode: 登录方式: 登录链接 (官方 login --no-browser)")
		fmt.Println("zcode: 会打印登录链接 -> 请在浏览器打开 -> 授权回调。")
		args2 := []string{"login", "--no-browser"}
		fmt.Printf("zcode: $ %s %s %s\n", node, scriptPath(rt), strings.Join(args2, " "))
		if err := runCLI(node, scriptPath(rt), args2); err != nil {
			fatal("登录失败: %v", err)
		}
	}
	fmt.Println("zcode: 登录完成。启动 app-server 并生成 web-remote 链接 ...")
	doRemoteOpts(o)
}

// ---- BigModel login (domestic) ----

const (
	bigmodelBaseURL = "https://open.bigmodel.cn/api/anthropic"
	bigmodelModel   = "bigmodel/GLM-5.3"
)

// bigmodelLogin ensures a BigModel API key is configured in ~/.zcode: an
// existing key wins, else BIGMODEL_API_KEY / an interactive prompt supplies
// it. The key is validated against BigModel's Anthropic-compatible /v1/models
// endpoint before being written into the desktop config (builtin:bigmodel)
// and the CLI config (provider/bigmodel + model/main).
func bigmodelLogin() {
	cliPath, v2Path := zcodeConfigPaths()
	key := os.Getenv("BIGMODEL_API_KEY")
	if key == "" {
		if hasBigmodelKey(cliPath) || hasBigmodelKey(v2Path) {
			fmt.Println("zcode: BigModel API key 已配置。")
			return
		}
		fmt.Print("zcode: 请输入 BigModel API Key (https://open.bigmodel.cn/apikeys 获取): ")
		line, err := stdinReader.ReadString('\n')
		if err != nil {
			fatal("读取 API key 失败: %v (非交互环境请 export BIGMODEL_API_KEY=...)", err)
		}
		key = strings.TrimSpace(line)
		if key == "" {
			fatal("BigModel API key 为空")
		}
	}
	if err := validateBigmodelKey(key); err != nil {
		fatal("BigModel API key 校验失败: %v", err)
	}
	writeBigmodelKey(v2Path, "builtin:bigmodel", key)
	writeBigmodelKey(cliPath, "bigmodel", key)
	setMainModel(cliPath, bigmodelModel)
	fmt.Println("zcode: BigModel API key 校验通过,已写入 ~/.zcode (v2 + cli 配置)。")
}

func validateBigmodelKey(key string) error {
	req, err := http.NewRequest(http.MethodGet, bigmodelBaseURL+"/v1/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	switch {
	case resp.StatusCode == http.StatusOK:
		var v struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body, &v)
		ids := make([]string, 0, len(v.Data))
		for _, m := range v.Data {
			ids = append(ids, m.ID)
		}
		fmt.Printf("zcode: key 有效,可用模型 %d 个: %s\n", len(ids), strings.Join(ids, ", "))
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("key 被拒绝 (HTTP %d)", resp.StatusCode)
	default:
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

func zcodeConfigPaths() (cliPath, v2Path string) {
	home, err := os.UserHomeDir()
	if err != nil {
		fatal("user home: %v", err)
	}
	base := filepath.Join(home, ".zcode")
	return filepath.Join(base, "cli", "config.json"), filepath.Join(base, "v2", "config.json")
}

func readJSONMap(path string) map[string]any {
	m := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func writeJSONMap(path string, m map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func hasBigmodelKey(path string) bool {
	prov, _ := readJSONMap(path)["provider"].(map[string]any)
	for _, name := range []string{"bigmodel", "builtin:bigmodel"} {
		p, _ := prov[name].(map[string]any)
		opts, _ := p["options"].(map[string]any)
		if s, _ := opts["apiKey"].(string); s != "" {
			return true
		}
	}
	return false
}

func writeBigmodelKey(path, name, key string) {
	m := readJSONMap(path)
	prov, _ := m["provider"].(map[string]any)
	if prov == nil {
		prov = map[string]any{}
	}
	p, _ := prov[name].(map[string]any)
	if p == nil {
		p = map[string]any{}
	}
	opts, _ := p["options"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
	}
	if _, ok := opts["baseURL"]; !ok {
		opts["baseURL"] = bigmodelBaseURL
	}
	opts["apiKey"] = key
	p["options"] = opts
	prov[name] = p
	m["provider"] = prov
	if err := writeJSONMap(path, m); err != nil {
		fatal("写入 %s: %v", path, err)
	}
}

func setMainModel(path, model string) {
	m := readJSONMap(path)
	mo, _ := m["model"].(map[string]any)
	if mo == nil {
		mo = map[string]any{}
	}
	mo["main"] = model
	m["model"] = mo
	if err := writeJSONMap(path, m); err != nil {
		fatal("写入 %s: %v", path, err)
	}
}
