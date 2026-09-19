package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFreshStoreInstallWorksWithoutPluginParameters(t *testing.T) {
	state := newRuntimeState()
	state.storageDir = t.TempDir()
	state.hostCall = testHost("account-a", "socks5://credential.invalid:1080")
	started := make(chan struct{}, 1)
	state.fetch = func(_ context.Context, auth probeAuth, model string, first *proxyEndpoint, endpoint proxyEndpoint) (string, string, string) {
		if first != nil || endpoint.URL != auth.ProxyURL {
			t.Error("default setup escaped credential proxy")
		}
		started <- struct{}{}
		return makeFernetToken(t, time.Now().UTC().Truncate(time.Second), 10), "ok", model
	}
	cfg := []byte("enabled: true\npriority: 0\nstore:\n  id: codex-turn-state-manager\n  version: 0.2.3\n  source-url: https://example.invalid/registry.json\n")
	if err := state.configure(jsonBytes(lifecycleRequest{ConfigYAML: cfg, SchemaVersion: 6})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(state.shutdown)
	if state.config.Mode != "force" || state.config.BlockWithoutState || !state.config.Probe.Enabled || !state.config.Probe.Continuous || state.config.Probe.RetrySeconds != 5 {
		t.Fatal("fresh defaults require extra configuration")
	}
	if !state.config.SelectionRequired || state.config.StateFile == "" || state.config.SelectionFile == "" || state.config.RuntimeFile == "" || state.config.ArchiveDir == "" {
		t.Fatal("default storage or UI selection unavailable")
	}
	if begin(t, state, "unchecked", "auth-a", "gpt-6-astra", "").Terminate {
		t.Fatal("unselected request blocked")
	}
	select {
	case <-started:
		t.Fatal("unselected install consumed a probe")
	default:
	}
	saveSelection(t, state, []string{digest("auth-a")[:12]}, []string{"gpt-6-astra"})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("UI selection did not start acquisition")
	}
	state.probeWG.Wait()
	state.stopBackground()
	if len(state.current[stateKey("auth-a", "gpt-6-astra")].Value) != 292 {
		// Wait for completion instead of cancelling an active accepted response.
		t.Fatal("default setup did not retain accepted state")
	}
	saved, err := loadSelection(state.config.SelectionFile)
	if err != nil || len(saved.Models) != 1 || len(saved.Accounts) != 1 {
		t.Fatal("UI selection was not persisted")
	}
	if result := begin(t, state, "with-cache", "auth-a", "gpt-6-astra", ""); result.Terminate || len(headerValue(result.Headers, turnStateHeader)) != 292 {
		t.Fatal("fresh install did not apply cache")
	}
	state.mu.Lock()
	delete(state.current, stateKey("auth-a", "gpt-6-astra"))
	state.mu.Unlock()
	if result := begin(t, state, "without-cache", "auth-a", "gpt-6-astra", "original"); result.Terminate || len(result.Headers) > 0 || len(result.ClearHeaders) > 0 {
		t.Fatal("missing cache did not pass through unchanged")
	}
}

func TestDefaultStorageFollowsCPAPluginDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "mounted plugins")
	config := filepath.Join(root, "host.yaml")
	if err := os.WriteFile(config, []byte("plugins:\n  dir: '"+filepath.ToSlash(dir)+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--config", config}, {"--config=" + config}, {"-config", config}, {"-config=" + config}} {
		if got := defaultStorageDir(args, root); got != filepath.Join(dir, "."+pluginName) {
			t.Fatalf("wrong storage directory: %s", got)
		}
	}
	if got := defaultStorageDir(nil, root); got != filepath.Join(root, "plugins", "."+pluginName) {
		t.Fatal("default plugins directory not used")
	}
}

func TestExplicitConfigurationStillOverridesDefaults(t *testing.T) {
	state := newRuntimeState()
	state.storageDir = t.TempDir()
	state.hostCall = testHost("account-a", "socks5://proxy.invalid:1080")
	if err := state.configure(jsonBytes(lifecycleRequest{ConfigYAML: []byte("mode: observe\nblock_without_state: true\nprobe:\n  enabled: false\n  continuous: false\n  retry_seconds: 20\n"), SchemaVersion: 4})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(state.shutdown)
	if state.config.Mode != "observe" || !state.config.BlockWithoutState || state.config.Probe.Enabled || state.config.Probe.Continuous || state.config.Probe.RetrySeconds != 20 {
		t.Fatal("explicit configuration was overwritten")
	}
	raw, err := state.statusResponse()
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct{ Result struct{ Body []byte } }
	if json.Unmarshal(raw, &envelope) != nil {
		t.Fatal("invalid status")
	}
}
