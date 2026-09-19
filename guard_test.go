package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func guardedRuntime(t *testing.T) *runtimeState {
	t.Helper()
	state := testRuntime(t, "mode: force\nblock_without_state: true\nprobe:\n  enabled: true\n  background_refresh: false\n  retry_seconds: 5\n")
	state.config.Defaults.AcceptedBlocks = []int{10, 12}
	state.config.SelectionRequired = true
	state.selection.Accounts["auth-a"] = identityHash("auth-a", "account-a")
	state.selection.Models = []string{"gpt-6-astra"}
	return state
}

func TestGuardRejectsUnusableState(t *testing.T) {
	for _, name := range []string{"missing", "expired", "future", "312", "356", "malformed", "identity", "timestamp", "policy", "host_unavailable", "account_replaced"} {
		t.Run(name, func(t *testing.T) {
			state := guardedRuntime(t)
			seed(state, makeFernetToken(t, state.now(), 12))
			key := stateKey("auth-a", "gpt-6-astra")
			current := state.current[key]
			switch name {
			case "missing":
				delete(state.current, key)
			case "expired":
				seed(state, makeFernetToken(t, state.now().Add(-time.Hour), 12))
			case "future":
				seed(state, makeFernetToken(t, state.now().Add(6*time.Minute), 12))
			case "312":
				seed(state, makeFernetToken(t, state.now(), 11))
			case "356":
				seed(state, makeFernetToken(t, state.now(), 13))
			case "malformed":
				current.Value = strings.Repeat("x", 332)
				state.current[key] = current
			case "identity":
				current.Identity = identityHash("auth-b", "account-b")
				state.current[key] = current
			case "timestamp":
				current.IssuedAt = current.IssuedAt.Add(time.Second)
				state.current[key] = current
			case "policy":
				state.config.Defaults.AcceptedBlocks = []int{10}
			case "host_unavailable":
				state.hostCall = func(string, any, any) error { return errors.New("unavailable") }
			case "account_replaced":
				state.hostCall = testHost("another-account", "socks5://proxy.invalid:1080")
			}
			// A client-supplied value cannot substitute for a trusted local cache.
			got := begin(t, state, "blocked", "auth-a", "gpt-6-astra", makeFernetToken(t, state.now(), 10))
			if !got.Terminate || got.StatusCode != http.StatusServiceUnavailable || got.ResponseHeaders.Get("Retry-After") != "5" {
				t.Fatalf("request was not terminated: %+v", got)
			}
			if len(got.Headers) != 0 || !strings.Contains(string(got.ResponseBody), `"code":`) || strings.Contains(string(got.ResponseBody), "auth-a") {
				t.Fatal("unsafe termination payload")
			}
			if len(state.requests) != 0 || len(state.candidates) != 0 || len(state.history) != 1 || state.history[0].Action != "blocked" || state.refreshRequests[key] != "missing_state" {
				t.Fatal("blocked request did not clean bindings, record the decision and queue acquisition")
			}
			finish(t, state, "blocked", "failed", false)
			if len(state.history) != 1 {
				t.Fatal("host completion double counted the block")
			}
		})
	}
}

func TestGuardAllowsSelectedFresh292And332(t *testing.T) {
	for _, blocks := range []int{10, 12} {
		state := guardedRuntime(t)
		token := makeFernetToken(t, state.now().Add(-55*time.Minute), blocks)
		seed(state, token)
		got := begin(t, state, "fresh", "auth-a", "gpt-6-astra", "")
		if got.Terminate || headerValue(got.Headers, turnStateHeader) != token {
			t.Fatal("unexpired cache was not replayed during refresh window")
		}
	}
}

func TestGuardPreservesUnselectedAndObservationModes(t *testing.T) {
	for _, name := range []string{"unselected_model", "unselected_credential", "observe", "replace_only", "dry_run", "disabled"} {
		t.Run(name, func(t *testing.T) {
			state := guardedRuntime(t)
			switch name {
			case "unselected_model":
				state.selection.Models = nil
			case "unselected_credential":
				state.selection.Accounts = nil
			case "observe", "replace_only":
				state.config.Mode = name
			case "dry_run":
				state.config.DryRun = true
			case "disabled":
				state.config.BlockWithoutState = false
			}
			if got := begin(t, state, name, "auth-a", "gpt-6-astra", ""); got.Terminate {
				t.Fatal("guard affected an excluded request")
			}
		})
	}
}

func TestGuardAcquisitionUnblocksThenExpiryBlocksAgain(t *testing.T) {
	state := guardedRuntime(t)
	if !begin(t, state, "before", "auth-a", "gpt-6-astra", "").Terminate {
		t.Fatal("missing cache allowed business traffic")
	}
	token := makeFernetToken(t, state.now(), 12)
	state.fetch = func(_ context.Context, _ probeAuth, _ string, first *proxyEndpoint, endpoint proxyEndpoint) (string, string, string) {
		if first != nil || endpoint.URL != "socks5://proxy.invalid:1080" {
			t.Error("probe escaped credential proxy")
		}
		return token, "ok", ""
	}
	state.ensureProbe("auth-a", "gpt-6-astra")
	if got := begin(t, state, "after", "auth-a", "gpt-6-astra", ""); got.Terminate || headerValue(got.Headers, turnStateHeader) != token {
		t.Fatal("successful acquisition did not automatically release the gate")
	}
	now := state.now().Add(time.Hour)
	state.now = func() time.Time { return now }
	if !begin(t, state, "expired", "auth-a", "gpt-6-astra", "").Terminate {
		t.Fatal("expired cache allowed business traffic")
	}
}
