package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"time"
)

// RepositoryURL identifies the dedicated plugin release repository.
var RepositoryURL = "https://github.com/jinshenganyuci/cpa-codex-turn-state-manager"

//go:embed web/*
var assets embed.FS

const apiBase = "/v0/management/codex-turn-state-manager"
const resourceBase = "/v0/resource/plugins/codex-turn-state-manager"

type observation struct {
	Acceptance  string `json:"acceptance,omitempty"`
	CacheAction string `json:"cache_action,omitempty"`
	StateSource string `json:"state_source,omitempty"`
	ProxyID     string `json:"proxy_id,omitempty"`
	credentialDisplay
	RequestedModel      string    `json:"requested_model"`
	ResponseModel       string    `json:"response_model"`
	ResponseModelSource string    `json:"response_model_source,omitempty"`
	HostOutcome         string    `json:"host_outcome,omitempty"`
	TerminalCompleted   bool      `json:"terminal_completed"`
	TerminalFailed      bool      `json:"terminal_failed"`
	At                  time.Time `json:"at"`
	Account             string    `json:"account"`
	Model               string    `json:"model"`
	Length              int       `json:"length"`
	Success             bool      `json:"success"`
	Action              string    `json:"action"`
	Reasoning           string    `json:"reasoning"`
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func identityHash(authID, accountID string) string { return digest(authID + "\x00" + accountID) }
func headerValue(headers http.Header, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}
func requestReasoning(raw []byte) string {
	var body struct {
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Effort string `json:"reasoning_effort"`
	}
	_ = json.Unmarshal(raw, &body)
	if body.Reasoning.Effort != "" {
		return body.Reasoning.Effort
	}
	return body.Effort
}
func (state *runtimeState) recordLocked(key string, length int, success bool, action, reasoning string) {
	state.journalRevision++
	auth, model := splitStateKey(key)
	state.history = append(state.history, observation{At: state.now(), Account: digest(auth)[:12], Model: model, RequestedModel: model, credentialDisplay: credentialDisplay{Label: filepath.Base(auth)}, Length: length, Success: success, Action: action, Reasoning: reasoning})
	if len(state.history) > 200 {
		state.history = append([]observation(nil), state.history[len(state.history)-200:]...)
	}
}

type authListing struct {
	Files []struct {
		ID          string `json:"id"`
		AccountType string `json:"account_type"`
		Email       string `json:"email"`
		PlanType    string `json:"plan_type"`
		Index       string `json:"auth_index"`
		Provider    string `json:"provider"`
		Disabled    bool   `json:"disabled"`
	} `json:"files"`
}

func (state *runtimeState) listAuths() (authListing, error) {
	var listing authListing
	if state.hostCall == nil {
		return listing, errors.New("host unavailable")
	}
	err := state.hostCall("host.auth.list", struct{}{}, &listing)
	return listing, err
}
func (state *runtimeState) selectedIdentity(authID string) (string, error) {
	identity, _, err := state.selectedCredential(authID)
	return identity, err
}

func (state *runtimeState) selectedCredential(authID string) (string, credentialDisplay, error) {
	// CPA derives config API-key IDs from key, endpoint, proxy and headers.
	// These records are absent from host.auth.list; OAuth files never use this ID shape.
	if suffix, ok := strings.CutPrefix(authID, "codex:apikey:"); ok {
		if raw, err := hex.DecodeString(suffix); err == nil && len(raw) == 6 {
			return identityHash(authID, ""), credentialDisplay{Label: filepath.Base(authID)}, nil
		}
	}
	listing, err := state.listAuths()
	if err != nil {
		return "", credentialDisplay{}, err
	}
	for _, entry := range listing.Files {
		if entry.ID != authID || entry.Disabled || entry.Provider != "codex" {
			continue
		}
		if entry.AccountType == "api_key" {
			return identityHash(authID, ""), credentialDisplay{Label: filepath.Base(authID)}, nil
		}
		var result struct {
			JSON credentialMetadata `json:"json"`
		}
		if err := state.hostCall("host.auth.get", map[string]string{"auth_index": entry.Index}, &result); err != nil {
			return "", credentialDisplay{}, err
		}
		if result.JSON.Disabled {
			return "", credentialDisplay{}, errors.New("credential disabled")
		}
		return identityHash(authID, result.JSON.AccountID), credentialPresentation(authID, entry.Email, result.JSON), nil
	}
	return "", credentialDisplay{}, errors.New("credential unavailable")
}

// The token is opaque: structure checks cannot verify its HMAC or routing quality.
// Archives survive expiry but are never loaded automatically for replay.
func archiveState(dir, key string, candidate storedState, reasoning string) error {
	if dir == "" || (len(candidate.Value) != 292 && len(candidate.Value) != 332) {
		return nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("create private archive failed")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || (goruntime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return errors.New("archive directory must have mode 0700")
	}
	auth, model := splitStateKey(key)
	entry := struct {
		State     storedState `json:"state"`
		Length    int         `json:"length"`
		Account   string      `json:"account"`
		Model     string      `json:"model"`
		Reasoning string      `json:"reasoning"`
		Source    string      `json:"source"`
		SHA256    string      `json:"sha256"`
	}{candidate, len(candidate.Value), digest(auth)[:12], model, reasoning, "successful_upstream_response", digest(candidate.Value)}
	raw, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, digest(key+"\x00"+candidate.Value)+".json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("create archive failed")
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return errors.New("write archive failed")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("sync archive failed")
	}
	if err := file.Close(); err != nil {
		return errors.New("close archive failed")
	}
	return nil
}

func updateTerminal(binding *requestBinding, raw []byte) {
	observeModel(binding, raw, false)
	var event struct {
		Type     string `json:"type"`
		Status   string `json:"status"`
		Response struct {
			Status string `json:"status"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	if event.Type == "response.completed" && event.Response.Status == "completed" || event.Type == "" && event.Status == "completed" {
		binding.Completed = true
	}
	if event.Type == "response.failed" || event.Type == "response.incomplete" || event.Type == "error" || event.Status == "failed" || event.Status == "incomplete" {
		binding.Failed = true
	}
}
func (state *runtimeState) observeBody(requestID string, body []byte) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if binding, ok := state.requests[requestID]; ok {
		updateTerminal(&binding, body)
		state.requests[requestID] = binding
	}
}
func (state *runtimeState) observeWebSocket(raw []byte) ([]byte, error) {
	var event struct {
		RequestID string
		AuthID    string
		Model     string
		EventType string
		Payload   []byte
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, errors.New("invalid websocket event")
	}
	state.mu.Lock()
	binding, ok := state.requests[event.RequestID]
	auth, model := splitStateKey(binding.Key)
	state.mu.Unlock()
	if !ok || event.AuthID != "" && event.AuthID != auth || event.Model != "" && event.Model != model {
		return okEnvelope(struct{}{})
	}
	var payload struct {
		Type    string                     `json:"type"`
		Headers map[string]json.RawMessage `json:"headers"`
	}
	if json.Unmarshal(event.Payload, &payload) == nil && payload.Type == "response.metadata" {
		for name, value := range payload.Headers {
			if !strings.EqualFold(name, turnStateHeader) {
				continue
			}
			var token string
			if json.Unmarshal(value, &token) != nil {
				var values []string
				if json.Unmarshal(value, &values) == nil && len(values) > 0 {
					token = values[0]
				}
			}
			state.captureCandidate(event.RequestID, http.Header{turnStateHeader: {token}})
		}
	}
	state.mu.Lock()
	if current, exists := state.requests[event.RequestID]; exists {
		observeModel(&current, event.Payload, true)
		state.requests[event.RequestID] = current
	}
	state.mu.Unlock()
	state.observeBody(event.RequestID, event.Payload)
	return okEnvelope(struct{}{})
}

type managementRequest struct {
	Method string
	Path   string
	Body   []byte
}

func rawRequest(raw []byte) managementRequest {
	var req managementRequest
	_ = json.Unmarshal(raw, &req)
	return req
}
func managementRoutes() any {
	routes := []map[string]string{{"Method": "GET", "Path": apiBase + "/status"}}
	for _, path := range []string{"probe", "cancel", "mode", "selection"} {
		routes = append(routes, map[string]string{"Method": "POST", "Path": apiBase + "/" + path})
	}
	return map[string]any{"routes": routes, "resources": []map[string]string{{"Path": "/status", "Menu": "回合状态", "Description": "观察、探测并保存 Codex 回合状态。"}, {"Path": "/app.js"}, {"Path": "/app.css"}}}
}
func managementJSON(status int, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return okEnvelope(struct {
		StatusCode int
		Headers    http.Header
		Body       []byte
	}{status, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}}, raw})
}
func (state *runtimeState) management(req managementRequest) ([]byte, error) {
	if req.Method == "GET" && strings.HasPrefix(req.Path, resourceBase+"/") {
		name, mime := "", ""
		switch req.Path {
		case resourceBase + "/status":
			name, mime = "index.html", "text/html; charset=utf-8"
		case resourceBase + "/app.js":
			name, mime = "app.js", "text/javascript"
		case resourceBase + "/app.css":
			name, mime = "app.css", "text/css"
		}
		if name == "" {
			return managementJSON(404, map[string]string{"error": "not_found"})
		}
		body, err := assets.ReadFile("web/" + name)
		if err != nil {
			return nil, err
		}
		return okEnvelope(struct {
			StatusCode int
			Headers    http.Header
			Body       []byte
		}{200, http.Header{"Content-Type": {mime}, "Cache-Control": {"no-cache"}, "X-Content-Type-Options": {"nosniff"}, "Referrer-Policy": {"no-referrer"}, "Content-Security-Policy": {"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'self'"}}, body})
	}
	if req.Method == "GET" && req.Path == apiBase+"/status" {
		return state.statusResponse()
	}
	if req.Method != "POST" {
		return managementJSON(404, map[string]string{"error": "not_found"})
	}
	if req.Path == apiBase+"/selection" {
		return state.updateSelection(req.Body)
	}
	var body struct {
		Account string `json:"account"`
		Model   string `json:"model"`
		Mode    string `json:"mode"`
		DryRun  bool   `json:"dry_run"`
	}
	if len(req.Body) > 4096 || json.Unmarshal(req.Body, &body) != nil {
		return managementJSON(400, map[string]string{"error": "invalid_body"})
	}
	switch req.Path {
	case apiBase + "/mode":
		if body.Mode != "observe" && body.Mode != "replace_only" && body.Mode != "force" {
			return managementJSON(400, map[string]string{"error": "invalid_mode"})
		}
		state.mu.Lock()
		state.config.Mode = body.Mode
		state.config.DryRun = body.DryRun
		state.mu.Unlock()
		return managementJSON(200, map[string]any{"ok": true, "persistence": "until_reconfigure"})
	case apiBase + "/cancel":
		state.mu.Lock()
		for _, cancel := range state.activeCancels {
			cancel()
		}
		state.refreshRequests = make(map[string]string)
		state.mu.Unlock()
		return managementJSON(200, map[string]bool{"ok": true})
	case apiBase + "/probe":
		listing, err := state.listAuths()
		if err != nil {
			return managementJSON(503, map[string]string{"error": "auth_unavailable"})
		}
		for _, auth := range listing.Files {
			if auth.Disabled || auth.Provider != "codex" || digest(auth.ID)[:12] != body.Account {
				continue
			}
			state.mu.Lock()
			policy, ok := credentialFor(state.config, auth.ID)
			if !state.accepting || !state.config.Probe.Enabled || !ok || body.Model == "" || strings.Contains(body.Model, "*") || !matchesModels(policy.Models, body.Model) || !state.pairSelectedLocked(auth.ID, body.Model) {
				state.mu.Unlock()
				return managementJSON(400, map[string]string{"error": "probe_not_enabled_or_model_out_of_scope"})
			}
			state.mu.Unlock()
			reason := state.startProbe(auth.ID, body.Model, true)
			if reason != "" {
				code := 400
				if reason == "probe_already_running" {
					code = 409
				}
				if reason == "probe_retry_later" {
					code = 429
				}
				return managementJSON(code, map[string]string{"error": reason})
			}
			return managementJSON(202, map[string]bool{"started": true})
		}
		return managementJSON(404, map[string]string{"error": "account_not_found"})
	}
	return managementJSON(404, map[string]string{"error": "not_found"})
}
func (state *runtimeState) statusResponse() ([]byte, error) {
	defer state.flushPersistence()
	listing, _ := state.listDisplayAuths()
	state.mu.Lock()
	defer state.mu.Unlock()
	keys := make(map[string]bool)
	for key := range state.current {
		keys[key] = true
	}
	for key := range state.standby {
		keys[key] = true
	}
	for key := range state.probeResults {
		keys[key] = true
	}
	for key := range state.refreshRequests {
		keys[key] = true
	}
	for key := range state.observed {
		keys[key] = true
	}
	for _, auth := range listing.Files {
		if auth.Disabled || auth.Provider != "codex" {
			continue
		}
		if _, ok := credentialFor(state.config, auth.ID); ok {
			for _, model := range modelOptions(state.config) {
				if !strings.Contains(model, "*") {
					keys[stateKey(auth.ID, model)] = true
				}
			}
		}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	entries := make([]map[string]any, 0, len(keys))
	validCount := 0
	for _, key := range ordered {
		auth, model := splitStateKey(key)
		state.promoteStandbyLocked(key)
		current := state.current[key]
		standby := state.standby[key]
		display := credentialDisplay{Label: filepath.Base(auth)}
		for _, entry := range listing.Files {
			if entry.ID == auth {
				display.Email, display.PlanType = entry.Email, entry.PlanType
				break
			}
		}
		selected := state.pairSelectedLocked(auth, model)
		valid := state.usableSlotLocked(key, current)
		if valid {
			validCount++
		}
		var nextRefresh *time.Time
		if selected && state.config.Probe.Enabled && (state.config.Probe.Continuous || current.Value != "" && enabledByDefault(state.config.Probe.BackgroundRefresh) || state.refreshRequests[key] != "") {
			due := state.nextRefreshLocked(key)
			nextRefresh = &due
		}
		entries = append(entries, map[string]any{"account": digest(auth)[:12], "model": model, "selected": selected, "email": display.Email, "plan_type": display.PlanType, "credential_label": display.Label, "state_length": len(current.Value), "source": current.Source, "response_model": current.ResponseModel, "standby": map[string]any{"length": len(standby.Value), "valid": state.usableSlotLocked(key, standby), "source": standby.Source, "issued_at": standby.IssuedAt, "expires_at": standby.IssuedAt.Add(turnStateTTL), "response_model": standby.ResponseModel}, "last_observed_length": state.observed[key], "valid": valid, "issued_at": current.IssuedAt, "expires_at": current.IssuedAt.Add(turnStateTTL), "next_refresh_at": nextRefresh, "last_probe": state.probeResults[key], "last_probe_at": state.lastProbe[key], "pending": state.refreshRequests[key], "probing": state.probing[key], "probes_this_hour": state.probeCounts[auth], "blocked_until": state.blockedUntil[key]})
	}
	limit := state.config.Probe.MaxPerHour
	if state.config.Probe.Continuous {
		limit = 0
	}
	return managementJSON(200, map[string]any{"selection": state.selectionViewLocked(listing), "version": pluginVersion, "prefer_292": enabledByDefault(state.config.Prefer292), "require_model_match": enabledByDefault(state.config.RequireModelMatch), "standby_enabled": enabledByDefault(state.config.StandbyEnabled), "history_persistent": state.config.RuntimeFile != "", "persistence_error": state.persistenceError, "mode": state.config.Mode, "dry_run": state.config.DryRun, "block_without_state": state.config.BlockWithoutState, "blocking_active": state.blockWithoutStateLocked(), "probe_parallel": true, "probe_enabled": state.config.Probe.Enabled, "continuous": state.config.Probe.Enabled && state.config.Probe.Continuous, "retry_seconds": state.config.Probe.RetrySeconds, "background_refresh": state.config.Probe.Enabled && (state.config.Probe.Continuous || enabledByDefault(state.config.Probe.BackgroundRefresh)), "refresh_before_seconds": state.config.Probe.RefreshBeforeSeconds, "proxy_mode": state.config.Probe.ProxyMode, "reasoning_effort": state.config.Probe.ReasoningEffort, "max_per_hour": limit, "valid_count": validCount, "entries": entries, "history": state.history})
}
