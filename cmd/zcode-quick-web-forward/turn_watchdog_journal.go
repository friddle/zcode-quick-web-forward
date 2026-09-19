package main

// Staged-input journal: the dead-turn resurrect path keeps the captured
// sendText only in memory, so a daemon exit (canary, deploy, crash) dropped
// it and the tasks sat idle forever after restart — observed 2026-09-17
// 17:42 → 03:49. The journal mirrors rec.stagedRetry to a small JSON file;
// on boot the entries are re-armed. A session whose turn is already running
// keeps its staged text for the normal dead-turn path; everything else gets
// ONE clean re-inject per boot (the sendText reactivates the persisted
// session in a fresh engine — the host loads it on demand).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const stagedJournalMaxAge = 24 * time.Hour

func stagedRetryJournalPath() string {
	if p := os.Getenv("ZQF_STAGED_JOURNAL"); p != "" {
		return p
	}
	if cache, err := os.UserCacheDir(); err == nil {
		return filepath.Join(cache, "zcode-quick-web-forward", "staged-retry.json")
	}
	return "zqf-staged-retry.json"
}

type stagedJournalEntry struct {
	Text string `json:"text"`
	Mode string `json:"mode,omitempty"`
	At   int64  `json:"at"`
	Tries int   `json:"tries,omitempty"`
}

// writeStagedJournalLocked mirrors the staged map to disk. Caller holds r.mu.
func writeStagedJournalLocked(r *officialRecovery) {
	out := map[string]stagedJournalEntry{}
	for sid, text := range r.stagedRetry {
		if text == "" {
			continue
		}
		at := r.stagedRetryAt[sid]
		if r.stagedRetryTaken[sid] && at == 0 {
			continue
		}
		out[sid] = stagedJournalEntry{Text: text, Mode: r.stagedModeBy[sid], At: at, Tries: r.stagedTries[sid]}
	}
	// Mode-only entries (text consumed, mode still valid) must survive the
	// rebuild or the next staged write would silently drop them.
	for sid, mode := range r.stagedModeBy {
		if mode == "" {
			continue
		}
		if e, ok := out[sid]; ok {
			e.Mode = mode
			out[sid] = e
			continue
		}
		at := r.stagedRetryAt[sid]
		if at == 0 {
			at = time.Now().UnixMilli()
		}
		out[sid] = stagedJournalEntry{Mode: mode, At: at, Tries: r.stagedTries[sid]}
	}
	writeStagedJournal(out)
}

func writeStagedJournal(out map[string]stagedJournalEntry) {
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	path := stagedRetryJournalPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// journalOutboxText records an outbox task BEFORE the inject runs. If the
// inject wedges the daemon (the rec.mu wedge that eats whole inject bursts)
// the text survives the canary exit + supervisor restart, and
// rearmStagedOnBoot re-delivers it — the outbox file itself is consumed at
// read time and cannot do this.
func journalOutboxText(sid, text string) {
	if sid == "" || strings.TrimSpace(text) == "" {
		return
	}
	if rec := officialActiveRec(); rec != nil {
		rec.setStagedRetry(sid, text)
		return
	}
	path := stagedRetryJournalPath()
	b, _ := os.ReadFile(path)
	out := map[string]stagedJournalEntry{}
	_ = json.Unmarshal(b, &out)
	e := out[sid] // preserve a journaled mode across the text update
	e.Text = text
	e.At = time.Now().UnixMilli()
	out[sid] = e
	writeStagedJournal(out)
}

// journalOutboxMode records an outbox `mode` submission for sid: the mode the
// session's tasks must run under. Replayed before every staged re-inject
// (see setStagedRetryMode).
func journalOutboxMode(sid, mode string) {
	if sid == "" || strings.TrimSpace(mode) == "" {
		return
	}
	if rec := officialActiveRec(); rec != nil {
		rec.setStagedRetryMode(sid, mode)
		return
	}
	path := stagedRetryJournalPath()
	b, _ := os.ReadFile(path)
	out := map[string]stagedJournalEntry{}
	_ = json.Unmarshal(b, &out)
	e := out[sid]
	e.Mode = strings.TrimSpace(mode)
	if e.At == 0 {
		e.At = time.Now().UnixMilli()
	}
	out[sid] = e
	writeStagedJournal(out)
}

// loadStagedJournal seeds the in-memory staged retry from disk at boot.
func loadStagedJournal(r *officialRecovery) {
	b, err := os.ReadFile(stagedRetryJournalPath())
	if err != nil {
		return
	}
	var in map[string]stagedJournalEntry
	if json.Unmarshal(b, &in) != nil || len(in) == 0 {
		return
	}
	now := time.Now().UnixMilli()
	r.mu.Lock()
	n := 0
	for sid, e := range in {
		if e.At == 0 || now-e.At > stagedJournalMaxAge.Milliseconds() {
			continue
		}
		if e.Mode != "" {
			if r.stagedModeBy == nil {
				r.stagedModeBy = map[string]string{}
			}
			r.stagedModeBy[sid] = e.Mode
		}
		if e.Text == "" {
			// Mode-only entry: the staged text was consumed, but keep the
			// daemon-injected marker alive (the headless auto-approve gate
			// keys off stagedRetryAt) without re-arming any text.
			if r.stagedRetryAt == nil {
				r.stagedRetryAt = map[string]int64{}
			}
			if r.stagedRetryAt[sid] == 0 {
				r.stagedRetryAt[sid] = e.At
			}
			continue
		}
		if r.stagedRetry == nil {
			r.stagedRetry = map[string]string{}
		}
		if r.stagedRetryAt == nil {
			r.stagedRetryAt = map[string]int64{}
		}
		if r.stagedRetryTaken == nil {
			r.stagedRetryTaken = map[string]bool{}
		}
		if r.stagedTries == nil {
			r.stagedTries = map[string]int{}
		}
		r.stagedTries[sid] = e.Tries
		if e.Tries >= stagedInjectMaxTries {
			// Poisoned input: this text already burned its inject budget
			// without ever producing a completed turn (the sess_ba174238
			// incident — 126 injections, each re-queued startNow, each
			// engine start wedging on the same input). Never re-arm it.
			fmt.Printf("zcode: watchdog: %s staged input hit %d tries — NOT re-arming from journal\n", shortSid(sid), e.Tries)
			continue
		}
		if _, taken := r.stagedRetryTaken[sid]; !taken {
			r.stagedRetry[sid] = e.Text
			r.stagedRetryAt[sid] = e.At
			n++
		}
	}
	r.mu.Unlock()
	if n > 0 {
		fmt.Printf("zcode: watchdog: re-armed %d staged input(s) from journal %s\n", n, stagedRetryJournalPath())
	}
}

// rearmStagedOnBoot re-delivers journaled inputs that never produced a turn:
// after the host settles, anything not running gets one re-inject (which
// re-stages and re-supervises through the normal ladder).
func rearmStagedOnBoot(b *officialHostBridge) {
	if b == nil {
		return
	}
	time.Sleep(45 * time.Second)
	if !b.h.Alive() {
		return
	}
	rec := b.rec
	now := time.Now().UnixMilli()
	rec.mu.Lock()
	pending := map[string]string{}
	for sid, text := range rec.stagedRetry {
		if text == "" || rec.stagedRetryTaken[sid] {
			continue
		}
		if at := rec.stagedRetryAt[sid]; at == 0 || now-at > stagedJournalMaxAge.Milliseconds() {
			continue
		}
		pending[sid] = text
	}
	rec.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	fmt.Printf("zcode: watchdog: boot re-arm: %d staged input(s) pending\n", len(pending))
	for sid, text := range pending {
		facts := parseSessionFacts(rec.snapFor(sid))
		if facts.status == "running" || facts.status == "in-progress" || facts.status == "active" {
			fmt.Printf("zcode: watchdog: boot re-arm: %s turn already running — leaving staged input in place\n", shortSid(sid))
			continue
		}
		injectStagedWithMode(b, sid, text)
	}
}
