package main

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

func enabledByDefault(value *bool) bool { return value == nil || *value }

// Caller holds mu. Each configuration generation owns one worker and context.
func (state *runtimeState) startBackgroundLocked() {
	state.workerDone = nil
	state.wake = make(chan struct{}, 1)
	if !state.accepting || !state.config.Probe.Enabled {
		return
	}
	done := make(chan struct{})
	state.workerDone = done
	go state.refreshLoop(state.probeCtx, state.wake, done)
}

func (state *runtimeState) stopBackground() {
	state.mu.Lock()
	if state.probeCancel != nil {
		state.probeCancel()
	}
	done := state.workerDone
	state.mu.Unlock()
	// Never hold mu while joining: a canceled probe still needs to finish its
	// bookkeeping. Quiesce must drain the worker before host callbacks retire.
	if done != nil {
		<-done
	}
	state.probeWG.Wait()
	state.mu.Lock()
	if state.workerDone == done {
		state.workerDone = nil
	}
	state.mu.Unlock()
}

func (state *runtimeState) refreshLoop(ctx context.Context, wake <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		state.refreshDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		}
	}
}

// Deterministic due-work selection is separated from real timers for testing.
func (state *runtimeState) refreshDue(ctx context.Context) {
	defer state.flushPersistence()
	type job struct {
		key string
		due time.Time
	}
	// Discover eligible credentials without holding mu across host callbacks.
	// This also discovers files loaded after the plugin's startup callback.
	state.mu.Lock()
	discover := state.accepting && state.config.Probe.Enabled && state.config.Probe.Continuous
	state.mu.Unlock()
	var listing authListing
	if discover && ctx.Err() == nil {
		listing, _ = state.listAuths()
	}
	state.mu.Lock()
	keys := make(map[string]bool)
	if state.config.Probe.Continuous {
		for _, auth := range listing.Files {
			if auth.Disabled || auth.Provider != "codex" || auth.AccountType == "api_key" {
				continue
			}
			policy, ok := credentialFor(state.config, auth.ID)
			if !ok || !autoUpdateEnabled(state.config, policy) {
				continue
			}
			for _, model := range state.acquisitionModelsLocked(policy) {
				if model != "" && !strings.Contains(model, "*") {
					keys[stateKey(auth.ID, model)] = true
				}
			}
		}
	}
	if enabledByDefault(state.config.Probe.BackgroundRefresh) {
		for key := range state.current {
			keys[key] = true
		}
	}
	for key := range state.refreshRequests {
		keys[key] = true
	}
	jobs := make([]job, 0, len(keys))
	now := state.now()
	for key := range keys {
		auth, model := splitStateKey(key)
		if !state.pairSelectedLocked(auth, model) {
			continue
		}
		due := state.nextRefreshLocked(key)
		if !now.Before(due) {
			jobs = append(jobs, job{key, due})
		}
	}
	state.mu.Unlock()
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].due.Equal(jobs[j].due) {
			return jobs[i].key < jobs[j].key
		}
		return jobs[i].due.Before(jobs[j].due)
	})
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		auth, model := splitStateKey(job.key)
		if auth != "" && model != "" {
			state.startProbe(auth, model, false)
		}
	}
}

func (state *runtimeState) nextRefreshLocked(key string) time.Time {
	// A missing state or an explicit request is due immediately. Reading a new
	// wall clock here would put it after refreshDue's earlier clock snapshot.
	due := state.acquisitionDueLocked(key)
	if last, ok := state.lastProbe[key]; ok {
		retry := last.Add(time.Duration(state.config.Probe.RetrySeconds) * time.Second)
		if retry.After(due) {
			due = retry
		}
	}
	if state.blockedUntil[key].After(due) {
		due = state.blockedUntil[key]
	}
	return due
}

func (state *runtimeState) queueRefreshLocked(key, reason string) {
	if !state.accepting || !state.config.Probe.Enabled || (!enabledByDefault(state.config.Probe.RefreshOnErrors) && reason != "manual" && reason != "missing_state" && reason != "model_mismatch") || key == "" || reason == "" {
		return
	}
	auth, model := splitStateKey(key)
	policy, ok := credentialFor(state.config, auth)
	if !ok || !autoUpdateEnabled(state.config, policy) || !matchesModels(policy.Models, model) || !state.pairSelectedLocked(auth, model) {
		return
	}
	if reason == "rate_limit" {
		state.blockedUntil[key] = state.now().Add(time.Duration(state.config.Probe.QuotaBackoffSeconds) * time.Second)
	}
	// Mark refresh demand once; API requests never wait for this scheduler signal.
	if state.refreshRequests[key] == "" {
		state.refreshRequests[key] = reason
	}
	select {
	case state.wake <- struct{}{}:
	default:
	}
}

func failureRefreshReason(status int, message string) string {
	if status == 429 {
		return "rate_limit"
	}
	if status == 502 || status == 503 || status == 504 {
		return "upstream_unavailable"
	}
	lower := strings.ToLower(message)
	if strings.Contains(lower, "overload") {
		return "overload"
	}
	for _, code := range []string{"rate_limit", "usage_limit_reached", "insufficient_quota", "too many requests"} {
		if strings.Contains(lower, code) {
			return "rate_limit"
		}
	}
	return ""
}

func (state *runtimeState) observeStreamFailure(requestID string, chunk []byte) {
	state.mu.Lock()
	defer state.mu.Unlock()
	binding, ok := state.requests[requestID]
	if !ok || len(chunk) == 0 {
		return
	}
	// CPA's Responses translator delivers complete data lines without SSE delimiters.
	line := bytes.TrimSpace(chunk)
	if bytes.HasPrefix(line, []byte("data:")) {
		line = bytes.TrimSpace(line[5:])
	}
	if bytes.HasPrefix(line, []byte("event:")) {
		var data []string
		for _, part := range strings.Split(string(line), "\n") {
			if strings.HasPrefix(part, "data:") {
				data = append(data, strings.TrimSpace(part[5:]))
			}
		}
		if combined := []byte(strings.Join(data, "\n")); json.Valid(combined) {
			line = combined
		}
	}
	if json.Valid(line) {
		// An upstream event: line can arrive in its own chunk. A complete data
		// event supersedes that framing fragment instead of concatenating JSONs.
		binding.StreamBuffer = nil
		updateTerminal(&binding, line)
		if reason := streamFailureReason(line); reason != "" {
			state.queueRefreshLocked(binding.Key, reason)
			delete(state.candidates, requestID)
		}
		state.requests[requestID] = binding
		return
	}
	// Host normally delivers one SSE event per chunk; retain bounded fragments
	// for split/multiline events. Never classify assistant text as an error.
	if len(binding.StreamBuffer)+len(chunk) > 1<<20 {
		binding.StreamBuffer = nil
		state.requests[requestID] = binding
		return
	}
	buffer := append(binding.StreamBuffer, chunk...)
	buffer = bytes.ReplaceAll(buffer, []byte("\r\n"), []byte("\n"))
	for {
		end := bytes.Index(buffer, []byte("\n\n"))
		if end < 0 {
			break
		}
		frame := buffer[:end]
		buffer = buffer[end+2:]
		var data []string
		for _, line := range strings.Split(string(frame), "\n") {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		event := []byte(strings.Join(data, "\n"))
		updateTerminal(&binding, event)
		if reason := streamFailureReason(event); reason != "" {
			state.queueRefreshLocked(binding.Key, reason)
			delete(state.candidates, requestID)
		}
	}
	// Some hosts send a raw JSON event rather than SSE framing.
	if json.Valid(buffer) {
		updateTerminal(&binding, buffer)
		if reason := streamFailureReason(buffer); reason != "" {
			state.queueRefreshLocked(binding.Key, reason)
			delete(state.candidates, requestID)
		}
		buffer = nil
	}
	binding.StreamBuffer = append([]byte(nil), buffer...)
	state.requests[requestID] = binding
}

func streamFailureReason(raw []byte) string {
	type upstreamError struct {
		Code       string `json:"code"`
		Type       string `json:"type"`
		Message    string `json:"message"`
		StatusCode int    `json:"status_code"`
	}
	var event struct {
		Type       string        `json:"type"`
		StatusCode int           `json:"status_code"`
		Code       string        `json:"code"`
		Message    string        `json:"message"`
		Error      upstreamError `json:"error"`
		Response   struct {
			Error upstreamError `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return ""
	}
	if event.Type != "error" && event.Type != "response.failed" && event.Type != "response.incomplete" {
		return ""
	}
	for _, e := range []upstreamError{event.Error, event.Response.Error, {Code: event.Code, Message: event.Message, StatusCode: event.StatusCode}} {
		if reason := failureRefreshReason(e.StatusCode, e.Code+" "+e.Type+" "+e.Message); reason != "" {
			return reason
		}
	}
	return ""
}
