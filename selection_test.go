package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func selectionRuntime(t *testing.T) (*runtimeState, string) {
	t.Helper()
	state := newRuntimeState()
	state.storageDir = t.TempDir()
	state.hostCall = testHost("account-a", "socks5://proxy.invalid:1080")
	now := time.Now().UTC().Truncate(time.Second)
	state.now = func() time.Time { return now }
	dir := t.TempDir()
	config := "enabled: true\nrequire_model_match: false\nprefer_292: false\nstandby_enabled: false\nmode: force\nselection_required: true\nstate_file: " + filepath.Join(dir, "current.json") + "\nselection_file: " + filepath.Join(dir, "selection.json") + "\narchive_dir: " + filepath.Join(dir, "archive") + "\ndefaults:\n  accepted_blocks: [10, 12]\n  models: [gpt-6-astra, gpt-5.6-sol]\nprobe:\n  enabled: false\n  continuous: true\n  retry_seconds: 5\n"
	if err := state.configure(jsonBytes(lifecycleRequest{ConfigYAML: []byte(config), SchemaVersion: 4})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(state.shutdown)
	return state, config
}

func responseCode(t *testing.T, raw []byte, err error) int {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		t.Fatal("invalid management envelope")
	}
	var response struct{ StatusCode int }
	if json.Unmarshal(env.Result, &response) != nil {
		t.Fatal("invalid management response")
	}
	return response.StatusCode
}

func saveSelection(t *testing.T, state *runtimeState, accounts, models []string) {
	t.Helper()
	state.mu.Lock()
	revision := state.selection.Revision
	state.mu.Unlock()
	raw, err := state.management(managementRequest{Method: "POST", Path: apiBase + "/selection", Body: jsonBytes(map[string]any{"revision": revision, "accounts": accounts, "models": models})})
	if code := responseCode(t, raw, err); code != 200 {
		t.Fatalf("selection save failed: %d", code)
	}
}

func TestSelectionRequiresBothCredentialAndModel(t *testing.T) {
	state, _ := selectionRuntime(t)
	state.config.Probe.Enabled = true
	calls := map[string]int{}
	var callsMu sync.Mutex
	state.fetch = func(_ context.Context, _ probeAuth, model string, _ *proxyEndpoint, _ proxyEndpoint) (string, string, string) {
		callsMu.Lock()
		calls[model]++
		callsMu.Unlock()
		return makeFernetToken(t, state.now(), 12), "ok", ""
	}
	state.refreshDue(context.Background())
	state.probeWG.Wait()
	if len(calls) != 0 {
		t.Fatal("unchecked startup sent requests")
	}
	begin(t, state, "unchecked", "auth-a", "gpt-6-astra", "")
	state.captureCandidate("unchecked", http.Header{turnStateHeader: {makeFernetToken(t, state.now(), 12)}})
	finish(t, state, "unchecked", "succeeded", true)
	if len(state.current) != 0 {
		t.Fatal("unchecked business response was cached")
	}
	saveSelection(t, state, []string{digest("auth-a")[:12]}, []string{"gpt-6-astra", "gpt-5.6-sol"})
	state.refreshDue(context.Background())
	state.probeWG.Wait()
	if calls["gpt-6-astra"] != 1 || calls["gpt-5.6-sol"] != 1 || len(state.current) != 2 {
		t.Fatalf("selected models were not independently acquired: %v", calls)
	}
	if _, ok := state.current[stateKey("auth-b", "gpt-6-astra")]; ok {
		t.Fatal("unchecked credential was probed")
	}
	if got := headerValue(begin(t, state, "selected", "auth-a", "gpt-6-astra", "").Headers, turnStateHeader); len(got) != 332 {
		t.Fatal("selected cache was not used")
	}
	if len(begin(t, state, "other-auth", "auth-b", "gpt-6-astra", "").Headers) != 0 {
		t.Fatal("cache crossed unchecked credential")
	}
	saveSelection(t, state, []string{digest("auth-a")[:12]}, []string{"gpt-6-astra"})
	if len(begin(t, state, "unchecked-sol", "auth-a", "gpt-5.6-sol", "").Headers) != 0 {
		t.Fatal("unchecked model still replaced a request")
	}
	if state.current[stateKey("auth-a", "gpt-5.6-sol")].Value == "" {
		t.Fatal("unchecking erased saved state")
	}
	raw, err := state.management(managementRequest{Method: "POST", Path: apiBase + "/probe", Body: jsonBytes(map[string]string{"account": digest("auth-a")[:12], "model": "gpt-5.6-sol"})})
	if responseCode(t, raw, err) != 400 {
		t.Fatal("manual probe bypassed selection")
	}
}

func TestSelectionPersistsAcrossReloadAndRejectsStaleSave(t *testing.T) {
	state, config := selectionRuntime(t)
	saveSelection(t, state, []string{digest("auth-a")[:12]}, []string{"gpt-6-astra", "gpt-5.6-sol"})
	info, err := os.Stat(state.config.SelectionFile)
	if err != nil || goruntime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("selection file is not private")
	}
	if err := state.configure(jsonBytes(lifecycleRequest{ConfigYAML: []byte(config), SchemaVersion: 4})); err != nil {
		t.Fatal(err)
	}
	if !state.pairSelectedLocked("auth-a", "gpt-5.6-sol") || state.pairSelectedLocked("auth-b", "gpt-6-astra") {
		t.Fatal("selection lost during reload")
	}
	raw, err := state.updateSelection([]byte(`{"revision":0,"accounts":[],"models":[]}`))
	if responseCode(t, raw, err) != 409 {
		t.Fatal("stale save overwrote selection")
	}
	loaded, err := loadSelection(state.config.SelectionFile)
	if err != nil || loaded.Revision != 1 || len(loaded.Models) != 2 {
		t.Fatal("stale save changed persisted choice")
	}
	saveSelection(t, state, []string{}, []string{})
	if err := state.configure(jsonBytes(lifecycleRequest{ConfigYAML: []byte(config), SchemaVersion: 4})); err != nil {
		t.Fatal(err)
	}
	if state.pairSelectedLocked("auth-a", "gpt-6-astra") {
		t.Fatal("empty selection re-enabled after reload")
	}
}

func TestDeselectCancelsProbeAndRejectsLateResult(t *testing.T) {
	state, _ := selectionRuntime(t)
	saveSelection(t, state, []string{digest("auth-a")[:12]}, []string{"gpt-6-astra"})
	state.config.Probe.Enabled = true
	started := make(chan struct{})
	done := make(chan struct{})
	token := makeFernetToken(t, state.now(), 12)
	state.fetch = func(ctx context.Context, _ probeAuth, _ string, _ *proxyEndpoint, _ proxyEndpoint) (string, string, string) {
		close(started)
		<-ctx.Done()
		return token, "ok", ""
	}
	go func() { defer close(done); state.ensureProbe("auth-a", "gpt-6-astra") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	saveSelection(t, state, []string{}, []string{"gpt-6-astra"})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("unchecking did not cancel request")
	}
	if len(state.current) != 0 {
		t.Fatal("late cancelled result was promoted")
	}
	files, _ := filepath.Glob(filepath.Join(state.config.ArchiveDir, "*.json"))
	if len(files) != 0 {
		t.Fatal("late cancelled result was archived")
	}
}

func TestSelectionDropsInflightBusinessCandidateAndChangedIdentity(t *testing.T) {
	state, _ := selectionRuntime(t)
	saveSelection(t, state, []string{digest("auth-a")[:12]}, []string{"gpt-6-astra"})
	begin(t, state, "pending", "auth-a", "gpt-6-astra", "")
	state.captureCandidate("pending", http.Header{turnStateHeader: {makeFernetToken(t, state.now(), 12)}})
	saveSelection(t, state, []string{}, []string{"gpt-6-astra"})
	finish(t, state, "pending", "succeeded", true)
	if len(state.current) != 0 {
		t.Fatal("deselected inflight business response was cached")
	}
	saveSelection(t, state, []string{digest("auth-a")[:12]}, []string{"gpt-6-astra"})
	state.hostCall = testHost("replacement-account", "socks5://proxy.invalid:1080")
	state.config.Probe.Enabled = true
	calls := 0
	state.fetch = func(context.Context, probeAuth, string, *proxyEndpoint, proxyEndpoint) (string, string, string) {
		calls++
		return "", "ok", ""
	}
	state.ensureProbe("auth-a", "gpt-6-astra")
	if calls != 0 || len(begin(t, state, "changed", "auth-a", "gpt-6-astra", "").Headers) != 0 {
		t.Fatal("credential selection crossed account replacement")
	}
}

func TestSelectionValidationAndSaveFailureAreAtomic(t *testing.T) {
	state, _ := selectionRuntime(t)
	for _, body := range []any{
		map[string]any{"revision": 0, "accounts": []string{digest("auth-a")[:12]}, "models": []string{"gpt-*"}},
		map[string]any{"revision": 0, "accounts": []string{"unknown"}, "models": []string{"gpt-6-astra"}},
	} {
		raw, err := state.updateSelection(jsonBytes(body))
		if responseCode(t, raw, err) != 400 {
			t.Fatal("invalid selection accepted")
		}
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	state.config.SelectionFile = filepath.Join(blocker, "selection.json")
	raw, err := state.updateSelection(jsonBytes(map[string]any{"revision": 0, "accounts": []string{digest("auth-a")[:12]}, "models": []string{"gpt-6-astra"}}))
	if responseCode(t, raw, err) != 500 || state.selection.Revision != 0 || len(state.selection.Accounts) != 0 {
		t.Fatal("failed save changed active selection")
	}
	raw, err = state.statusResponse()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "synthetic-token") {
		t.Fatal("selection status leaked credentials")
	}
}
