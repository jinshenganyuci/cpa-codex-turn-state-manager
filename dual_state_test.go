package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

func TestDefaultPolicyAcceptsBothTargetLengths(t *testing.T) {
	for _, plan := range []string{"", "team", "plus", "pro"} {
		policy, err := normalizeCredential(credentialConfig{Plan: plan})
		if err != nil {
			t.Fatal(err)
		}
		for _, blocks := range []int{10, 11, 12, 13} {
			if normalBlockCount(policy, blocks) != (blocks == 10 || blocks == 12) {
				t.Fatalf("unexpected default policy for plan %q, blocks %d", plan, blocks)
			}
		}
	}
	policy, err := normalizeCredential(credentialConfig{AcceptedBlocks: []int{10}})
	if err != nil || normalBlockCount(policy, 12) {
		t.Fatal("explicit accepted_blocks policy was widened")
	}
}

func TestBothLengthsPersistArchiveAndReplayLatest(t *testing.T) {
	dir := t.TempDir()
	config := "mode: force\narchive_dir: " + filepath.Join(dir, "archive") + "\nstate_file: " + filepath.Join(dir, "current.json") + "\n"
	state := testRuntime(t, config)
	state.config.Defaults.AcceptedBlocks = []int{10, 12}
	key := stateKey("auth-a", "gpt-6-astra")
	now := state.now()
	var latest string
	for index, tc := range []struct {
		blocks, age int
	}{{12, 10}, {10, 9}, {12, 9}} {
		id := []string{"first332", "next292", "same_second332"}[index]
		latest = makeFernetToken(t, now.Add(-time.Duration(tc.age)*time.Second), tc.blocks)
		begin(t, state, id, "auth-a", "gpt-6-astra", "")
		state.captureCandidate(id, http.Header{turnStateHeader: {latest}})
		finish(t, state, id, "succeeded", true)
		if got := headerValue(begin(t, state, id+"-replay", "auth-a", "gpt-6-astra", "").Headers, turnStateHeader); got != latest {
			t.Fatalf("latest successful state was not replayed at %s", id)
		}
		persisted, err := loadPersisted(state.config.StateFile, 4096)
		if err != nil || persisted[key].Value != latest {
			t.Fatalf("full active value was not persisted at %s", id)
		}
	}
	for _, tc := range []struct {
		id          string
		blocks, age int
		outcome     string
	}{{"older292", 10, 11, "succeeded"}, {"unaccepted356", 13, 0, "succeeded"}, {"failed292", 10, 0, "failed"}} {
		begin(t, state, tc.id, "auth-a", "gpt-6-astra", "")
		state.captureCandidate(tc.id, http.Header{turnStateHeader: {makeFernetToken(t, now.Add(-time.Duration(tc.age)*time.Second), tc.blocks)}})
		finish(t, state, tc.id, tc.outcome, true)
		if state.current[key].Value != latest {
			t.Fatalf("%s replaced the latest valid state", tc.id)
		}
	}
	files, err := filepath.Glob(filepath.Join(dir, "archive", "*.json"))
	if err != nil || len(files) != 4 {
		t.Fatalf("expected both lengths and successful older archive, got %d", len(files))
	}
	counts := map[int]int{}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var saved struct {
			State  storedState `json:"state"`
			Length int         `json:"length"`
			SHA256 string      `json:"sha256"`
		}
		if json.Unmarshal(raw, &saved) != nil || saved.Length != len(saved.State.Value) || saved.SHA256 != digest(saved.State.Value) {
			t.Fatal("archive lost full state or metadata")
		}
		counts[saved.Length]++
		info, err := os.Stat(path)
		if err != nil || goruntime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatal("archive permission is not private")
		}
	}
	if counts[292] != 2 || counts[332] != 2 {
		t.Fatalf("wrong archive lengths: %v", counts)
	}
	reloaded := newRuntimeState()
	reloaded.hostCall = testHost("account-a", "socks5://proxy.invalid:1080")
	reloaded.now = state.now
	if err := reloaded.configure(jsonBytes(lifecycleRequest{ConfigYAML: []byte(config + "require_model_match: false\nprefer_292: false\nstandby_enabled: false\nselection_required: false\ndefaults:\n  models: [gpt-6-astra]\n"), SchemaVersion: 4})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reloaded.shutdown)
	if got := headerValue(begin(t, reloaded, "after_restart", "auth-a", "gpt-6-astra", "").Headers, turnStateHeader); got != latest {
		t.Fatal("restart did not restore full 332 state")
	}
	raw, _ := reloaded.statusResponse()
	if strings.Contains(string(raw), latest) {
		t.Fatal("status leaked the active state")
	}
}

func TestRefreshAt30MinutesAcceptsEitherLength(t *testing.T) {
	for _, blocks := range []int{10, 12} {
		t.Run(map[int]string{10: "292_to_332", 12: "332_to_292"}[blocks], func(t *testing.T) {
			dir := t.TempDir()
			state := testRuntime(t, "mode: force\narchive_dir: "+filepath.Join(dir, "archive")+"\nstate_file: "+filepath.Join(dir, "current.json")+"\n")
			state.config.Defaults.AcceptedBlocks = []int{10, 12}
			state.config.Probe.Enabled = true
			state.config.Probe.MaxAttempts = 3
			key := stateKey("auth-a", "gpt-6-astra")
			issued := state.now()
			now := issued
			state.now = func() time.Time { return now }
			old := makeFernetToken(t, issued, blocks)
			seed(state, old)
			calls := 0
			var next string
			state.fetch = func(_ context.Context, _ probeAuth, _ string, _ *proxyEndpoint, _ proxyEndpoint) (string, string, string) {
				calls++
				next = makeFernetToken(t, now, 22-blocks)
				return next, "ok", ""
			}
			now = issued.Add(30*time.Minute - time.Second)
			state.refreshDue(context.Background())
			state.probeWG.Wait()
			if calls != 0 {
				t.Fatal("refreshed before the 30-minute threshold")
			}
			now = issued.Add(30 * time.Minute)
			state.refreshDue(context.Background())
			state.probeWG.Wait()
			if calls != 1 || state.current[key].Value != next {
				t.Fatal("did not stop on the first accepted refresh value")
			}
			persisted, err := loadPersisted(state.config.StateFile, 4096)
			if err != nil || persisted[key].Value != next {
				t.Fatal("refreshed state was not persisted")
			}
			files, err := filepath.Glob(filepath.Join(dir, "archive", "*.json"))
			if err != nil || len(files) != 1 {
				t.Fatal("refreshed state was not archived")
			}

			if got := headerValue(begin(t, state, "refreshed", "auth-a", "gpt-6-astra", "").Headers, turnStateHeader); got != next {
				t.Fatal("refreshed value not applied")
			}
			if got := state.nextRefreshLocked(key); !got.Equal(now.Add(30 * time.Minute)) {
				t.Fatalf("unexpected next refresh: %v", got)
			}
			state.refreshDue(context.Background())
			state.probeWG.Wait()
			if calls != 1 {
				t.Fatal("immediately probed again after successful refresh")
			}
		})
	}
}

func TestFailedRefreshKeepsUnexpiredStateAndRetries(t *testing.T) {
	state := testRuntime(t, "mode: force\n")
	state.config.Defaults.AcceptedBlocks = []int{10, 12}
	state.config.Probe.Enabled = true
	key := stateKey("auth-a", "gpt-6-astra")
	issued := state.now()
	now := issued.Add(30 * time.Minute)
	state.now = func() time.Time { return now }
	old := makeFernetToken(t, issued, 12)
	seed(state, old)
	calls := 0
	state.fetch = func(context.Context, probeAuth, string, *proxyEndpoint, proxyEndpoint) (string, string, string) {
		calls++
		return "", "upstream_http_502_server_is_overloaded", ""
	}
	state.refreshDue(context.Background())
	state.probeWG.Wait()
	if calls != 1 || state.current[key].Value != old {
		t.Fatal("failed refresh discarded old state")
	}
	if got := headerValue(begin(t, state, "during_failure", "auth-a", "gpt-6-astra", "").Headers, turnStateHeader); got != old {
		t.Fatal("unexpired state was not usable after refresh failure")
	}
	state.refreshDue(context.Background())
	state.probeWG.Wait()
	if calls != 1 {
		t.Fatal("retry spacing ignored")
	}
	now = now.Add(time.Minute)
	state.refreshDue(context.Background())
	state.probeWG.Wait()
	if calls != 2 {
		t.Fatal("failed refresh was not retried after configured spacing")
	}
	now = issued.Add(time.Hour)
	if len(begin(t, state, "expired", "auth-a", "gpt-6-astra", "").Headers) != 0 {
		t.Fatal("state reused at or after one hour")
	}
	if state.current[key].Value != old {
		t.Fatal("expiry destroyed the saved state")
	}
}
