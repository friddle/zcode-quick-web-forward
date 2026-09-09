// The `provider` subcommand: add/list/remove third-party model providers
// (SiliconFlow, DeepSeek, local zhipu gateways, ...) at the CONFIG level so
// the engine can call them directly — bypassing the zai gateway and its
// captcha requirement. Written into the same config blocks the desktop uses
// (provider.<name> with kind/enabled/source/options/models), so the model
// picker on the phone sees them natively.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// providerCmdSpec describes one third-party provider for `provider add`.
type providerCmdSpec struct {
	name    string // config key + id, e.g. "siliconflow"
	label   string // display name
	kind    string // "openai" (openai-compatible) or "anthropic"
	baseURL string
	apiKey  string
	models  []string // model ids as the upstream API expects them
	context int64    // context window tokens (display/limit)
	output  int64    // max output tokens
}

func doProvider(args []string) {
	if len(args) == 0 {
		printProviderUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "add":
		doProviderAdd(args[1:])
	case "list", "ls":
		doProviderList()
	case "remove", "rm":
		doProviderRemove(args[1:])
	default:
		printProviderUsage()
		os.Exit(1)
	}
}

func printProviderUsage() {
	fmt.Println(`usage:
  zcode-quick-web-forward provider add <name> --base-url <url> --api-key <key> --model <id> [--model <id> ...]
        [--kind openai|anthropic] [--label <display>] [--context 128000] [--output 8192]
  zcode-quick-web-forward provider list
  zcode-quick-web-forward provider remove <name>

examples:
  provider add siliconflow --base-url https://api.siliconflow.cn/v1 \
      --api-key sk-xxx --model deepseek-ai/DeepSeek-V3.1 --model Qwen/Qwen3-Coder-480B-A35B-Instruct
  provider add deepseek --base-url https://api.deepseek.com/v1 --api-key sk-xxx --model deepseek-chat
  provider add zhipu-local --base-url http://127.0.0.1:8000/v1 --api-key none --model glm-4.7

kind defaults to openai (openai-compatible); use anthropic for anthropic-format gateways.
After adding, restart the daemon (systemctl restart zqf) so the host re-syncs
the provider registry to the engine, then pick the model in 管理模型 on the phone.`)
}

func doProviderAdd(argv []string) {
	fs := flag.NewFlagSet("provider add", flag.ContinueOnError)
	spec := &providerCmdSpec{kind: "openai", context: 128000, output: 8192, label: ""}
	fs.StringVar(&spec.baseURL, "base-url", "", "upstream base URL (required)")
	fs.StringVar(&spec.apiKey, "api-key", "", "API key (required)")
	fs.StringVar(&spec.kind, "kind", "openai", "openai | anthropic")
	fs.StringVar(&spec.label, "label", "", "display name (defaults to <name>)")
	ctx := fs.Int64("context", 128000, "context window tokens")
	out := fs.Int64("output", 8192, "max output tokens")
	models := multiFlag{}
	fs.Var(&models, "model", "model id (repeatable, required)")
	if err := fs.Parse(argv); err != nil {
		os.Exit(1)
	}
	if fs.NArg() < 1 {
		printProviderUsage()
		os.Exit(1)
	}
	spec.name = strings.ToLower(strings.TrimSpace(fs.Arg(0)))
	spec.context, spec.output = *ctx, *out
	spec.models = models
	if spec.name == "" || spec.baseURL == "" || spec.apiKey == "" || len(spec.models) == 0 {
		fmt.Println("zcode: ERROR: <name>, --base-url, --api-key and at least one --model are required")
		os.Exit(1)
	}
	if spec.kind != "openai" && spec.kind != "anthropic" {
		fmt.Printf("zcode: ERROR: unsupported kind %q (openai|anthropic)\n", spec.kind)
		os.Exit(1)
	}
	if spec.label == "" {
		spec.label = strings.Title(spec.name) //nolint:staticcheck
	}
	writeProviderBothConfigs(spec)
	fmt.Printf("zcode: provider %s (%s, %s) 已写入 ~/.zcode — models: %s\n",
		spec.name, spec.label, spec.kind, strings.Join(spec.models, ", "))
	fmt.Println("zcode: 重启 daemon 生效: systemctl restart zqf，然后在手机 管理模型 里选择")
}

// writeProviderBothConfigs mirrors writeBigmodelKey: the v2 (desktop) config
// and the cli config must agree or the host/agent re-sync drops the provider.
func writeProviderBothConfigs(spec *providerCmdSpec) {
	cliPath, v2Path := zcodeConfigPaths()
	writeProviderKey(v2Path, spec.name, spec)
	writeProviderKey(cliPath, spec.name, spec)
}

func writeProviderKey(path, name string, spec *providerCmdSpec) {
	m := readJSONMap(path)
	prov, _ := m["provider"].(map[string]any)
	if prov == nil {
		prov = map[string]any{}
	}
	models := map[string]any{}
	for _, id := range spec.models {
		models[id] = map[string]any{
			"limit":      map[string]any{"context": spec.context, "output": spec.output},
			"modalities": map[string]any{"input": []string{"text"}, "output": []string{"text"}},
		}
	}
	prov[name] = map[string]any{
		"name":    spec.label,
		"kind":    spec.kind,
		"enabled": true,
		"source":  "custom",
		"options": map[string]any{
			"baseURL": spec.baseURL,
			"apiKey":  spec.apiKey,
		},
		"models": models,
	}
	m["provider"] = prov
	if err := writeJSONMap(path, m); err != nil {
		fatal("写入 %s: %v", path, err)
	}
}

func doProviderList() {
	cliPath, v2Path := zcodeConfigPaths()
	for _, p := range []struct{ label, path string }{{"cli", cliPath}, {"v2", v2Path}} {
		prov, _ := readJSONMap(p.path)["provider"].(map[string]any)
		fmt.Printf("[%s] %s\n", p.label, p.path)
		for name, v := range prov {
			pm, _ := v.(map[string]any)
			enabled, _ := pm["enabled"].(bool)
			kind, _ := pm["kind"].(string)
			models := []string{}
			if mm, ok := pm["models"].(map[string]any); ok {
				for id := range mm {
					models = append(models, id)
				}
			}
			b, _ := json.Marshal(models)
			fmt.Printf("  %-16s kind=%-9s enabled=%-5v models=%s\n", name, kind, enabled, string(b))
		}
	}
}

func doProviderRemove(argv []string) {
	if len(argv) < 1 {
		fmt.Println("usage: provider remove <name>")
		os.Exit(1)
	}
	name := argv[0]
	cliPath, v2Path := zcodeConfigPaths()
	for _, path := range []string{v2Path, cliPath} {
		m := readJSONMap(path)
		prov, _ := m["provider"].(map[string]any)
		if _, ok := prov[name]; ok {
			delete(prov, name)
			m["provider"] = prov
			if err := writeJSONMap(path, m); err != nil {
				fatal("写入 %s: %v", path, err)
			}
			fmt.Printf("zcode: provider %s removed from %s\n", name, path)
		}
	}
}
