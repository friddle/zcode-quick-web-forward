package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The page's provider-settings mapper (JHt in the renderer bundle) does
// e.models.map(...) and structuredClone(e.effectiveConfig) with no guards —
// a provider entry missing models/effectiveConfig crashes the 管理模型
// dialog with "Cannot read properties of undefined (reading 'map')" and,
// through the error boundary, blocks task submission.
func TestProviderSettingsViewShape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".zcode", "cli")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"provider":{"bigmodel":{"enabled":true,"name":"BigModel","models":{"glm-5.3":{},"glm-5.3-flash":{}}}}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	view := providerSettingsView()
	providers, ok := view["providers"].([]any)
	if !ok || len(providers) == 0 {
		t.Fatalf("view must carry a non-empty providers array, got %#v", view["providers"])
	}
	for _, pv := range providers {
		p, ok := pv.(map[string]any)
		if !ok {
			t.Fatalf("provider entry must be an object, got %#v", pv)
		}
		if _, ok := p["models"].([]any); !ok {
			t.Fatalf("provider %v: models must be an array (JHt maps it unguarded), got %#v", p["providerId"], p["models"])
		}
		cfg, ok := p["effectiveConfig"].(map[string]any)
		if !ok {
			t.Fatalf("provider %v: effectiveConfig must be an object (structuredClone'd), got %#v", p["providerId"], p["effectiveConfig"])
		}
		if g, _ := cfg["group"].(string); g == "" {
			t.Fatalf("provider %v: effectiveConfig.group must be a non-empty string", p["providerId"])
		}
		models := p["models"].([]any)
		if pid, _ := p["providerId"].(string); !strings.HasPrefix(pid, "account:") && len(models) == 0 {
			t.Fatalf("provider %v: models must not be empty", p["providerId"])
		}
		for _, mv := range models {
			m, ok := mv.(map[string]any)
			if !ok {
				t.Fatalf("model entry must be an object, got %#v", mv)
			}
			for _, k := range []string{"kind", "modelId", "effectiveConfig", "issues"} {
				if _, ok := m[k]; !ok {
					t.Fatalf("provider %v model %v: missing field %q", p["providerId"], m["modelId"], k)
				}
			}
		}
	}
}

// The composer derives its runtime config from view.effectiveSelection and
// IGNORES the draft's modelSelection when the view is ready — a view without
// effectiveSelection is the "picked model never commits" bug. The selection
// arrives in the getView args; it must be validated against the view's own
// providers/models before being echoed back.
func TestModelSelectionViewEffectiveSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".zcode", "cli")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"model":{"main":"bigmodel/GLM-5.3-Flash"},"provider":{"bigmodel":{"enabled":true,"name":"BigModel","models":{"GLM-5.3":{},"GLM-5.3-Flash":{}}}}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	view := modelSelectionView(map[string]any{
		"selection": map[string]any{
			"providerId": "bigmodel",
			"modelId":    "GLM-5.3",
			"options":    map[string]any{"reasoningLevel": "high"},
		},
	})
	sel, ok := view["effectiveSelection"].(map[string]any)
	if !ok {
		t.Fatalf("effectiveSelection missing: %v", view)
	}
	if sel["providerId"] != "bigmodel" || sel["modelId"] != "GLM-5.3" {
		t.Fatalf("effectiveSelection mismatch: %v", sel)
	}
	// Unknown provider/model must NOT become effective (but preferred from
	// config.json may, when it names a real view entry).
	view = modelSelectionView(map[string]any{
		"selection": map[string]any{"providerId": "ghost", "modelId": "X"},
	})
	if sel, ok := view["effectiveSelection"].(map[string]any); ok {
		if sel["providerId"] == "ghost" {
			t.Fatalf("ghost selection echoed back: %v", sel)
		}
	}
	// No selection arg at all: fall back to preferred (config model.main).
	view = modelSelectionView(map[string]any{})
	sel, ok = view["effectiveSelection"].(map[string]any)
	if !ok {
		t.Fatalf("preferred fallback missing (config has model.main): %v", view)
	}
	if sel["modelId"] != "GLM-5.3-Flash" {
		t.Fatalf("preferred fallback = model.main, got %v", sel)
	}
}
