// Model-provider payload from real ZCode config, deviceMid reuse,
// uuid/hostname helpers and the `download` subcommand.

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
)

func providerPayload() []any {
	providers := zcode.Providers()
	out := make([]any, 0, len(providers))
	for _, p := range providers {
		models := make([]any, 0, len(p.Models))
		for _, m := range p.Models {
			models = append(models, map[string]any{
				"id":              m,
				"name":            m,
				"kinds":           []any{"anthropic", "openai-compatible"},
				"modalities":      map[string]any{"input": []any{"text"}, "output": []any{"text"}},
				"contextWindow":   1000000, // engine default for the GLM models
				"maxOutputTokens": 8192,
				"deleted":         false,
			})
		}
		endpoints := map[string]any{}
		if p.BaseURL != "" {
			endpoints = map[string]any{
				"baseURL": p.BaseURL,
				"paths": map[string]any{
					"anthropic":         "/api/anthropic",
					"openai-compatible": "/v1/chat/completions",
				},
			}
		}
		out = append(out, map[string]any{
			"id":             p.ID,
			"name":           p.Name,
			"apiKey":         p.APIKey,
			"apiFormat":      p.Kind,
			"enabled":        p.Enabled,
			"source":         p.Source,
			"presetId":       p.PresetID,
			"endpoints":      endpoints,
			"models":         models,
			"headers":        map[string]any{},
			"apiKeyRequired": true,
		})
	}
	return out
}

// loadOrCreateDeviceMid reuses the ZCode desktop client's deviceMid.
func loadOrCreateDeviceMid(cache string) string {
	if home, err := os.UserHomeDir(); err == nil {
		b, err := os.ReadFile(filepath.Join(home, ".zcode", "v2", "telemetry-state.json"))
		if err == nil {
			var st struct {
				DeviceMid string `json:"deviceMid"`
			}
			if json.Unmarshal(b, &st) == nil && st.DeviceMid != "" {
				return st.DeviceMid
			}
		}
	}
	path := filepath.Join(cache, "device-mid")
	if b, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return string(bytes.TrimSpace(b))
	}
	mid := fmt.Sprintf("zqf-%s", strings.ReplaceAll(uuidNew(), "-", ""))
	_ = os.MkdirAll(cache, 0o755)
	_ = os.WriteFile(path, []byte(mid), 0o600)
	return mid
}

func uuidNew() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "zcode-quick-web-forward"
	}
	return h
}

func doDownload(args []string) {
	o := parseCommon(args)
	rt := resolveRuntime(o.runtimePath)
	fmt.Printf("zcode: download/use complete at %s\n", rt)
}
