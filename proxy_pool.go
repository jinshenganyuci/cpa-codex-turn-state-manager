package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const poolFailureThreshold = 3
const poolCooldown = time.Minute

type proxyCheck struct {
	At           time.Time `json:"at"`
	Connected    bool      `json:"connected"`
	Status       int       `json:"http_status"`
	Milliseconds int64     `json:"latency_ms"`
	Result       string    `json:"result"`
	ExitIP       string    `json:"exit_ip,omitempty"`
}
type poolEntry struct {
	ID            string     `json:"id"`
	Label         string     `json:"label"`
	URL           string     `json:"url"`
	Enabled       bool       `json:"enabled"`
	Rotating      bool       `json:"rotating"`
	Attempts      int        `json:"attempts"`
	Success292    int        `json:"success_292"`
	Success332    int        `json:"success_332"`
	Failures      int        `json:"failures"`
	CooldownUntil time.Time  `json:"cooldown_until"`
	LastResult    string     `json:"last_result"`
	LastUsed      time.Time  `json:"last_used"`
	Test          proxyCheck `json:"test"`
}
type poolFile struct {
	Version  int         `json:"version"`
	Revision uint64      `json:"revision"`
	Enabled  bool        `json:"enabled"`
	Entries  []poolEntry `json:"entries"`
}
type probeProxyPool struct {
	mu               sync.Mutex
	path             string
	data             poolFile
	selections       map[string]*proxySelection
	testing          map[string]bool
	persistenceError string
	check            func(context.Context, proxyEndpoint) proxyCheck
}
type poolTicket struct{ ID, URL string }

type proxySelection struct {
	uses     map[string]uint64
	previous map[string]bool
}

func newProbeProxyPool() *probeProxyPool {
	return &probeProxyPool{data: poolFile{Version: 1, Entries: []poolEntry{}}, testing: make(map[string]bool), selections: make(map[string]*proxySelection), check: checkProxyConnection}
}
func validPoolURL(raw string) bool {
	if len(raw) == 0 || len(raw) > 8192 || strings.ContainsAny(raw, "\r\n\t") {
		return false
	}
	_, err := (proxyEndpoint{URL: raw}).resolve()
	return err == nil
}
func (p *probeProxyPool) load(path string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.path == path {
		return nil
	}
	data := poolFile{Version: 1, Entries: []poolEntry{}}
	if err := readPrivateJSON(path, 2<<20, &data); err != nil {
		return errors.New("proxy_pool_file_invalid")
	}
	if data.Version != 1 || len(data.Entries) > 128 {
		return errors.New("proxy_pool_file_invalid")
	}
	seen := make(map[string]bool)
	for _, e := range data.Entries {
		if e.ID == "" || seen[e.ID] || !validPoolURL(e.URL) || len(e.Label) > 120 {
			return errors.New("proxy_pool_file_invalid")
		}
		seen[e.ID] = true
	}
	p.path, p.data = path, data
	p.selections = make(map[string]*proxySelection)
	return nil
}
func (p *probeProxyPool) view() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries := make([]map[string]any, 0, len(p.data.Entries))
	for _, e := range p.data.Entries {
		u, _ := url.Parse(e.URL)
		authenticated := u.User != nil
		u.User = nil
		entries = append(entries, map[string]any{"id": e.ID, "label": e.Label, "address": u.String(), "authenticated": authenticated, "enabled": e.Enabled, "rotating": e.Rotating, "attempts": e.Attempts, "success_292": e.Success292, "success_332": e.Success332, "failures": e.Failures, "cooldown_until": e.CooldownUntil, "last_result": e.LastResult, "last_used": e.LastUsed, "test": e.Test, "testing": p.testing[e.ID]})
	}
	return map[string]any{"enabled": p.data.Enabled, "revision": p.data.Revision, "entries": entries, "persistent": p.path != "", "persistence_error": p.persistenceError, "failure_threshold": poolFailureThreshold, "cooldown_seconds": int(poolCooldown / time.Second)}
}

type poolEdit struct {
	Revision uint64 `json:"revision"`
	ID       string `json:"id"`
	Label    string `json:"label"`
	URL      string `json:"url"`
	Enabled  bool   `json:"enabled"`
	Rotating bool   `json:"rotating"`
}

func (p *probeProxyPool) edit(action string, b poolEdit) (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if b.Revision != p.data.Revision {
		return 409, "proxy_pool_changed_reload"
	}
	if p.path == "" {
		return 400, "proxy_pool_persistence_required"
	}
	next := p.data
	next.Entries = append([]poolEntry{}, p.data.Entries...)
	index := -1
	for i, e := range next.Entries {
		if e.ID == b.ID {
			index = i
			break
		}
	}
	switch action {
	case "mode":
		next.Enabled = b.Enabled
	case "save":
		b.URL = strings.TrimSpace(b.URL)
		b.Label = strings.TrimSpace(b.Label)
		if len(b.Label) > 120 || strings.ContainsAny(b.Label, "\r\n") {
			return 400, "invalid_proxy_label"
		}
		if b.ID != "" && index < 0 {
			return 404, "proxy_not_found"
		}
		if b.ID == "" {
			if len(next.Entries) >= 128 {
				return 400, "proxy_pool_full"
			}
			var id [12]byte
			if _, err := rand.Read(id[:]); err != nil {
				return 500, "proxy_id_failed"
			}
			next.Entries = append(next.Entries, poolEntry{ID: hex.EncodeToString(id[:])})
			index = len(next.Entries) - 1
		}
		e := &next.Entries[index]
		if b.URL != "" {
			e.URL = b.URL
		}
		if !validPoolURL(e.URL) {
			return 400, "invalid_proxy_url"
		}
		for i, other := range next.Entries {
			if i != index && other.URL == e.URL {
				return 409, "proxy_already_exists"
			}
		}
		if index < len(p.data.Entries) && p.data.Entries[index].URL != e.URL {
			e.Failures = 0
			e.CooldownUntil = time.Time{}
			e.Test = proxyCheck{}
			e.LastResult = ""
		}
		e.Label, e.Enabled, e.Rotating = b.Label, b.Enabled, b.Rotating
		if e.Label == "" {
			e.Label = "代理"
		}
		if e.Rotating {
			e.CooldownUntil = time.Time{}
		}
	case "delete":
		if index < 0 {
			return 404, "proxy_not_found"
		}
		next.Entries = append(next.Entries[:index], next.Entries[index+1:]...)
	case "reset":
		if index < 0 {
			return 404, "proxy_not_found"
		}
		next.Entries[index].Failures = 0
		next.Entries[index].CooldownUntil = time.Time{}
	default:
		return 404, "not_found"
	}
	next.Revision++
	if err := writePrivateJSON(p.path, next); err != nil {
		p.persistenceError = "proxy_pool_save_failed"
		return 500, p.persistenceError
	}
	p.data = next
	p.persistenceError = ""
	return 200, ""
}

// Fairness is per credential, independent of other accounts' acquisition traffic.
// Previously unused entries win, then least-used entries with random tie breaking.
func (p *probeProxyPool) routes(auth, credentialURL string, limit int, now time.Time) ([]probeRoute, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.data.Enabled {
		endpoint := proxyEndpoint{URL: strings.TrimSpace(credentialURL)}
		if endpoint.URL == "" {
			return nil, "credential_proxy_required"
		}
		if _, err := endpoint.resolve(); err != nil {
			return nil, "proxy_invalid"
		}
		return []probeRoute{{endpoint: endpoint}}, ""
	}
	indices := make([]int, 0, len(p.data.Entries))
	for i, e := range p.data.Entries {
		if e.Enabled && !now.Before(e.CooldownUntil) {
			indices = append(indices, i)
		}
	}
	if len(indices) == 0 || limit < 1 {
		return nil, "proxy_pool_unavailable"
	}
	selection := p.selections[auth]
	if selection == nil {
		if len(p.selections) >= 4096 {
			p.selections = make(map[string]*proxySelection)
		}
		selection = &proxySelection{uses: make(map[string]uint64), previous: make(map[string]bool)}
		p.selections[auth] = selection
	}
	existing := make(map[string]bool, len(p.data.Entries))
	for _, entry := range p.data.Entries {
		existing[entry.ID] = true
	}
	for id := range selection.uses {
		if !existing[id] {
			delete(selection.uses, id)
			delete(selection.previous, id)
		}
	}
	mathrand.Shuffle(len(indices), func(i, j int) { indices[i], indices[j] = indices[j], indices[i] })
	sort.SliceStable(indices, func(i, j int) bool {
		a, b := p.data.Entries[indices[i]].ID, p.data.Entries[indices[j]].ID
		if selection.previous[a] != selection.previous[b] {
			return !selection.previous[a]
		}
		return selection.uses[a] < selection.uses[b]
	})
	selection.previous = make(map[string]bool)
	if limit > len(indices) {
		limit = len(indices)
	}
	routes := make([]probeRoute, 0, limit)
	for _, i := range indices[:limit] {
		e := &p.data.Entries[i]
		selection.uses[e.ID]++
		selection.previous[e.ID] = true
		e.Attempts++
		e.LastUsed = now
		routes = append(routes, probeRoute{endpoint: proxyEndpoint{URL: e.URL, Label: e.Label}, ticket: poolTicket{e.ID, e.URL}})
	}
	return routes, ""
}

func (p *probeProxyPool) batchCapacity(limit int, now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.data.Enabled {
		return 1
	}
	available := 0
	for _, e := range p.data.Entries {
		if e.Enabled && !now.Before(e.CooldownUntil) {
			available++
		}
	}
	if available == 0 {
		return 1
	}
	if available < limit {
		return available
	}
	return limit
}

func credentialProbeFailure(result string) bool {
	for _, s := range []string{"401", "403", "429", "usage_limit", "quota", "invalid_api_key"} {
		if strings.Contains(result, s) {
			return true
		}
	}
	return false
}
func (p *probeProxyPool) finish(ticket poolTicket, result string, length int, now time.Time) bool {
	if ticket.ID == "" {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.data.Entries {
		e := &p.data.Entries[i]
		if e.ID != ticket.ID || e.URL != ticket.URL {
			continue
		}
		if !p.data.Enabled || !e.Enabled {
			return false
		}
		e.LastResult = result
		switch {
		case result == "ok":
			e.Failures = 0
			e.CooldownUntil = time.Time{}
			if length == 292 {
				e.Success292++
			}
			if length == 332 {
				e.Success332++
			}
		case credentialProbeFailure(result) || result == "cancelled":
		case e.Rotating && (result == "state_rejected" || result == "model_mismatch" || result == "model_evidence_missing"):
		default:
			e.Failures++
			if e.Failures >= poolFailureThreshold {
				e.CooldownUntil = now.Add(poolCooldown)
				e.Failures = 0
			}
		}
		p.persistLocked()
		return true
	}
	return false
}
func (p *probeProxyPool) persistLocked() {
	if p.path != "" && writePrivateJSON(p.path, p.data) != nil {
		p.persistenceError = "proxy_pool_save_failed"
	} else {
		p.persistenceError = ""
	}
}

// Test the HTTPS destination without OAuth or a model invocation.
func checkProxyConnection(ctx context.Context, endpoint proxyEndpoint) proxyCheck {
	start := time.Now()
	result := proxyCheck{At: start, Result: "connection_failed"}
	client := newProbeHTTPClient(ctx, nil, endpoint)
	defer client.transport.CloseIdleConnections()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/codex/responses", nil)
	if err == nil {
		response, errCall := client.do(request)
		if errCall == nil {
			result.Connected = true
			result.Status = response.StatusCode
			result.Result = "reachable"
			if response.StatusCode == 403 {
				result.Result = "upstream_forbidden"
			}
			if response.StatusCode == 407 {
				result.Result = "proxy_auth_failed"
			}
			if response.StatusCode >= 500 {
				result.Result = "upstream_unavailable"
			}
			result.ExitIP = client.finish(ctx, response)
		}
	}
	if ctx.Err() != nil {
		result.Result = "test_timeout_or_cancelled"
	}
	result.Milliseconds = time.Since(start).Milliseconds()
	return result
}
func (state *runtimeState) poolManagement(action string, raw []byte) ([]byte, error) {
	var body poolEdit
	if len(raw) > 16<<10 || json.Unmarshal(raw, &body) != nil {
		return managementJSON(400, map[string]string{"error": "invalid_body"})
	}
	if action == "test" {
		return state.startPoolTest(body.ID)
	}
	code, errCode := state.pool.edit(action, body)
	if code != 200 {
		return managementJSON(code, map[string]string{"error": errCode})
	}
	state.mu.Lock()
	select {
	case state.wake <- struct{}{}:
	default:
	}
	state.mu.Unlock()
	return managementJSON(200, map[string]any{"ok": true, "proxy_pool": state.pool.view()})
}
func (state *runtimeState) startPoolTest(id string) ([]byte, error) {
	state.mu.Lock()
	if !state.accepting || state.probeCtx == nil || state.probeCtx.Err() != nil {
		state.mu.Unlock()
		return managementJSON(409, map[string]string{"error": "plugin_not_running"})
	}
	ctx, cancel := context.WithTimeout(state.probeCtx, 15*time.Second)
	state.probeWG.Add(1)
	state.mu.Unlock()
	p := state.pool
	p.mu.Lock()
	var entry poolEntry
	for _, e := range p.data.Entries {
		if e.ID == id {
			entry = e
			break
		}
	}
	errCode := ""
	if entry.ID == "" {
		errCode = "proxy_not_found"
	} else if p.testing[id] {
		errCode = "proxy_test_running"
	} else if len(p.testing) >= 4 {
		errCode = "proxy_test_busy"
	}
	if errCode != "" {
		p.mu.Unlock()
		cancel()
		state.probeWG.Done()
		return managementJSON(409, map[string]string{"error": errCode})
	}
	p.testing[id] = true
	p.mu.Unlock()
	go func() {
		defer state.probeWG.Done()
		defer cancel()
		result := p.check(ctx, proxyEndpoint{URL: entry.URL})
		p.mu.Lock()
		defer p.mu.Unlock()
		delete(p.testing, id)
		for i := range p.data.Entries {
			if p.data.Entries[i].ID == id && p.data.Entries[i].URL == entry.URL {
				p.data.Entries[i].Test = result
				p.persistLocked()
				break
			}
		}
	}()
	return managementJSON(202, map[string]bool{"started": true})
}
