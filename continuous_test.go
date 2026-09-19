package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func continuousRuntime(t *testing.T) *runtimeState {
	t.Helper()
	state := testRuntime(t, "mode: force\n")
	state.config.Defaults.AcceptedBlocks = []int{10, 12}
	state.config.Defaults.Models = []string{"gpt-6-astra"}
	state.config.Probe.Enabled = true
	state.config.Probe.Continuous = true
	state.config.Probe.RetrySeconds = 10
	state.config.Probe.MaxPerHour = 2
	base := testHost("account-a", "socks5://proxy.invalid:1080")
	state.hostCall = func(method string, request, result any) error {
		if method == "host.auth.list" {
			return json.Unmarshal(jsonBytes(map[string]any{"files": []any{map[string]string{"id": "auth-a", "auth_index": "a", "provider": "codex"}}}), result)
		}
		return base(method, request, result)
	}
	return state
}

func TestContinuousDiscoversAndRetriesUntilEitherTarget(t *testing.T) {
	for _, blocks := range []int{10, 12} {
		t.Run(map[int]string{10: "292", 12: "332"}[blocks], func(t *testing.T) {
			state := continuousRuntime(t)
			state.config.Prefer292 = boolSetting(true)
			now := state.now()
			state.now = func() time.Time { return now }
			calls := 0
			state.fetch = func(_ context.Context, _ probeAuth, _ string, first *proxyEndpoint, endpoint proxyEndpoint) (string, string, string) {
				if first != nil || endpoint.URL != "socks5://proxy.invalid:1080" {
					t.Fatal("continuous acquisition escaped credential proxy")
				}
				calls++
				if calls == 1 {
					return "", "network_error", ""
				}
				if calls < 8 {
					return makeFernetToken(t, now, 13), "ok", ""
				}
				return makeFernetToken(t, now, blocks), "ok", ""
			}
			for index := 0; index < 8; index++ {
				state.refreshDue(context.Background())
				state.probeWG.Wait()
				if calls != index+1 {
					t.Fatalf("continuous retry stopped at %d, want %d", calls, index+1)
				}
				state.refreshDue(context.Background())
				state.probeWG.Wait()
				if calls != index+1 {
					t.Fatal("retry interval ignored")
				}
				now = now.Add(10 * time.Second)
			}
			key := stateKey("auth-a", "gpt-6-astra")
			if state.current[key].Blocks != blocks || state.probeCounts["auth-a"] != 8 {
				t.Fatal("target missing or fixed hourly cap still applied")
			}
			state.refreshDue(context.Background())
			state.probeWG.Wait()
			if calls != 8 {
				t.Fatal("acquisition continued despite a fresh target")
			}
			now = state.current[key].IssuedAt.Add(55 * time.Minute)
			state.refreshDue(context.Background())
			state.probeWG.Wait()
			if calls != 9 {
				t.Fatal("continuous mode did not refresh near expiry")
			}
		})
	}
}

func TestContinuousWorkerStartsWithoutBusinessRequest(t *testing.T) {
	state := continuousRuntime(t)
	started := make(chan struct{})
	ended := make(chan struct{})
	state.fetch = func(ctx context.Context, _ probeAuth, _ string, _ *proxyEndpoint, _ proxyEndpoint) (string, string, string) {
		close(started)
		<-ctx.Done()
		close(ended)
		return "", "cancelled", ""
	}
	state.mu.Lock()
	state.startBackgroundLocked()
	state.mu.Unlock()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("no automatic startup acquisition")
	}
	state.shutdown()
	select {
	case <-ended:
	default:
		t.Fatal("shutdown did not drain automatic acquisition")
	}
}

func TestContinuousKeepsRateLimitBackoff(t *testing.T) {
	state := continuousRuntime(t)
	now := state.now()
	state.now = func() time.Time { return now }
	calls := 0
	state.fetch = func(context.Context, probeAuth, string, *proxyEndpoint, proxyEndpoint) (string, string, string) {
		calls++
		return "", "upstream_http_429_rate_limit_exceeded", ""
	}
	state.refreshDue(context.Background())
	state.probeWG.Wait()
	now = now.Add(10 * time.Second)
	state.refreshDue(context.Background())
	state.probeWG.Wait()
	if calls != 1 {
		t.Fatal("continuous mode bypassed rate-limit backoff")
	}
	now = state.blockedUntil[stateKey("auth-a", "gpt-6-astra")]
	state.refreshDue(context.Background())
	state.probeWG.Wait()
	if calls != 2 {
		t.Fatal("continuous mode did not resume after backoff")
	}
}

func TestContinuousDiscoversLaterCredentialAndSkipsIneligible(t *testing.T) {
	state := continuousRuntime(t)
	base := state.hostCall
	loaded := false
	state.hostCall = func(method string, request, result any) error {
		if method == "host.auth.list" && !loaded {
			return json.Unmarshal([]byte(`{"files":[{"id":"disabled","provider":"codex","disabled":true},{"id":"key","provider":"codex","account_type":"api_key"},{"id":"other","provider":"claude"}]}`), result)
		}
		return base(method, request, result)
	}
	calls := 0
	state.fetch = func(context.Context, probeAuth, string, *proxyEndpoint, proxyEndpoint) (string, string, string) {
		calls++
		return makeFernetToken(t, state.now(), 12), "ok", ""
	}
	state.refreshDue(context.Background())
	state.probeWG.Wait()
	if calls != 0 {
		t.Fatal("ineligible credential was probed")
	}
	loaded = true
	state.refreshDue(context.Background())
	state.probeWG.Wait()
	if calls != 1 {
		t.Fatal("credential loaded after startup was not discovered")
	}
}
