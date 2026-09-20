package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func scheduleStatus(t *testing.T, state *runtimeState, body any) int {
	t.Helper()
	raw, err := state.management(managementRequest{Method: "POST", Path: apiBase + "/schedule", Body: jsonBytes(body)})
	if err != nil {
		t.Fatal(err)
	}
	var result struct{ Result struct{ StatusCode int } }
	if json.Unmarshal(raw, &result) != nil {
		t.Fatal("invalid management response")
	}
	return result.Result.StatusCode
}

func TestScheduleUpdatesDueTimeAndSurvivesReload(t *testing.T) {
	state := priorityRuntime(t)
	key := stateKey("auth-a", "gpt-6-astra")
	active := acceptedState(t, state, 12, state.now(), "probe")
	state.acceptStateLocked(key, active)
	if !state.nextRefreshLocked(key).Equal(active.IssuedAt.Add(30 * time.Minute)) {
		t.Fatal("default refresh is not 30 minutes before expiry")
	}
	for _, tc := range []struct{ advance, retry int }{{10, 11}, {45, 3}} {
		body := map[string]any{"revision": state.schedule.Revision, "advance_minutes": tc.advance, "retry_seconds": tc.retry}
		if scheduleStatus(t, state, body) != 200 {
			t.Fatal("schedule update rejected")
		}
		if !state.nextRefreshLocked(key).Equal(active.IssuedAt.Add(time.Duration(60-tc.advance)*time.Minute)) || state.config.Probe.RetrySeconds != tc.retry || state.current[key] != active {
			t.Fatal("schedule did not update immediately or altered the cached state")
		}
	}
	info, err := os.Stat(state.schedulePath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("schedule settings not saved privately")
	}
	state.config.Probe.Enabled = false
	state.config.Probe.RetrySeconds = 15
	state.config.Probe.RefreshBeforeSeconds = 300
	raw, _ := yaml.Marshal(state.config)
	reloaded := newRuntimeState()
	reloaded.hostCall = state.hostCall
	if err := reloaded.configure(jsonBytes(lifecycleRequest{ConfigYAML: raw, SchemaVersion: 6})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reloaded.shutdown)
	if reloaded.config.Probe.RetrySeconds != 3 || reloaded.config.Probe.RefreshBeforeSeconds != 2700 || reloaded.schedule.Revision != 2 {
		t.Fatal("saved UI settings did not survive reload or override old YAML")
	}
}

func TestScheduleValidationConflictAndFailedSave(t *testing.T) {
	state := testRuntime(t, "")
	for _, body := range []map[string]any{
		{}, {"advance_minutes": 0, "retry_seconds": 7}, {"advance_minutes": 60, "retry_seconds": 7},
		{"advance_minutes": 30, "retry_seconds": 0}, {"advance_minutes": 30, "retry_seconds": 3601},
		{"advance_minutes": 30, "retry_seconds": 1.5},
	} {
		if scheduleStatus(t, state, body) != 400 {
			t.Fatal("invalid schedule accepted")
		}
	}
	body := map[string]any{"revision": 0, "advance_minutes": 20, "retry_seconds": 9}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- scheduleStatus(t, state, body) }()
	}
	wg.Wait()
	a, b := <-codes, <-codes
	if !(a == 200 && b == 409 || a == 409 && b == 200) {
		t.Fatal("concurrent edits overwrote each other")
	}
	old := state.schedule
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	state.schedulePath = filepath.Join(blocker, "schedule.json")
	body = map[string]any{"revision": old.Revision, "advance_minutes": 40, "retry_seconds": 5}
	if scheduleStatus(t, state, body) != 500 || state.schedule != old || state.config.Probe.RetrySeconds != 9 || state.config.Probe.RefreshBeforeSeconds != 1200 {
		t.Fatal("failed persistence changed the running schedule")
	}
}

func TestSchedulerHonorsIntervalsBetweenDiscoveryTicks(t *testing.T) {
	state := testRuntime(t, "")
	key := stateKey("auth-a", "gpt-6-astra")
	start := state.now()
	now := start
	state.now = func() time.Time { return now }
	state.lastProbe[key] = start
	if state.nextWakeDelay() != 5*time.Second {
		t.Fatal("discovery cadence changed")
	}
	now = start.Add(5 * time.Second)
	if state.nextWakeDelay() != 2*time.Second {
		t.Fatal("seven-second retry would be rounded up to ten seconds")
	}
	now = start.Add(6 * time.Second)
	if state.nextWakeDelay() != time.Second {
		t.Fatal("retry deadline was not followed")
	}
	state.blockedUntil[key] = start.Add(time.Minute)
	if state.nextWakeDelay() != 5*time.Second {
		t.Fatal("account backoff caused a tight polling loop")
	}
}
