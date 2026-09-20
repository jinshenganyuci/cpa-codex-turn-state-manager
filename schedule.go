package main

import (
	"encoding/json"
	"errors"
)

const defaultRefreshBeforeSeconds = 30 * 60

type acquisitionSchedule struct {
	Version              int    `json:"version"`
	Revision             uint64 `json:"revision"`
	RefreshBeforeSeconds int    `json:"refresh_before_seconds"`
	RetrySeconds         int    `json:"retry_seconds"`
	ProxyConcurrency     int    `json:"proxy_concurrency"`
}

func loadAcquisitionSchedule(path string, probe probeConfig) (acquisitionSchedule, error) {
	schedule := acquisitionSchedule{Version: 1, RefreshBeforeSeconds: probe.RefreshBeforeSeconds, RetrySeconds: probe.RetrySeconds, ProxyConcurrency: probe.ProxyConcurrency}
	if err := readPrivateJSON(path, 4096, &schedule); err != nil {
		return schedule, errors.New("schedule_file_invalid")
	}
	if schedule.Version != 1 || schedule.RefreshBeforeSeconds <= 0 || schedule.RefreshBeforeSeconds >= 3600 || schedule.RetrySeconds < 1 || schedule.ProxyConcurrency < 1 || schedule.ProxyConcurrency > maxProxyConcurrency {
		return schedule, errors.New("schedule_file_invalid")
	}
	return schedule, nil
}

// Caller holds mu. This object is independent of selections and transient history.
func (state *runtimeState) scheduleViewLocked() map[string]any {
	return map[string]any{
		"revision": state.schedule.Revision, "persistent": state.schedulePath != "",
		"advance_minutes":           float64(state.config.Probe.RefreshBeforeSeconds) / 60,
		"retry_seconds":             state.config.Probe.RetrySeconds,
		"default_advance_minutes":   defaultRefreshBeforeSeconds / 60,
		"default_retry_seconds":     defaultProbeRetrySeconds,
		"proxy_concurrency":         state.config.Probe.ProxyConcurrency,
		"default_proxy_concurrency": defaultProxyConcurrency,
	}
}

func (state *runtimeState) updateSchedule(raw []byte) ([]byte, error) {
	var body struct {
		Revision         uint64 `json:"revision"`
		AdvanceMinutes   *int   `json:"advance_minutes"`
		RetrySeconds     *int   `json:"retry_seconds"`
		ProxyConcurrency *int   `json:"proxy_concurrency"`
	}
	if len(raw) > 4096 || json.Unmarshal(raw, &body) != nil || body.AdvanceMinutes == nil || body.RetrySeconds == nil || *body.AdvanceMinutes < 1 || *body.AdvanceMinutes > 59 || *body.RetrySeconds < 1 || *body.RetrySeconds > 3600 || body.ProxyConcurrency != nil && (*body.ProxyConcurrency < 1 || *body.ProxyConcurrency > maxProxyConcurrency) {
		return managementJSON(400, map[string]string{"error": "invalid_schedule"})
	}
	state.scheduleMu.Lock()
	defer state.scheduleMu.Unlock()
	state.mu.Lock()
	if body.Revision != state.schedule.Revision {
		state.mu.Unlock()
		return managementJSON(409, map[string]string{"error": "schedule_changed_reload"})
	}
	concurrency := state.config.Probe.ProxyConcurrency
	if body.ProxyConcurrency != nil {
		concurrency = *body.ProxyConcurrency
	}
	path := state.schedulePath
	state.mu.Unlock()
	if path == "" {
		return managementJSON(400, map[string]string{"error": "schedule_persistence_required"})
	}
	next := acquisitionSchedule{Version: 1, Revision: body.Revision + 1, RefreshBeforeSeconds: *body.AdvanceMinutes * 60, RetrySeconds: *body.RetrySeconds, ProxyConcurrency: concurrency}
	if err := writePrivateJSON(path, next); err != nil {
		return managementJSON(500, map[string]string{"error": "schedule_save_failed"})
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.schedule = next
	state.config.Probe.RefreshBeforeSeconds = next.RefreshBeforeSeconds
	state.config.Probe.RetrySeconds = next.RetrySeconds
	state.config.Probe.ProxyConcurrency = next.ProxyConcurrency
	select {
	case state.wake <- struct{}{}:
	default:
	}
	return managementJSON(200, map[string]any{"ok": true, "schedule": state.scheduleViewLocked()})
}
