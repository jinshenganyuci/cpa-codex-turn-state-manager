package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func poolChange(t *testing.T, p *probeProxyPool, action string, edit poolEdit) {
	t.Helper()
	p.mu.Lock()
	edit.Revision = p.data.Revision
	p.mu.Unlock()
	if status, err := p.edit(action, edit); status != 200 {
		t.Fatalf("pool mutation: %d %s", status, err)
	}
}
func addPoolProxy(t *testing.T, p *probeProxyPool, url string, rotating bool) string {
	t.Helper()
	poolChange(t, p, "save", poolEdit{URL: url, Label: "Test proxy", Enabled: true, Rotating: rotating})
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.data.Entries[len(p.data.Entries)-1].ID
}
func TestProxyPoolCRUDPersistenceAndSecretProjection(t *testing.T) {
	p := newProbeProxyPool()
	path := filepath.Join(t.TempDir(), "proxies.json")
	if err := p.load(path); err != nil {
		t.Fatal(err)
	}
	id := addPoolProxy(t, p, "socks5://private-user:private-password@proxy.invalid:1080", false)
	poolChange(t, p, "mode", poolEdit{Enabled: true})
	raw, _ := json.Marshal(p.view())
	if strings.Contains(string(raw), "private-user") || strings.Contains(string(raw), "private-password") {
		t.Fatal("proxy auth exposed by status")
	}
	reloaded := newProbeProxyPool()
	if err := reloaded.load(path); err != nil {
		t.Fatal(err)
	}
	if !reloaded.data.Enabled || len(reloaded.data.Entries) != 1 || reloaded.data.Entries[0].URL != p.data.Entries[0].URL {
		t.Fatal("proxy config lost on reload")
	}
	if code, _ := p.edit("delete", poolEdit{Revision: 0, ID: id}); code != 409 {
		t.Fatal("stale mutation accepted")
	}
	poolChange(t, p, "save", poolEdit{ID: id, Label: "Renamed", Enabled: false})
	if p.data.Entries[0].URL != reloaded.data.Entries[0].URL {
		t.Fatal("blank edit erased proxy credentials")
	}
	previous := p.data.Revision
	p.path = t.TempDir()
	if code, _ := p.edit("delete", poolEdit{Revision: previous, ID: id}); code != 500 || len(p.data.Entries) != 1 || p.data.Revision != previous {
		t.Fatal("failed persistence changed live configuration")
	}
	p.path = path
	poolChange(t, p, "delete", poolEdit{ID: id})
	if err := reloaded.load(filepath.Join(t.TempDir(), "empty.json")); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.load(path); err != nil || len(reloaded.data.Entries) != 0 {
		t.Fatal("delete not persisted")
	}
}
func TestProxyPoolRotationCooldownAndNoCredentialFallback(t *testing.T) {
	p := newProbeProxyPool()
	if err := p.load(filepath.Join(t.TempDir(), "p.json")); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	credential := "socks5://credential.invalid:1080"
	routes, err := p.routes("auth", credential, 3, now)
	endpoint, ticket := routes[0].endpoint, routes[0].ticket
	if err != "" || ticket.ID != "" || endpoint.URL != credential {
		t.Fatal("disabled pool changed route")
	}
	a := addPoolProxy(t, p, "socks5://a.invalid:1080", false)
	b := addPoolProxy(t, p, "socks5://b.invalid:1080", false)
	poolChange(t, p, "mode", poolEdit{Enabled: true})
	for range 2 {
		seen := make(map[string]bool)
		for range 2 {
			got, reason := p.routes("auth", credential, 1, now)
			if reason != "" || seen[got[0].ticket.ID] {
				t.Fatal("single route did not rotate")
			}
			seen[got[0].ticket.ID] = true
		}
	}
	for _, e := range p.data.Entries {
		for range poolFailureThreshold {
			p.finish(poolTicket{e.ID, e.URL}, "network_error", 0, now)
		}
	}
	if _, err = p.routes("auth", credential, 3, now); err != "proxy_pool_unavailable" {
		t.Fatal("cooling pool fell back to credential")
	}
	if _, err = p.routes("auth", credential, 3, now.Add(poolCooldown)); err != "" {
		t.Fatal("cooldown did not recover")
	}
	poolChange(t, p, "delete", poolEdit{ID: a})
	poolChange(t, p, "delete", poolEdit{ID: b})
	if _, err = p.routes("auth", credential, 3, now); err != "proxy_pool_unavailable" {
		t.Fatal("empty pool fell back to credential")
	}
}
func TestProbePoolUsesPoolOnlyAndRetainsSelectionScope(t *testing.T) {
	state, _ := selectionRuntime(t)
	state.config.Probe.Enabled = true
	id := addPoolProxy(t, state.pool, "socks5://pool.invalid:1080", false)
	poolChange(t, state.pool, "mode", poolEdit{Enabled: true})
	saveSelection(t, state, []string{digest("auth-a")[:12]}, []string{"gpt-6-astra"})
	calls := 0
	state.fetch = func(_ context.Context, auth probeAuth, model string, first *proxyEndpoint, endpoint proxyEndpoint) (string, string, string) {
		calls++
		if first != nil || endpoint.URL != "socks5://pool.invalid:1080" || auth.ProxyURL != "socks5://proxy.invalid:1080" {
			t.Error("business credential proxy mutated or pool ignored")
		}
		return makeFernetToken(t, state.now(), 12), "ok", model
	}
	state.ensureProbe("auth-b", "gpt-6-astra")
	state.ensureProbe("auth-a", "gpt-5.6-sol")
	if calls != 0 {
		t.Fatal("pool bypassed selection")
	}
	state.ensureProbe("auth-a", "gpt-6-astra")
	if calls != 1 || state.history[len(state.history)-1].ProxyID != id {
		t.Fatal("selected acquisition did not use pool")
	}
	state.ensureProbe("auth-a", "gpt-6-astra")
	if calls != 1 {
		t.Fatal("332 cache did not pause pool acquisition")
	}
	if got := begin(t, state, "unchecked", "auth-b", "gpt-6-astra", ""); len(got.Headers) != 0 || got.Terminate {
		t.Fatal("pool affected unchecked business request")
	}
}
func TestProxyPoolRotatingValidationFailuresAndAccountLimits(t *testing.T) {
	p := newProbeProxyPool()
	_ = p.load(filepath.Join(t.TempDir(), "p.json"))
	id := addPoolProxy(t, p, "socks5://rotating.invalid:1080", true)
	poolChange(t, p, "mode", poolEdit{Enabled: true})
	ticket := poolTicket{id, p.data.Entries[0].URL}
	now := time.Now()
	for range 10 {
		p.finish(ticket, "model_mismatch", 356, now)
		p.finish(ticket, "upstream_http_429", 0, now)
	}
	if !p.data.Entries[0].CooldownUntil.IsZero() {
		t.Fatal("rotating validation or account quota cooled the endpoint")
	}
	p.finish(ticket, "ok", 332, now)
	if p.data.Entries[0].Success332 != 1 {
		t.Fatal("success not recorded")
	}
	poolChange(t, p, "delete", poolEdit{ID: id})
	if p.finish(ticket, "ok", 292, now) {
		t.Fatal("deleted proxy result was retained")
	}
}
func TestProxyPoolConnectionTestDoesNotReadCredentialOrProbeModel(t *testing.T) {
	state := testRuntime(t, "")
	id := addPoolProxy(t, state.pool, "socks5://probe.invalid:1080", false)
	state.hostCall = func(string, any, any) error { t.Error("proxy test accessed credentials"); return nil }
	state.fetch = func(context.Context, probeAuth, string, *proxyEndpoint, proxyEndpoint) (string, string, string) {
		t.Error("connection test called a model")
		return "", "", ""
	}
	started, release := make(chan struct{}), make(chan struct{})
	state.pool.check = func(ctx context.Context, p proxyEndpoint) proxyCheck {
		close(started)
		<-release
		return proxyCheck{At: time.Now(), Connected: true, Status: 401, Result: "reachable"}
	}
	raw, err := state.management(managementRequest{Method: http.MethodPost, Path: apiBase + "/proxy-pool/test", Body: jsonBytes(map[string]string{"id": id})})
	if responseCode(t, raw, err) != 202 {
		t.Fatal("test did not start asynchronously")
	}
	<-started
	raw, err = state.management(managementRequest{Method: http.MethodPost, Path: apiBase + "/proxy-pool/test", Body: jsonBytes(map[string]string{"id": id})})
	if responseCode(t, raw, err) != 409 {
		t.Fatal("duplicate test queued")
	}
	close(release)
	state.probeWG.Wait()
	if state.pool.data.Entries[0].Test.Status != 401 || state.pool.data.Entries[0].Attempts != 0 {
		t.Fatal("test result missing or model budget consumed")
	}
	if _, err := os.Stat(state.config.ProxyPoolFile); err != nil {
		t.Fatal(err)
	}
}
func TestStatusHidesUncheckedPairsButKeepsSelectionOptions(t *testing.T) {
	state, _ := selectionRuntime(t)
	state.recordLocked(stateKey("auth-a", "gpt-6-astra"), 332, true, "probe", "medium")
	state.recordLocked(stateKey("auth-b", "gpt-6-astra"), 332, true, "probe", "medium")
	saveSelection(t, state, []string{digest("auth-a")[:12]}, []string{"gpt-6-astra"})
	raw, err := state.statusResponse()
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct{ Result struct{ Body []byte } }
	_ = json.Unmarshal(raw, &envelope)
	var view struct {
		Entries   []struct{ Account, Model string }
		History   []struct{ Account string }
		Selection struct{ Accounts []any }
	}
	if json.Unmarshal(envelope.Result.Body, &view) != nil || len(view.Entries) != 1 || view.Entries[0].Account != digest("auth-a")[:12] || len(view.History) != 1 || len(view.Selection.Accounts) != 2 {
		t.Fatal("unselected rows visible or selection options hidden")
	}
}
