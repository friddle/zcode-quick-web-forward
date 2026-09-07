package main

// Catalogs for the composer's @ (files / plugins / skills) and / (commands /
// plugins / subagents) mention menus. Shapes were extracted from the official
// web client bundle — see docs/web-remote-protocol.md.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/friddle/zcode-quick-web-forward/internal/zcode"
)

// mentionCatalogSkipDirs are heavy/vendored directories excluded from the
// @ file mention listing.
var mentionCatalogSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true,
	".next": true, ".nuxt": true, ".cache": true, "target": true,
	"vendor": true, "__pycache__": true, ".venv": true, "venv": true,
	"coverage": true, ".turbo": true, ".output": true,
}

// listWorkspaceFileEntries walks the workspace for the @ file mention menu.
// The client maps the result directly, so it MUST be an array of
// {type, path, relativePath, name}.
func listWorkspaceFileEntries(root string) []any {
	out := []any{}
	if root == "" {
		return out
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return out
	}
	count := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if mentionCatalogSkipDirs[name] || (strings.HasPrefix(name, ".") && name != ".zcode") {
				return fs.SkipDir
			}
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		typ := "file"
		if d.IsDir() {
			typ = "directory"
		}
		out = append(out, map[string]any{
			"type":         typ,
			"path":         path,
			"relativePath": filepath.ToSlash(rel),
			"name":         name,
		})
		count++
		if count >= 3000 {
			return fs.SkipAll
		}
		return nil
	})
	return out
}

// pluginCatalogEntries lists the engine's installed plugins (the runtime
// plugin cache: <home>/cli/plugins/cache/<marketplace>/<plugin>/<version>).
// skillQualifiedNames/mcpServerNames feed the mention subtitle ("N 技能 · M MCP").
func pluginCatalogEntries() []any {
	out := []any{}
	cacheDir := filepath.Join(zcode.Home(), "cli", "plugins", "cache")
	marketplaces, err := os.ReadDir(cacheDir)
	if err != nil {
		return out
	}
	for _, mk := range marketplaces {
		if !mk.IsDir() {
			continue
		}
		plugins, err := os.ReadDir(filepath.Join(cacheDir, mk.Name()))
		if err != nil {
			continue
		}
		for _, p := range plugins {
			if !p.IsDir() {
				continue
			}
			skillNames := []any{}
			pluginDir := filepath.Join(cacheDir, mk.Name(), p.Name())
			_ = filepath.WalkDir(pluginDir, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || d.Name() != "SKILL.md" {
					return nil
				}
				skill := filepath.Base(filepath.Dir(path))
				skillNames = append(skillNames, p.Name()+":"+skill)
				return nil
			})
			out = append(out, map[string]any{
				"pluginId":             p.Name(),
				"name":                 p.Name(),
				"enabled":              true,
				"conflictingPluginIds": []any{},
				"skillQualifiedNames":  skillNames,
				"mcpServerNames":       []any{},
				"marketplace":          mk.Name(),
				"description":          "",
			})
			if len(out) >= 100 {
				return out
			}
		}
	}
	return out
}

// skillCatalogEntries collects installed skills (SKILL.md frontmatter) from
// the plugin cache and user skill directories. Plugin layouts nest several
// levels (cache/marketplace/plugin/version/skills/<skill>/SKILL.md), so walk
// depth-limited instead of guessing the tree.
func skillCatalogEntries() []any {
	out := []any{}
	seen := map[string]bool{}
	home := zcode.Home()
	roots := []string{
		filepath.Join(home, "cli", "plugins", "cache"),
		filepath.Join(home, "agents", "skills"),
		filepath.Join(home, "skills"),
	}
	for _, root := range roots {
		depth := 6
		if root != roots[0] {
			depth = 2
		}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				rel, rerr := filepath.Rel(root, path)
				if rerr == nil && strings.Count(filepath.ToSlash(rel), "/") >= depth && path != root {
					return fs.SkipDir
				}
				return nil
			}
			if d.Name() != "SKILL.md" || seen[filepath.Base(filepath.Dir(path))] {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			seen[filepath.Base(filepath.Dir(path))] = true
			// plugin name = the path segment right after the cache root's
			// marketplace dir; feeds the mention's glm-scope filter.
			plugin := ""
			if i := strings.Index(path, "plugins/cache/"); i >= 0 {
				parts := strings.Split(filepath.ToSlash(path[i+len("plugins/cache/"):]), "/")
				if len(parts) >= 2 {
					plugin = parts[1]
				}
			}
			out = append(out, map[string]any{
				"name":          filepath.Base(filepath.Dir(path)),
				"description":   skillDescription(b),
				"path":          path,
				"source":        "plugin",
				"scope":         "plugin",
				"enabled":       true,
				"plugin":        plugin,
				"qualifiedName": plugin + ":" + filepath.Base(filepath.Dir(path)),
			})
			if len(out) >= 200 {
				return fs.SkipAll
			}
			return nil
		})
	}
	return out
}

// skillDescription extracts the description line from SKILL.md frontmatter.
func skillDescription(b []byte) string {
	lines := strings.Split(string(b), "\n")
	limit := len(lines)
	if limit > 30 {
		limit = 30
	}
	for i := 0; i < limit; i++ {
		if v, ok := strings.CutPrefix(lines[i], "description:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// builtinSlashCommands seeds the composer's / menu for workspace-state
// queries (the engine's session/read carries the same builtins once a
// session exists).
func builtinSlashCommands() []any {
	return []any{
		map[string]any{"name": "goal", "description": "Show or set the current session goal.", "inputHint": "/goal [pause|resume|clear|replace <objective>|<objective>]", "source": "builtin"},
		map[string]any{"name": "compact", "description": "Compact the current conversation with optional instructions.", "inputHint": "/compact [instructions]", "source": "builtin"},
		map[string]any{"name": "init", "description": "Create or update workspace AGENTS.md instructions.", "inputHint": "/init [notes]", "source": "builtin"},
		map[string]any{"name": "plan", "description": "Switch to Plan mode and optionally send a task.", "inputHint": "/plan [task]", "source": "builtin"},
	}
}
