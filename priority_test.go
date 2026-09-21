package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func boolSetting(value bool) *bool { return &value }

func priorityRuntime(t *testing.T) *runtimeState {
	state := guardedRuntime(t)
	state.config.RequireModelMatch = boolSetting(true)
	state.config.InvalidateOnMismatch = boolSetting(true)
	state.config.Prefer292 = boolSetting(true)
	state.config.StandbyEnabled = boolSetting(true)
	state.config.Probe.Continuous = true
	return state
}

func acceptedState(t *testing.T, state *runtimeState, blocks int, issued time.Time, source string) storedState {
	value := makeFernetToken(t, issued, blocks)
	return storedState{Value: value, IssuedAt: issued, Blocks: blocks, Identity: identityHash("auth-a", "account-a"), ResponseModel: "gpt-6-astra", Source: source, CapturedAt: state.now()}
}

func Test292PriorityAndStandbyHandoff(t *testing.T) {
	state := priorityRuntime(t)
	key := stateKey("auth-a", "gpt-6-astra")
	now := state.now()
	state.now = func() time.Time { return now }
	fallback := acceptedState(t, state, 12, now.Add(-10*time.Minute), "probe")
	if got := state.acceptStateLocked(key, fallback); got != "active" {
		t.Fatal(got)
	}
	if !state.acquisitionDueLocked(key).Equal(fallback.IssuedAt.Add(30 * time.Minute)) {
		t.Fatal("fresh 332 did not pause acquisition until refresh")
	}
	preferred := acceptedState(t, state, 10, now.Add(-5*time.Minute), "business")
	if got := state.acceptStateLocked(key, preferred); got != "upgraded_292" {
		t.Fatal(got)
	}
	newFallback := acceptedState(t, state, 12, now.Add(-time.Minute), "probe")
	if got := state.acceptStateLocked(key, newFallback); got != "standby" {
		t.Fatal(got)
	}
	if state.current[key].Value != preferred.Value {
		t.Fatal("332 replaced valid 292")
	}
	next := acceptedState(t, state, 10, now, "business")
	if got := state.acceptStateLocked(key, next); got != "standby" {
		t.Fatal(got)
	}
	if state.current[key].Value != preferred.Value || state.standby[key].Value != next.Value {
		t.Fatal("standby interrupted the current state")
	}
	if got := state.acceptStateLocked(key, next); got != "duplicate" {
		t.Fatal("echo was accepted again")
	}
	newer332 := acceptedState(t, state, 12, now.Add(time.Second), "probe")
	if got := state.acceptStateLocked(key, newer332); got != "lower_priority" {
		t.Fatal("332 replaced preferred standby")
	}
	now = preferred.IssuedAt.Add(time.Hour)
	if !state.promoteStandbyLocked(key) || state.current[key].Value != next.Value {
		t.Fatal("standby did not take over on expiry")
	}
}

func TestBusiness292RecoveryNeedsSuccessAndMatchingModel(t *testing.T) {
	for _, kind := range []string{"good", "failed", "mismatch", "missing_model"} {
		t.Run(kind, func(t *testing.T) {
			state := priorityRuntime(t)
			key := stateKey("auth-a", "gpt-6-astra")
			state.config.ArchiveDir = filepath.Join(t.TempDir(), "archive")
			active := acceptedState(t, state, 10, state.now().Add(-5*time.Minute), "probe")
			state.acceptStateLocked(key, active)
			calls := 0
			state.fetch = func(context.Context, probeAuth, string, *proxyEndpoint, proxyEndpoint) (string, string, string) {
				calls++
				return "", "network_error", ""
			}
			begin(t, state, "business", "auth-a", "gpt-6-astra", "")
			newValue := makeFernetToken(t, state.now(), 10)
			state.captureCandidate("business", http.Header{turnStateHeader: {newValue}})
			model := "gpt-6-astra"
			outcome := "succeeded"
			status := "completed"
			if kind == "mismatch" {
				model = "gpt-5.6-luna"
			}
			if kind == "missing_model" {
				model = ""
			}
			if kind == "failed" {
				outcome = "failed"
				status = "failed"
			}
			state.observeBody("business", jsonBytes(map[string]any{"type": "response." + status, "response": map[string]string{"status": status, "model": model}}))
			finish(t, state, "business", outcome, false)
			row := state.history[len(state.history)-1]
			if calls != 0 {
				t.Fatal("business recovery generated a probe")
			}
			if kind == "good" {
				if state.standby[key].Value != newValue || state.standby[key].Source != "business" || row.CacheAction != "standby" || row.Acceptance != "accepted" {
					t.Fatalf("business standby not recorded: %+v", row)
				}
				original := state.standby[key]
				now := active.IssuedAt.Add(30 * time.Minute)
				state.now = func() time.Time { return now }
				state.ensureProbe("auth-a", "gpt-6-astra")
				if calls != 0 || !state.nextRefreshLocked(key).Equal(original.IssuedAt.Add(30*time.Minute)) {
					t.Fatal("business standby failed to avoid the old active refresh")
				}
				begin(t, state, "echo", "auth-a", "gpt-6-astra", "")
				state.captureCandidate("echo", http.Header{turnStateHeader: {newValue}})
				state.observeBody("echo", []byte(`{"type":"response.completed","response":{"status":"completed","model":"gpt-6-astra"}}`))
				finish(t, state, "echo", "succeeded", false)
				if state.standby[key] != original || state.history[len(state.history)-1].CacheAction != "duplicate" {
					t.Fatal("echo renewed standby time or provenance")
				}
			} else if len(state.standby) != 0 {
				t.Fatal("unverified business state entered standby")
			}
			if kind == "mismatch" {
				if len(state.current) != 0 || !begin(t, state, "after_mismatch", "auth-a", "gpt-6-astra", "").Terminate {
					t.Fatal("mismatched state did not close the request gate")
				}
			}
		})
	}
}

func TestLateMismatchDoesNotInvalidateNewCache(t *testing.T) {
	state := priorityRuntime(t)
	key := stateKey("auth-a", "gpt-6-astra")
	old := acceptedState(t, state, 12, state.now().Add(-time.Minute), "probe")
	state.acceptStateLocked(key, old)
	begin(t, state, "old_request", "auth-a", "gpt-6-astra", "")
	preferred := acceptedState(t, state, 10, state.now(), "probe")
	state.acceptStateLocked(key, preferred)
	state.observeBody("old_request", []byte(`{"type":"response.completed","response":{"status":"completed","model":"gpt-5.6-luna"}}`))
	finish(t, state, "old_request", "succeeded", false)
	if state.current[key].Value != preferred.Value {
		t.Fatal("late request invalidated newly acquired 292")
	}
}

func TestProbeModelAcceptancePausesOn332UntilRefresh(t *testing.T) {
	state := priorityRuntime(t)
	key := stateKey("auth-a", "gpt-6-astra")
	now := state.now()
	state.now = func() time.Time { return now }
	calls := 0
	state.fetch = func(context.Context, probeAuth, string, *proxyEndpoint, proxyEndpoint) (string, string, string) {
		calls++
		switch calls {
		case 1:
			return makeFernetToken(t, now, 10), "ok", "gpt-5.6-luna"
		case 2:
			return makeFernetToken(t, now, 12), "ok", "gpt-6-astra"
		default:
			return makeFernetToken(t, now, 10), "ok", "gpt-6-astra"
		}
	}
	state.ensureProbe("auth-a", "gpt-6-astra")
	if len(state.current) != 0 || state.history[0].Acceptance != "model_mismatch" {
		t.Fatal("mismatched probe was cached")
	}
	now = now.Add(time.Duration(state.config.Probe.RetrySeconds) * time.Second)
	state.ensureProbe("auth-a", "gpt-6-astra")
	if len(state.current[key].Value) != 332 {
		t.Fatal("332 was not available as fallback")
	}
	now = now.Add(time.Duration(state.config.Probe.RetrySeconds) * time.Second)
	state.ensureProbe("auth-a", "gpt-6-astra")
	if calls != 2 || len(state.current[key].Value) != 332 {
		t.Fatal("fresh 332 triggered another acquisition")
	}
	now = state.current[key].IssuedAt.Add(30 * time.Minute)
	state.ensureProbe("auth-a", "gpt-6-astra")
	if calls != 3 || len(state.current[key].Value) != 292 {
		t.Fatal("scheduled refresh failed to accept and prefer 292")
	}
	now = now.Add(time.Duration(state.config.Probe.RetrySeconds) * time.Second)
	state.ensureProbe("auth-a", "gpt-6-astra")
	if calls != 3 {
		t.Fatal("fresh 292 did not pause acquisition")
	}
}

func TestHistoryAndStandbySurviveReload(t *testing.T) {
	state := priorityRuntime(t)
	dir := t.TempDir()
	state.config.StateFile = filepath.Join(dir, "state.json")
	state.config.RuntimeFile = filepath.Join(dir, "runtime.json")
	state.config.SelectionFile = filepath.Join(dir, "selection.json")
	if err := persistSelection(state.config.SelectionFile, state.selection); err != nil {
		t.Fatal(err)
	}
	key := stateKey("auth-a", "gpt-6-astra")
	state.acceptStateLocked(key, acceptedState(t, state, 10, state.now().Add(-time.Minute), "probe"))
	state.acceptStateLocked(key, acceptedState(t, state, 10, state.now(), "business"))
	state.recordLocked(key, 292, true, "replaced", "medium")
	state.history[0].Acceptance = "accepted"
	state.history[0].StateSource = "business"
	state.history[0].ExitIP = "203.0.113.24"
	state.history[0].ProxyLabel = "Saved proxy label"
	state.journalRevision++
	state.flushPersistence()
	for _, p := range []string{state.config.StateFile, state.config.RuntimeFile} {
		info, err := os.Stat(p)
		if err != nil || goruntime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatal("persistence not private")
		}
	}
	state.config.Probe.Enabled = false
	config, err := yaml.Marshal(state.config)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := newRuntimeState()
	reloaded.now = state.now
	reloaded.hostCall = state.hostCall
	if err := reloaded.configure(jsonBytes(lifecycleRequest{ConfigYAML: config, SchemaVersion: 4})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reloaded.shutdown)
	if len(reloaded.history) != 1 || reloaded.standby[key].Source != "business" {
		t.Fatal("runtime history or standby lost on reload")
	}
	if reloaded.history[0].ExitIP != "203.0.113.24" || reloaded.history[0].ProxyLabel != "Saved proxy label" {
		t.Fatal("historical egress or proxy label lost on reload")
	}
	status, err := reloaded.statusResponse()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(status), state.standby[key].Value) {
		t.Fatal("state value leaked in status")
	}
	var decoded runtimeJournal
	if err := readPrivateJSON(state.config.RuntimeFile, 4<<20, &decoded); err != nil || decoded.History[0].StateSource != "business" {
		t.Fatal("history was not persisted")
	}
}

func TestLegacyStateCannotInventModelEvidence(t *testing.T) {
	state := priorityRuntime(t)
	key := stateKey("auth-a", "gpt-6-astra")
	legacy := acceptedState(t, state, 10, state.now(), "")
	legacy.ResponseModel = ""
	if state.acceptStateLocked(key, legacy) != "rejected" {
		t.Fatal("legacy state without evidence passed strict admission")
	}
	for _, model := range []string{"gpt-6-astra-mini", "gpt-5.6-luna", "gpt-6"} {
		if modelConsistent("gpt-6-astra", model) {
			t.Fatal("a partial name passed exact model admission")
		}
	}
}

func TestFresh332StandbyPostponesAcquisitionAndSurvivesActiveExpiry(t *testing.T) {
	for _, activeBlocks := range []int{10, 12} {
		t.Run(map[int]string{10: "active292", 12: "active332"}[activeBlocks], func(t *testing.T) {
			state := priorityRuntime(t)
			key := stateKey("auth-a", "gpt-6-astra")
			now := state.now()
			state.now = func() time.Time { return now }
			active := acceptedState(t, state, activeBlocks, now.Add(-54*time.Minute), "probe")
			state.acceptStateLocked(key, active)
			standby := acceptedState(t, state, 12, now, "business")
			state.acceptStateLocked(key, standby)
			calls := 0
			state.fetch = func(context.Context, probeAuth, string, *proxyEndpoint, proxyEndpoint) (string, string, string) {
				calls++
				return makeFernetToken(t, now, 12), "ok", "gpt-6-astra"
			}
			now = active.IssuedAt.Add(55 * time.Minute)
			state.ensureProbe("auth-a", "gpt-6-astra")
			if calls != 0 || !state.nextRefreshLocked(key).Equal(standby.IssuedAt.Add(30*time.Minute)) {
				t.Fatal("valid 332 standby did not postpone refresh")
			}
			now = active.IssuedAt.Add(time.Hour)
			state.ensureProbe("auth-a", "gpt-6-astra")
			if calls != 0 || state.current[key].Value != standby.Value {
				t.Fatal("active expiry failed to use 332 standby without probing")
			}
			now = standby.IssuedAt.Add(30 * time.Minute)
			state.ensureProbe("auth-a", "gpt-6-astra")
			if calls != 1 {
				t.Fatal("332 standby did not refresh near expiry")
			}
			now = now.Add(time.Duration(state.config.Probe.RetrySeconds) * time.Second)
			state.ensureProbe("auth-a", "gpt-6-astra")
			if calls != 1 {
				t.Fatal("successful 332 refresh triggered another acquisition")
			}
		})
	}
}

func TestModelMismatchPreservesValidCacheByDefault(t *testing.T) {
	state := priorityRuntime(t)
	// By default (InvalidateOnMismatch: nil/false), model mismatch preserves valid cache
	state.config.InvalidateOnMismatch = nil
	key := stateKey("auth-a", "gpt-6-astra")
	active := acceptedState(t, state, 10, state.now().Add(-5*time.Minute), "probe")
	state.acceptStateLocked(key, active)

	begin(t, state, "req-1", "auth-a", "gpt-6-astra", "")
	newValue := makeFernetToken(t, state.now(), 11)
	state.captureCandidate("req-1", http.Header{turnStateHeader: {newValue}})
	state.observeBody("req-1", jsonBytes(map[string]any{
		"type": "response.completed",
		"response": map[string]string{"status": "completed", "model": "gpt-5.6-luna"},
	}))
	finish(t, state, "req-1", "succeeded", false)

	if state.standby[key].Value != "" {
		t.Fatal("candidate state on mismatch entered standby")
	}
	if state.current[key].Value != active.Value {
		t.Fatal("active 292 cache was wiped out on model mismatch")
	}
	resp := begin(t, state, "req-2", "auth-a", "gpt-6-astra", "")
	if resp.Terminate {
		t.Fatal("gate was closed despite valid active cache")
	}
	if resp.Headers.Get(turnStateHeader) != active.Value {
		t.Fatalf("expected injected state %s, got %s", active.Value, resp.Headers.Get(turnStateHeader))
	}
}
