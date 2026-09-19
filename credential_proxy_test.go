package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCredentialProxyOverridesRetiredPoolAndFollowsEdits(t *testing.T) {
	state := testRuntime(t, "probe:\n  enabled: true\n  continuous: true\n  background_refresh: false\n  retry_seconds: 5\n  proxy_mode: pool\n  allow_proxy_override: true\n  failure_threshold: 1\n  cooldown_seconds: 3600\n  credential_pools:\n    auth-a:\n      - url_file: /missing/retired-pool.txt\n  proxy_pool:\n    - url: socks5://retired.invalid:1080\n  first_proxy:\n    url: socks5://retired-first.invalid:1080\n")
	now := state.now()
	state.now = func() time.Time { return now }
	if state.config.Probe.ProxyMode != "credential" || len(state.config.Probe.CredentialPools) != 0 || state.config.Probe.FirstProxy != nil {
		t.Fatal("retired routing was retained")
	}
	var endpoints []string
	state.fetch = func(_ context.Context, auth probeAuth, _ string, first *proxyEndpoint, endpoint proxyEndpoint) (string, string, string) {
		if first != nil || endpoint.URL != auth.ProxyURL {
			t.Error("acquisition escaped credential settings")
		}
		endpoints = append(endpoints, endpoint.URL)
		return "", "network_error", ""
	}
	for i := 0; i < 8; i++ {
		state.ensureProbe("auth-a", "gpt-6-astra")
		if len(endpoints) != i+1 {
			t.Fatal("removed pool cooling delayed normal retry")
		}
		now = now.Add(5 * time.Second)
	}
	state.hostCall = testHost("account-a", "socks5://updated.invalid:1080")
	state.ensureProbe("auth-a", "gpt-6-astra")
	if endpoints[len(endpoints)-1] != "socks5://updated.invalid:1080" {
		t.Fatal("credential proxy update was ignored")
	}
	for _, value := range []string{"", "not-a-proxy"} {
		now = now.Add(5 * time.Second)
		state.hostCall = testHost("account-a", value)
		state.ensureProbe("auth-a", "gpt-6-astra")
		if len(endpoints) != 9 {
			t.Fatal("missing or invalid proxy reached transport")
		}
	}
}

func TestRetiredPoolJournalCannotDisableCredentialProxy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.json")
	key := stateKey("auth-a", "gpt-6-astra")
	now := time.Now().UTC()
	old := map[string]any{"version": 1, "proxies": map[string]any{"old-proxy": map[string]any{"disabled": true, "cooldown_until": now.Add(time.Hour)}}, "probe_results": map[string]string{key: "proxies_cooling_or_disabled"}, "blocked_until": map[string]time.Time{key: now.Add(15 * time.Minute)}}
	if err := writePrivateJSON(path, old); err != nil {
		t.Fatal(err)
	}
	j, err := loadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	state := priorityRuntime(t)
	state.restoreJournalLocked(path, j)
	if state.probeResults[key] != "" {
		t.Fatal("retired cooling result remained active")
	}
	if !state.blockedUntil[key].After(now) {
		t.Fatal("credential quota backoff was lost")
	}
	raw, err := state.management(managementRequest{Method: http.MethodPost, Path: apiBase + "/proxy", Body: []byte(`{"id":"old-proxy","action":"enable"}`)})
	if responseCode(t, raw, err) != 404 {
		t.Fatal("retired proxy control is still available")
	}
	encoded, _ := json.Marshal(managementRoutes())
	if strings.Contains(string(encoded), `/proxy"`) {
		t.Fatal("retired proxy route advertised")
	}
}
