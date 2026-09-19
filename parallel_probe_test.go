package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestAcquisitionStartsAllPairsWithoutQueueing(t *testing.T) {
	state := continuousRuntime(t)
	state.config.Defaults.Models = []string{"gpt-6-astra", "gpt-5.6-sol"}
	base := testHost("account-a", "socks5://unused.invalid:1080")
	state.hostCall = func(method string, request, result any) error {
		if method == "host.auth.get" {
			index := request.(map[string]string)["auth_index"]
			return json.Unmarshal(jsonBytes(map[string]any{"json": map[string]any{"type": "codex", "access_token": "synthetic-token", "account_id": "account-" + index, "proxy_url": "socks5://" + index + ".invalid:1080"}}), result)
		}
		return base(method, request, result)
	}
	started := make(chan string, 8)
	release := make(chan struct{})
	state.fetch = func(ctx context.Context, auth probeAuth, model string, first *proxyEndpoint, endpoint proxyEndpoint) (string, string, string) {
		if first != nil || endpoint.URL != auth.ProxyURL {
			t.Error("credential proxy mixed across concurrent jobs")
		}
		started <- auth.AccountID + ":" + model
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "", "cancelled", ""
	}
	state.refreshDue(context.Background())
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		select {
		case pair := <-started:
			if seen[pair] {
				t.Fatal("duplicate job")
			}
			seen[pair] = true
		case <-time.After(2 * time.Second):
			t.Fatal("one pair waited for another pair to finish")
		}
	}
	state.refreshDue(context.Background())
	if reason := state.startProbe("auth-a", "gpt-6-astra", true); reason != "probe_already_running" {
		t.Fatal("duplicate was not rejected immediately")
	}
	select {
	case <-started:
		t.Fatal("duplicate acquisition queued or started")
	default:
	}
	close(release)
	state.probeWG.Wait()
	if len(state.probing) != 0 || len(state.activeCancels) != 0 {
		t.Fatal("finished acquisition leaked running state")
	}
}

func TestManualProbeStartsImmediatelyAndBusinessDoesNotWait(t *testing.T) {
	state := priorityRuntime(t)
	started := make(chan struct{}, 1)
	state.fetch = func(ctx context.Context, _ probeAuth, _ string, _ *proxyEndpoint, _ proxyEndpoint) (string, string, string) {
		started <- struct{}{}
		<-ctx.Done()
		return "", "cancelled", ""
	}
	body := jsonBytes(map[string]string{"account": digest("auth-a")[:12], "model": "gpt-6-astra"})
	raw, err := state.management(managementRequest{Method: http.MethodPost, Path: apiBase + "/probe", Body: body})
	if responseCode(t, raw, err) != 202 {
		t.Fatal("manual acquisition did not start")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("manual acquisition waited in queue")
	}
	raw, err = state.management(managementRequest{Method: http.MethodPost, Path: apiBase + "/probe", Body: body})
	if responseCode(t, raw, err) != 409 {
		t.Fatal("duplicate did not fail fast")
	}
	done := make(chan requestInterceptResponse, 1)
	go func() { done <- begin(t, state, "business", "auth-a", "gpt-6-astra", "") }()
	select {
	case result := <-done:
		if !result.Terminate || result.StatusCode != 503 {
			t.Fatal("missing cache did not block")
		}
	case <-time.After(time.Second):
		t.Fatal("business request waited for acquisition")
	}
	raw, err = state.management(managementRequest{Method: http.MethodPost, Path: apiBase + "/cancel", Body: []byte(`{}`)})
	if responseCode(t, raw, err) != 200 {
		t.Fatal("cancel failed")
	}
	state.probeWG.Wait()
	select {
	case <-started:
		t.Fatal("a duplicate was queued")
	default:
	}
}

func TestDeselectCancelsOnlyItsOwnParallelProbe(t *testing.T) {
	state, _ := selectionRuntime(t)
	saveSelection(t, state, []string{digest("auth-a")[:12]}, []string{"gpt-6-astra", "gpt-5.6-sol"})
	state.config.Probe.Enabled = true
	started := make(chan string, 2)
	ended := make(chan string, 2)
	state.fetch = func(ctx context.Context, _ probeAuth, model string, _ *proxyEndpoint, _ proxyEndpoint) (string, string, string) {
		started <- model
		<-ctx.Done()
		ended <- model
		return "", "cancelled", ""
	}
	state.startProbe("auth-a", "gpt-6-astra", true)
	state.startProbe("auth-a", "gpt-5.6-sol", true)
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("second model did not start")
		}
	}
	saveSelection(t, state, []string{digest("auth-a")[:12]}, []string{"gpt-5.6-sol"})
	select {
	case model := <-ended:
		if model != "gpt-6-astra" {
			t.Fatal("wrong model cancelled")
		}
	case <-time.After(time.Second):
		t.Fatal("deselected job not cancelled")
	}
	state.mu.Lock()
	sol := state.activeCancels[stateKey("auth-a", "gpt-5.6-sol")]
	state.mu.Unlock()
	if sol == nil {
		t.Fatal("selected model lost cancellation handle")
	}
	select {
	case <-ended:
		t.Fatal("selected model was also cancelled")
	default:
	}
	sol()
	state.probeWG.Wait()
}
