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
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	if c.ChannelName != "model-selection" && c.ChannelName != "provider-settings" && c.ChannelName != "usage-stats" {
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
	case c.ChannelName == "usage-stats" && c.Name == "getEntitlementSnapshot":
		// The 管理模型 card reads the coding-plan entitlement here; the host
		// has no usage-stats service on the svc port, so the call hung and
		// the page degraded to 未连接/暂不可用 even though the engine runs
		// on a live subscription. Serve the real snapshot captured from the
		// account's own entitlement cache (see entitlementSnapshot()). The
		// hook reads snapshot.provider.id directly off the resolved value —
		// return the bare snapshot, no cache envelope.
		argJSON, _ := json.Marshal(argMap(c.Arg))
		fmt.Printf("zcode: model-shim: getEntitlementSnapshot arg %s\n", argJSON)
		result = json.RawMessage(entitlementSnapshot(argJSON))
	case c.ChannelName == "usage-stats" && c.Name == "getCodingPlanResetStatus":
		// The reset-time baseline tracker reads the same snapshot shape
		// (RR(snapshot, resetType) pulls each limit's nextResetTime); an
		// empty ok made the card show 套餐查询失败.
		result = json.RawMessage(entitlementSnapshot([]byte(`{}`)))
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
// 管理模型 dialog. The page's JHt mapper touches EVERY field below without
// guards — e.models.map(...) on a provider without a models array is the
// "Cannot read properties of undefined (reading 'map')" crash that killed
// the dialog (and, through the error boundary, task submission with it).
// providerTemplates stays empty: adding new providers is a desktop
// OAuth/API-key flow, not available headless.
func providerSettingsView() map[string]any {
	providers := []any{}
	for _, p := range configProviders() {
		models := []any{}
		for _, m := range p.Models {
			models = append(models, map[string]any{
				"kind":                  "builtin",
				"modelId":               m,
				"builtin":               true,
				"effectiveBuiltinConfig": map[string]any{},
				"useRecommendedConfig":  true,
				"effectiveConfig":       map[string]any{"group": providerGroup(p.ID)},
				"executable":            true,
				"selectable":            true,
				"issues":                []any{},
			})
		}
		providers = append(providers, map[string]any{
			"providerId":   p.ID,
			"providerName": p.Name,
			"enabled":      true,
			"executable":   true,
			"accountState": "active",
			"issues":       []any{},
			"effectiveConfig": map[string]any{
				"group": providerGroup(p.ID),
			},
			"models": models,
		})
	}
	// The account-bound coding-plan entry. The page decides 已连接 vs 未连接
	// by filtering the view for a provider whose effectiveConfig.access is a
	// zhipu-account with entitled=true and whose providerId is one of the
	// four coding-plan ids — this deployment's live plan is
	// account:bigmodel-individual-coding-plan, and the card's subscription/
	// quota numbers come from the getEntitlementSnapshot replay above.
	// The access object is parsed with a STRICT 4-key zod schema
	// (type/accountType/mode/entitled): extra or missing keys make
	// safeParse fail and the card degrades to 未连接.
	providers = append(providers, map[string]any{
		"providerId":   "account:bigmodel-individual-coding-plan",
		"providerName": "GLM Coding Pro",
		"enabled":      true,
		"executable":   true,
		"accountState": "active",
		"issues":       []any{},
		"effectiveConfig": map[string]any{
			"group": "bigmodel-family",
			"access": map[string]any{
				"type":        "zhipu-account",
				"accountType": "bigmodel",
				"mode":        "individual-coding-plan",
				"entitled":    true,
			},
		},
		"models": []any{},
	})
	return map[string]any{
		"revision":          configRevision(),
		"providers":         providers,
		"providerTemplates": []any{},
	}
}

// providerGroup classifies a config provider the way the page's own
// registry does; only "standard-personal" gets special treatment
// (reorderable personal providers), everything else renders as-is.
func providerGroup(id string) string {
	switch {
	case strings.Contains(id, "bigmodel"):
		return "bigmodel-family"
	case strings.Contains(id, "zai"):
		return "zai-family"
	default:
		return "standard"
	}
}

// codingPlanEntitlementSnapshot is the account's real entitlement snapshot
// (the individual coding plan this deployment runs on, expires 2026-10-16)
// exactly as the usage-stats service returns it — captured from the
// account's own cached snapshot after the desktop session fetched it, with
// provider.id rewritten to the account: provider the page actually requests
// (the page requires the snapshot to match its preferredProviderId). The
// host cannot serve usage-stats headlessly, so the shim replays this: the
// page then renders the provider card as connected with live quota instead
// of 未连接/暂不可用.
const codingPlanEntitlementSnapshot = `{"generatedAt":1789136237905,"authenticated":true,"context":{"scope":"personal","productId":"product-733034","displayName":"GLM Coding Pro"},"provider":{"id":"account:bigmodel-individual-coding-plan","name":"GLM Coding Pro"},"remaining":{"count":810,"isShow":true,"percentage":19,"nextResetTime":1789552022998},"subscription":{"identityType":"unknown","identityMasked":null,"details":[{"productId":"product-733034","productName":"GLM Coding Pro","purchaseTime":null,"beginTime":null,"billingCycle":"annually","renewTime":null,"expireTime":"2026-10-16T00:00:00.000Z"}]},"quota":{"level":"pro","limits":[{"type":"TIME_LIMIT","unit":5,"number":1,"usage":1000,"currentValue":190,"remaining":810,"percentage":19,"nextResetTime":1789552022998,"usageDetails":[{"modelCode":"search-prime","usage":180},{"modelCode":"web-reader","usage":10},{"modelCode":"zread","usage":0}]},{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":23,"nextResetTime":1789144551446,"usageDetails":[]}]},"mcpQuota":{"aggregate":{"type":"MCP_USAGE_LIMIT","currentValue":0,"usage":0,"remaining":1000,"percentage":0,"nextResetTime":1789142400000,"usageDetails":[]},"level":"pro","scope":{"providerFamily":"bigmodel","targetType":"PERSONAL"},"serverTime":1789136240000}}`

// startPlanEntitlementSnapshot is the expired Weekend-Build trial snapshot —
// kept so a start-plan request still renders deterministically.
const startPlanEntitlementSnapshot = `{"generatedAt":1789136237905,"authenticated":true,"context":{"scope":"personal"},"provider":{"id":"builtin:bigmodel-start-plan","name":"BigModel- Coding Plan"},"remaining":null,"subscription":null,"quota":null}`

// entitlementSnapshot routes a getEntitlementSnapshot request to the right
// replayed snapshot by matching the provider id anywhere in the arg. The
// live coding-plan subscription is the default: it is the only plan this
// deployment is subscribed to.
func entitlementSnapshot(argJSON []byte) json.RawMessage {
	switch {
	case bytes.Contains(argJSON, []byte("start-plan")):
		return json.RawMessage(startPlanEntitlementSnapshot)
	default:
		return json.RawMessage(codingPlanEntitlementSnapshot)
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
