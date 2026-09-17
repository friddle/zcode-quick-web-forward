package main

// Desktop channel shim for model-selection / provider-settings.
//
// The web-remote page (like the desktop renderer) resolves the model picker
// and the composer's provider-registry send gate through two channels the
// official host does not register headlessly. Every call came back
// "Unknown channel", the picker showed 模型加载失败/管理模型 and newer
// pages disable SEND entirely (hasUsableProvider=false). Both views are
// derived here from the same source of truth the desktop uses:
// ~/.zcode/cli/config.json (provider registry + model.main).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/friddle/zcode-quick-web-forward/internal/relay"
	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
)

// reasoningLevels mirrors the GLM thought-level variants the engine accepts
// (v2 config reasoning.variants low/max/high); the page derives the default
// selection from values.at(-1), so "high" must stay last.
var reasoningLevels = []string{"low", "medium", "high"}

// answerDesktopChannelShim answers the desktop-only channels locally.
// Returns true when the call was consumed (must not reach the host — it
// would only log "Unknown channel" and hang the page's promise).
func answerDesktopChannelShim(c *relay.ChannelCall) bool {
	if c.ChannelName != "model-selection" && c.ChannelName != "provider-settings" {
		return false
	}
	switch c.Kind {
	case relay.KindEventListen, relay.KindEventDispose:
		// on* subscriptions: no ack is expected; the views are static per
		// config and no change events are ever fired headlessly.
		return true
	case relay.KindPromise:
	default:
		return false
	}

	var result any
	switch {
	case c.ChannelName == "model-selection" && (c.Name == "getView" || c.Name == "refresh"):
		result = modelSelectionView()
	case c.ChannelName == "model-selection" && c.Name == "testModelConnectivity":
		result = map[string]any{"results": []any{}}
	case c.ChannelName == "provider-settings" && (c.Name == "getView" || c.Name == "refresh"):
		result = providerSettingsView()
	default:
		fmt.Printf("zcode: model-shim: %s.%s unsupported headless — empty ok\n", c.ChannelName, c.Name)
		result = map[string]any{}
	}
	out, err := json.Marshal(result)
	if err != nil {
		return true
	}
	officialState.mu.Lock()
	eng := officialState.engine
	officialState.mu.Unlock()
	if eng == nil {
		return true
	}
	eng.SendRawChannelBytes(relay.PromiseSuccessBytes(c.ID, out), senderSend())
	fmt.Printf("zcode: model-shim: answered %s.%s (call %d)\n", c.ChannelName, c.Name, c.ID)
	return true
}

// providerEntry is one config.json provider record relevant to the views.
type providerEntry struct {
	ID      string
	Name    string
	Enabled bool
	Models  []string // model ids in config order
}

// configProviders reads the enabled provider registry from
// ~/.zcode/cli/config.json (sorted by provider id for a stable picker).
func configProviders() []providerEntry {
	b, err := os.ReadFile(filepath.Join(zcode.Home(), "cli", "config.json"))
	if err != nil {
		return nil
	}
	var cfg struct {
		Provider map[string]struct {
			Enabled bool           `json:"enabled"`
			Name    string         `json:"name"`
			Models  map[string]any `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil
	}
	out := make([]providerEntry, 0, len(cfg.Provider))
	for id, p := range cfg.Provider {
		if !p.Enabled {
			continue
		}
		models := make([]string, 0, len(p.Models))
		for m := range p.Models {
			models = append(models, m)
		}
		sort.Strings(models)
		name := p.Name
		if name == "" {
			name = id
		}
		out = append(out, providerEntry{ID: id, Name: name, Enabled: true, Models: models})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// reasoningSpec builds the optionSpecs fragment both the page's readiness
// check and its selection validator read.
func reasoningSpec() map[string]any {
	return map[string]any{
		"optionSpecs": map[string]any{
			"reasoningLevel": map[string]any{"values": reasoningLevels},
		},
	}
}

// modelSelectionView is the model-selection.getView payload: the composer's
// picker list plus the active selection (model.main from config).
func modelSelectionView() map[string]any {
	provID, modelID := zcode.DefaultModel()
	providers := []any{}
	for _, p := range configProviders() {
		models := []any{}
		for _, m := range p.Models {
			models = append(models, map[string]any{
				"modelId":    m,
				"modelName":  m,
				"providerId": p.ID,
				"config":     reasoningSpec(),
			})
		}
		entry := map[string]any{
			"providerId":   p.ID,
			"providerName": p.Name,
			"enabled":      true,
			"models":       models,
			"config":       reasoningSpec(),
		}
		providers = append(providers, entry)
	}
	view := map[string]any{
		"revision":  configRevision(),
		"providers": providers,
	}
	if provID != "" && modelID != "" {
		view["preferredSelection"] = map[string]any{
			"providerId": provID,
			"modelId":    modelID,
			"options":    map[string]any{"reasoningLevel": reasoningLevels[len(reasoningLevels)-1]},
		}
	}
	return view
}

// providerSettingsView is the provider-settings.getView payload behind the
// 管理模型 dialog. providerTemplates stays empty: adding new providers is a
// desktop OAuth/API-key flow, not available headless.
func providerSettingsView() map[string]any {
	providers := []any{}
	for _, p := range configProviders() {
		providers = append(providers, map[string]any{
			"providerId":   p.ID,
			"providerName": p.Name,
			"enabled":      true,
			"config": map[string]any{
				"builtinModelIds": p.Models,
			},
		})
	}
	return map[string]any{
		"revision":          configRevision(),
		"providers":         providers,
		"providerTemplates": []any{},
	}
}

// configRevision turns the config mtime into a monotonic view revision so a
// provider/model edit in config.json invalidates the page's cached view.
func configRevision() int64 {
	fi, err := os.Stat(filepath.Join(zcode.Home(), "cli", "config.json"))
	if err != nil {
		return 1
	}
	return fi.ModTime().Unix()
}
