package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func batchPool(t *testing.T, state *runtimeState, count int) {
	t.Helper()
	for i := range count {
		addPoolProxy(t, state.pool, fmt.Sprintf("socks5://proxy-%d.invalid:1080", i), true)
	}
	poolChange(t, state.pool, "mode", poolEdit{Enabled: true})
}

func TestProxyBatchesCoverPreviousOmissions(t *testing.T) {
	state := priorityRuntime(t)
	batchPool(t, state, 4)
	previous := map[string]bool{}
	for range 100 {
		routes, reason := state.pool.routes("auth-a", "", 3, state.now())
		if reason != "" || len(routes) != 3 {
			t.Fatal("wrong batch size", reason)
		}
		seen := map[string]bool{}
		for _, route := range routes {
			if seen[route.ticket.ID] {
				t.Fatal("duplicate proxy within batch")
			}
			seen[route.ticket.ID] = true
		}
		for _, entry := range state.pool.data.Entries {
			if len(previous) > 0 && !previous[entry.ID] && !seen[entry.ID] {
				t.Fatal("next batch skipped a previous omission")
			}
		}
		previous = seen
	}
	// Activity by another account must not change this account's rotation state.
	before := fmt.Sprint(state.pool.selections["auth-a"])
	_, _ = state.pool.routes("auth-b", "", 3, state.now())
	if before != fmt.Sprint(state.pool.selections["auth-a"]) {
		t.Fatal("credential fairness state shared")
	}
	state.pool.data.Entries[0].Enabled = false
	state.pool.data.Entries[1].CooldownUntil = state.now().Add(time.Minute)
	routes, _ := state.pool.routes("auth-a", "", 3, state.now())
	if len(routes) != 2 {
		t.Fatal("disabled or cooling proxies were included")
	}
	for _, route := range routes {
		if route.ticket.ID == state.pool.data.Entries[0].ID || route.ticket.ID == state.pool.data.Entries[1].ID {
			t.Fatal("ineligible proxy selected")
		}
	}
}

func receiveBatch(t *testing.T, started <-chan string, count int) map[string]bool {
	t.Helper()
	seen := make(map[string]bool)
	for range count {
		select {
		case route := <-started:
			if seen[route] {
				t.Fatal("duplicate concurrent route")
			}
			seen[route] = true
		case <-time.After(2 * time.Second):
			t.Fatal("requests did not start concurrently")
		}
	}
	return seen
}

func TestProbeBatchesEnforceSharedCredentialLimitAndRotate(t *testing.T) {
	state := priorityRuntime(t)
	state.selection.Models = []string{"gpt-6-astra", "gpt-5.6-sol"}
	state.config.Defaults.Models = state.selection.Models
	batchPool(t, state, 4)
	started, release := make(chan string, 6), make(chan struct{})
	state.fetch = func(ctx context.Context, _ probeAuth, _ string, _ *proxyEndpoint, endpoint proxyEndpoint) (string, string, string) {
		started <- endpoint.URL
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "", "state_missing", ""
	}
	if reason := state.startProbe("auth-a", "gpt-6-astra", true); reason != "" {
		t.Fatal(reason)
	}
	first := receiveBatch(t, started, 3)
	if reason := state.startProbe("auth-a", "gpt-5.6-sol", true); reason != "credential_probe_limit" {
		t.Fatal("another model exceeded the credential cap", reason)
	}
	select {
	case <-started:
		t.Fatal("fourth request started")
	default:
	}
	close(release)
	state.probeWG.Wait()
	if len(state.credentialInFlight) != 0 || len(state.probing) != 0 {
		t.Fatal("completed wave leaked reservations")
	}
	select {
	case <-started:
		t.Fatal("rejected task was queued")
	default:
	}
	if reason := state.startProbe("auth-a", "gpt-5.6-sol", true); reason != "" {
		t.Fatal("capacity was not reusable", reason)
	}
	second := receiveBatch(t, started, 3)
	state.probeWG.Wait()
	for _, entry := range state.pool.data.Entries {
		if !first[entry.URL] && !second[entry.URL] {
			t.Fatal("rotation did not span models of the same credential")
		}
	}
}

func TestBatchSuccessCancelsSiblingsAndPreserves292Priority(t *testing.T) {
	for _, mode := range []string{"332", "292", "quota", "parent_cancel"} {
		t.Run(mode, func(t *testing.T) {
			state := priorityRuntime(t)
			batchPool(t, state, 3)
			started, release := make(chan string, 3), make(chan struct{})
			fallback := makeFernetToken(t, state.now(), 12)
			preferred := makeFernetToken(t, state.now(), 10)
			winner := state.pool.data.Entries[0].URL
			state.fetch = func(ctx context.Context, _ probeAuth, model string, _ *proxyEndpoint, endpoint proxyEndpoint) (string, string, string) {
				started <- endpoint.URL
				<-release
				if endpoint.URL == winner {
					if mode == "quota" {
						return "", "upstream_http_429", ""
					}
					return fallback, "ok", model
				}
				<-ctx.Done()
				if mode == "292" {
					// Simulate a completed response racing the sibling cancellation.
					return preferred, "ok", model
				}
				return "", "network_error", ""
			}
			if reason := state.startProbe("auth-a", "gpt-6-astra", true); reason != "" {
				t.Fatal(reason)
			}
			receiveBatch(t, started, 3)
			key := stateKey("auth-a", "gpt-6-astra")
			if mode == "parent_cancel" {
				state.mu.Lock()
				state.activeCancels[key]()
				state.mu.Unlock()
			}
			close(release)
			finished := make(chan struct{})
			go func() { state.probeWG.Wait(); close(finished) }()
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("sibling requests were not cancelled")
			}
			if len(state.credentialInFlight) != 0 || len(state.history) != 3 {
				t.Fatal("reservations leaked or per-proxy history missing")
			}
			switch mode {
			case "332", "292":
				want := fallback
				if mode == "292" {
					want = preferred
				}
				if state.current[key].Value != want || state.observed[key] != len(want) {
					t.Fatal("winner not cached with 292 preference")
				}
				state.ensureProbe("auth-a", "gpt-6-astra")
				if len(state.history) != 3 {
					t.Fatal("fresh cache caused another wave")
				}
			case "quota":
				if len(state.current) != 0 || !state.blockedUntil[key].After(state.now()) {
					t.Fatal("quota failure did not back off")
				}
			case "parent_cancel":
				if len(state.current) != 0 || state.probeResults[key] != "cancelled" {
					t.Fatal("cancelled wave cached a result")
				}
			}
		})
	}
}

func TestConcurrencyScheduleMigrationPersistenceAndValidation(t *testing.T) {
	state := priorityRuntime(t)
	path := filepath.Join(t.TempDir(), "old.schedule.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"revision":2,"refresh_before_seconds":600,"retry_seconds":11}`), 0600); err != nil {
		t.Fatal(err)
	}
	old, err := loadAcquisitionSchedule(path, state.config.Probe)
	if err != nil || old.ProxyConcurrency != 3 || old.RetrySeconds != 11 || old.RefreshBeforeSeconds != 600 {
		t.Fatal("old schedule was not migrated additively", err)
	}
	for _, bad := range []any{0, 129, 1.5, "3"} {
		if scheduleStatus(t, state, map[string]any{"advance_minutes": 30, "retry_seconds": 7, "proxy_concurrency": bad}) != 400 {
			t.Fatal("invalid concurrency accepted", bad)
		}
	}
	for _, count := range []int{1, 4, 2, 128, 3} {
		if scheduleStatus(t, state, map[string]any{"revision": state.schedule.Revision, "advance_minutes": 30, "retry_seconds": 7, "proxy_concurrency": count}) != 200 {
			t.Fatal("valid concurrency rejected")
		}
		if state.config.Probe.ProxyConcurrency != count {
			t.Fatal("concurrency change not applied")
		}
		saved, err := loadAcquisitionSchedule(state.schedulePath, state.config.Probe)
		if err != nil || saved.ProxyConcurrency != count {
			t.Fatal("concurrency not persisted")
		}
		if scheduleStatus(t, state, map[string]any{"revision": state.schedule.Revision, "advance_minutes": 20, "retry_seconds": 9}) != 200 || state.config.Probe.ProxyConcurrency != count {
			t.Fatal("old UI reset the concurrency")
		}
	}
}

func TestCustomProxyConcurrencyAndHourlyBudget(t *testing.T) {
	for _, count := range []int{1, 2, 4, 6} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			state := priorityRuntime(t)
			batchPool(t, state, 4)
			state.config.Probe.ProxyConcurrency = count
			started, release := make(chan string, 4), make(chan struct{})
			state.fetch = func(ctx context.Context, _ probeAuth, _ string, _ *proxyEndpoint, endpoint proxyEndpoint) (string, string, string) {
				started <- endpoint.URL
				select {
				case <-release:
				case <-ctx.Done():
				}
				return "", "state_missing", ""
			}
			if reason := state.startProbe("auth-a", "gpt-6-astra", true); reason != "" {
				t.Fatal(reason)
			}
			receiveBatch(t, started, min(count, 4))
			close(release)
			state.probeWG.Wait()
			if len(state.history) != min(count, 4) {
				t.Fatal("custom concurrency not enforced")
			}
		})
	}
	state := priorityRuntime(t)
	batchPool(t, state, 4)
	state.config.Probe.Continuous = false
	state.config.Probe.MaxPerHour = 2
	state.config.Probe.MaxAttempts = 2
	state.fetch = func(context.Context, probeAuth, string, *proxyEndpoint, proxyEndpoint) (string, string, string) {
		return "", "state_missing", ""
	}
	state.ensureProbe("auth-a", "gpt-6-astra")
	if state.probeCounts["auth-a"] != 2 || len(state.history) != 2 {
		t.Fatal("parallel batch exceeded hourly request budget")
	}
}

func TestConcurrentCredentialsDoNotShareCapacity(t *testing.T) {
	state := priorityRuntime(t)
	batchPool(t, state, 4)
	state.selection.Accounts["auth-b"] = identityHash("auth-b", "account-a")
	started := make(chan string, 6)
	state.fetch = func(ctx context.Context, _ probeAuth, _ string, _ *proxyEndpoint, endpoint proxyEndpoint) (string, string, string) {
		started <- endpoint.URL
		<-ctx.Done()
		return "", "cancelled", ""
	}
	for _, auth := range []string{"auth-a", "auth-b"} {
		if reason := state.startProbe(auth, "gpt-6-astra", true); reason != "" {
			t.Fatal(reason)
		}
		receiveBatch(t, started, 3)
	}
	state.mu.Lock()
	for _, cancel := range state.activeCancels {
		cancel()
	}
	state.mu.Unlock()
	state.probeWG.Wait()
	if len(state.credentialInFlight) != 0 {
		t.Fatal("account capacity leaked")
	}
}

func TestLoweredLimitAppliesToNextWave(t *testing.T) {
	state := priorityRuntime(t)
	batchPool(t, state, 4)
	state.config.Probe.MaxAttempts = 2
	started, release := make(chan string, 4), make(chan struct{})
	state.fetch = func(ctx context.Context, _ probeAuth, _ string, _ *proxyEndpoint, endpoint proxyEndpoint) (string, string, string) {
		started <- endpoint.URL
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "", "state_missing", ""
	}
	if reason := state.startProbe("auth-a", "gpt-6-astra", true); reason != "" {
		t.Fatal(reason)
	}
	receiveBatch(t, started, 3)
	if scheduleStatus(t, state, map[string]any{"advance_minutes": 30, "retry_seconds": 7, "proxy_concurrency": 1}) != 200 {
		t.Fatal("save failed")
	}
	close(release)
	state.probeWG.Wait()
	if len(state.history) != 4 || len(started) != 1 || len(state.credentialInFlight) != 0 {
		t.Fatal("lowered limit did not constrain next wave")
	}
}
