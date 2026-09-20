package main

import (
	"bufio"
	"bytes"
	"context"

	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultProbeRetrySeconds = 15

type probeConfig struct {
	// Decode retired pool options for old configurations; normalization discards them.
	CredentialPools      map[string][]proxyEndpoint `yaml:"credential_pools,omitempty"`
	FailureThreshold     int                        `yaml:"failure_threshold,omitempty"`
	CooldownSeconds      int                        `yaml:"cooldown_seconds,omitempty"`
	SuccessRestSeconds   int                        `yaml:"success_rest_seconds,omitempty"`
	ReasoningEffort      string                     `yaml:"reasoning_effort"`
	ProxyMode            string                     `yaml:"proxy_mode"`
	AllowProxyOverride   bool                       `yaml:"allow_proxy_override,omitempty"`
	OnMissing            bool                       `yaml:"on_missing"`
	MaxPerHour           int                        `yaml:"max_per_hour"`
	Enabled              bool                       `yaml:"enabled"`
	Continuous           bool                       `yaml:"continuous"`
	FirstProxy           *proxyEndpoint             `yaml:"first_proxy,omitempty"`
	ProxyPool            []proxyEndpoint            `yaml:"proxy_pool,omitempty"`
	RetrySeconds         int                        `yaml:"retry_seconds"`
	RefreshBeforeSeconds int                        `yaml:"refresh_before_seconds"`
	MaxAttempts          int                        `yaml:"max_attempts"`
	BackgroundRefresh    *bool                      `yaml:"background_refresh"`
	RefreshOnErrors      *bool                      `yaml:"refresh_on_errors"`
	QuotaBackoffSeconds  int                        `yaml:"quota_backoff_seconds"`
}

func normalizeProbe(cfg *probeConfig) error {
	// Retired YAML pools stay ignored. The dashboard owns the new private pool file.
	cfg.ProxyMode = "credential"
	cfg.AllowProxyOverride = false
	cfg.FirstProxy, cfg.ProxyPool, cfg.CredentialPools = nil, nil, nil
	cfg.FailureThreshold, cfg.CooldownSeconds, cfg.SuccessRestSeconds = 0, 0, 0
	if cfg.ReasoningEffort == "" {
		cfg.ReasoningEffort = "medium"
	}
	if cfg.ReasoningEffort != "medium" {
		return errors.New("this release supports medium probe reasoning only")
	}
	if cfg.MaxPerHour == 0 {
		cfg.MaxPerHour = 6
	}
	if cfg.MaxPerHour < 1 || cfg.MaxPerHour > 60 {
		return errors.New("max_per_hour must be between 1 and 60")
	}
	if cfg.RetrySeconds == 0 {
		cfg.RetrySeconds = defaultProbeRetrySeconds
	}
	if cfg.RefreshBeforeSeconds == 0 {
		cfg.RefreshBeforeSeconds = 300
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 1
	}
	if cfg.QuotaBackoffSeconds == 0 {
		cfg.QuotaBackoffSeconds = 900
	}
	if cfg.QuotaBackoffSeconds < 60 || cfg.QuotaBackoffSeconds > 86400 {
		return errors.New("quota_backoff_seconds must be between 60 and 86400")
	}
	if cfg.RetrySeconds < 1 || cfg.RefreshBeforeSeconds < 0 || cfg.RefreshBeforeSeconds >= 3600 || cfg.MaxAttempts < 1 || cfg.MaxAttempts > 5 {
		return errors.New("invalid probe limits")
	}
	return nil
}

type probeAuth struct {
	Display     credentialDisplay `json:"-"`
	ProxyURL    string            `json:"proxy_url"`
	AccessToken string            `json:"access_token"`
	AccountID   string            `json:"account_id"`
	Type        string            `json:"type"`
	BaseURL     string            `json:"base_url"`
	Disabled    bool              `json:"disabled"`
}

func (state *runtimeState) selectedProbeAuth(authID string) (probeAuth, error) {
	if state.hostCall == nil {
		return probeAuth{}, errors.New("host auth callbacks unavailable")
	}
	var listing struct {
		Files []struct {
			ID       string `json:"id"`
			Index    string `json:"auth_index"`
			Provider string `json:"provider"`
			Disabled bool   `json:"disabled"`
			BaseURL  string `json:"base_url"`
		} `json:"files"`
	}
	if err := state.hostCall("host.auth.list", struct{}{}, &listing); err != nil {
		return probeAuth{}, err
	}
	for _, entry := range listing.Files {
		if entry.ID != authID {
			continue
		}
		if entry.Disabled || entry.Provider != "codex" || entry.BaseURL != "" {
			return probeAuth{}, errors.New("auth not eligible for Codex probe")
		}
		var result struct {
			JSON json.RawMessage `json:"json"`
		}
		if err := state.hostCall("host.auth.get", map[string]string{"auth_index": entry.Index}, &result); err != nil {
			return probeAuth{}, err
		}
		var auth probeAuth
		var metadata credentialMetadata
		if json.Unmarshal(result.JSON, &auth) != nil {
			return probeAuth{}, errors.New("invalid credential JSON")
		}
		_ = json.Unmarshal(result.JSON, &metadata)
		auth.Display = credentialPresentation(authID, "", metadata)
		// Never forward arbitrary client Authorization or third-party API keys.
		// Tokens are obtained only from the host-selected Codex OAuth credential.
		if auth.Type != "codex" || auth.Disabled || auth.BaseURL != "" || auth.AccessToken == "" {
			return probeAuth{}, errors.New("Codex OAuth credential unavailable")
		}
		return auth, nil
	}
	return probeAuth{}, errors.New("selected auth not found")
}

type probeTask struct {
	authID, model, key string
	config             pluginConfig
	policy             credentialConfig
	ctx                context.Context
	cancel             context.CancelFunc
	generation         uint64
}

// Reserve synchronously so concurrent scheduler passes cannot queue the same pair.
func (state *runtimeState) prepareProbe(authID, model string, manual bool) (*probeTask, string) {
	key := stateKey(authID, model)
	state.mu.Lock()
	defer state.mu.Unlock()
	cfg := state.config
	policy, ok := credentialFor(cfg, authID)
	now := state.now()
	if !state.accepting || !cfg.Probe.Enabled || state.probeCtx == nil || state.probeCtx.Err() != nil || !ok || !autoUpdateEnabled(cfg, policy) || !matchesModels(policy.Models, model) || !state.pairSelectedLocked(authID, model) {
		return nil, "probe_not_enabled_or_model_out_of_scope"
	}
	if state.probing[key] {
		return nil, "probe_already_running"
	}
	due := state.nextRefreshLocked(key)
	if manual {
		due = state.blockedUntil[key]
		if last, ok := state.lastProbe[key]; ok {
			retry := last.Add(time.Duration(cfg.Probe.RetrySeconds) * time.Second)
			if retry.After(due) {
				due = retry
			}
		}
	}
	if now.Before(due) {
		return nil, "probe_retry_later"
	}
	if state.budgetStart.IsZero() || now.Sub(state.budgetStart) >= time.Hour {
		state.budgetStart = now
		state.probeCounts = make(map[string]int)
	}
	if !cfg.Probe.Continuous && state.probeCounts[authID] >= cfg.Probe.MaxPerHour {
		state.probeResults[key] = "hourly_budget_reached"
		state.blockedUntil[key] = state.budgetStart.Add(time.Hour)
		state.journalRevision++
		return nil, "probe_retry_later"
	}
	state.lastProbe[key] = now
	reason := state.refreshRequests[key]
	if manual {
		reason = "manual"
	}
	if reason == "" {
		if state.current[key].Value != "" {
			reason = "before_expiry"
		} else {
			reason = "missing_state"
		}
	}
	state.probeReasons[key] = reason
	state.probing[key] = true
	ctx, cancel := context.WithCancel(state.probeCtx)
	state.activeCancels[key] = cancel
	state.probeWG.Add(1)
	return &probeTask{authID: authID, model: model, key: key, config: cfg, policy: policy, ctx: ctx, cancel: cancel, generation: state.generation}, ""
}

func (state *runtimeState) startProbe(authID, model string, manual bool) string {
	task, reason := state.prepareProbe(authID, model, manual)
	if task == nil {
		return reason
	}
	go state.runProbe(task)
	return ""
}

func (state *runtimeState) ensureProbe(authID, model string) {
	task, _ := state.prepareProbe(authID, model, false)
	if task == nil {
		state.flushPersistence()
		return
	}
	state.runProbe(task)
}

func (state *runtimeState) runProbe(task *probeTask) {
	defer state.probeWG.Done()
	defer state.flushPersistence()
	defer task.cancel()
	authID, model, key := task.authID, task.model, task.key
	cfg, policy, ctx, generation := task.config, task.policy, task.ctx, task.generation
	auth, err := state.selectedProbeAuth(authID)
	if err == nil {
		state.mu.Lock()
		selected := state.identitySelectedLocked(authID, model, identityHash(authID, auth.AccountID))
		state.mu.Unlock()
		if !selected {
			err = errors.New("credential selection no longer matches")
		}
	}
	var candidate storedState
	outcome := "auth_unavailable"
	lastHistoryAt := time.Time{}
	if err == nil {
		for attempt := 0; attempt < cfg.Probe.MaxAttempts && ctx.Err() == nil; attempt++ {
			state.mu.Lock()
			if !cfg.Probe.Continuous && state.probeCounts[authID] >= cfg.Probe.MaxPerHour {
				state.mu.Unlock()
				outcome = "hourly_budget_reached"
				break
			}
			state.mu.Unlock()
			endpoint, ticket, routeError := state.pool.route(auth.ProxyURL, state.now())
			if routeError != "" {
				outcome = routeError
				break
			}
			state.mu.Lock()
			state.probeCounts[authID]++
			state.mu.Unlock()
			observedEgress := &probeEgress{}
			fetchCtx := context.WithValue(ctx, probeEgressKey{}, observedEgress)
			value, status, responseModel := state.fetch(fetchCtx, auth, model, nil, endpoint)
			outcome = status
			parsed, parseErr := parseTurnState(value, cfg.MaxStateBytes)
			if status == "ok" {
				if enabledByDefault(cfg.RequireModelMatch) && !modelConsistent(model, responseModel) {
					if responseModel == "" {
						outcome = "model_evidence_missing"
					} else {
						outcome = "model_mismatch"
					}
				} else if parseErr != nil || !normalBlockCount(policy, parsed.Blocks) || parsed.IssuedAt.After(state.now().Add(5*time.Minute)) || !state.now().Before(parsed.IssuedAt.Add(turnStateTTL)) {
					outcome = "state_rejected"
				}
			}
			if ctx.Err() != nil {
				outcome = "cancelled"
			}
			if !state.pool.finish(ticket, outcome, len(value), state.now()) {
				outcome = "proxy_changed"
			}
			state.mu.Lock()
			if generation != state.generation {
				state.mu.Unlock()
				return
			}
			state.observed[key] = len(value)
			state.recordLocked(key, len(value), status == "ok", "probe", cfg.Probe.ReasoningEffort)
			row := &state.history[len(state.history)-1]
			lastHistoryAt = row.At
			row.credentialDisplay = auth.Display
			row.ResponseModel = responseModel
			row.StateSource = "probe"
			row.ProxyID = ticket.ID
			row.ProxyLabel = endpoint.Label
			row.ExitIP = observedEgress.IP
			row.Acceptance = outcome
			if responseModel != "" {
				row.ResponseModelSource = "upstream.probe.response.model"
			}
			state.mu.Unlock()
			if outcome == "ok" {
				candidate = storedState{Value: value, IssuedAt: parsed.IssuedAt, Blocks: parsed.Blocks, Identity: identityHash(authID, auth.AccountID), ResponseModel: responseModel, Source: "probe", CapturedAt: state.now()}
				break
			}
			if strings.Contains(outcome, "429") || strings.Contains(outcome, "401") || strings.Contains(outcome, "403") || strings.Contains(outcome, "usage_limit") || strings.Contains(outcome, "quota") {
				break
			}
		}
	}
	if ctx.Err() != nil {
		outcome = "cancelled"
		candidate = storedState{}
	}
	if candidate.Value != "" {
		identity, err := state.selectedIdentity(authID)
		if err != nil || identity != candidate.Identity {
			outcome = "identity_changed"
			candidate = storedState{}
		}
	}
	state.mu.Lock()
	if generation != state.generation {
		state.mu.Unlock()
		return
	}
	delete(state.probing, key)
	if !state.identitySelectedLocked(authID, model, candidate.Identity) {
		candidate = storedState{}
	}
	delete(state.activeCancels, key)
	delete(state.refreshRequests, key)
	state.probeResults[key] = outcome
	if strings.Contains(outcome, "429") || strings.Contains(outcome, "401") || strings.Contains(outcome, "403") || strings.Contains(outcome, "usage_limit_reached") || strings.Contains(outcome, "insufficient_quota") {
		state.blockedUntil[key] = state.now().Add(time.Duration(cfg.Probe.QuotaBackoffSeconds) * time.Second)
	}
	cacheAction := ""
	if state.accepting && candidate.Value != "" {
		cacheAction = state.acceptStateLocked(key, candidate)
		delete(state.blockedUntil, key)
	}
	for i := len(state.history) - 1; i >= 0; i-- {
		row := &state.history[i]
		if row.Action == "probe" && row.Model == model && row.Account == digest(authID)[:12] && row.At.Equal(lastHistoryAt) {
			row.CacheAction = cacheAction
			row.Acceptance = outcome
			break
		}
	}
	state.journalRevision++
	archiveDir := state.config.ArchiveDir
	state.mu.Unlock()
	if candidate.Value != "" && archiveState(archiveDir, key, candidate, cfg.Probe.ReasoningEffort) != nil {
		state.mu.Lock()
		state.probeResults[key] = "archive_failed"
		state.journalRevision++
		state.mu.Unlock()
	}
}

// Probes use a tiny independent prompt, never the user's business payload.
// Success requires response.completed; an HTTP 200 with a failed SSE is rejected.
func fetchProbe(ctx context.Context, auth probeAuth, model string, first *proxyEndpoint, second proxyEndpoint) (string, string, string) {
	body, _ := json.Marshal(map[string]any{
		"model": model, "reasoning": map[string]string{"effort": "medium"}, "stream": true, "store": false, "instructions": "Reply with OK only.",
		"input": []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": "ping"}}}},
	})
	client := newProbeHTTPClient(ctx, first, second)
	defer client.transport.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(body))
	if err != nil {
		return "", "request_invalid", ""
	}
	req.Header.Set("Authorization", "Bearer "+auth.AccessToken)
	if auth.AccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", auth.AccountID)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("Originator", "codex_cli_rs")
	req.Header.Set("User-Agent", "codex_cli_rs/0.154.0")
	resp, err := client.do(req)
	if err != nil {
		return "", "network_error", ""
	}
	defer func() {
		ip := client.finish(ctx, resp)
		if observed, ok := ctx.Value(probeEgressKey{}).(*probeEgress); ok {
			observed.IP = ip
		}
	}()
	if resp.StatusCode != 200 {
		status := "upstream_http_" + httpStatus(resp.StatusCode)
		var failure struct {
			Error struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"error"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&failure) == nil {
			for _, code := range []string{failure.Error.Code, failure.Error.Type} {
				switch code {
				case "usage_limit_reached", "rate_limit_exceeded", "insufficient_quota", "model_not_found", "invalid_api_key":
					return "", status + "_" + code, ""
				}
			}
		}
		return "", status, ""
	}
	value := strings.TrimSpace(resp.Header.Get(turnStateHeader))
	completed, responseModel := probeResponse(io.LimitReader(resp.Body, 1<<20))
	if !completed {
		return "", "upstream_incomplete", responseModel
	}
	if value == "" {
		return "", "state_missing", responseModel
	}
	return value, "ok", responseModel
}

func httpStatus(status int) string {
	b, _ := json.Marshal(status)
	return string(b)
}

func probeCompleted(reader io.Reader) bool {
	completed, _ := probeResponse(reader)
	return completed
}

func probeResponse(reader io.Reader) (bool, string) {
	var binding requestBinding
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	var data []string
	check := func() (bool, bool) {
		var event struct {
			Type     string `json:"type"`
			Response struct {
				Status string `json:"status"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(strings.Join(data, "\n")), &event) != nil {
			return false, false
		}
		observeModel(&binding, []byte(strings.Join(data, "\n")), true)
		switch event.Type {
		case "response.completed":
			return true, event.Response.Status == "completed"
		case "response.failed", "response.incomplete", "error":
			return true, false
		}
		return false, false
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if done, ok := check(); done {
				return ok, binding.ResponseModel
			}
			data = nil
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if scanner.Err() != nil {
		return false, binding.ResponseModel
	}
	done, ok := check()
	return done && ok, binding.ResponseModel
}
