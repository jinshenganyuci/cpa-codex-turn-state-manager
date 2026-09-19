package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"gopkg.in/yaml.v3"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func makeFernetToken(t *testing.T, issued time.Time, blocks int) string {
	t.Helper()
	raw := make([]byte, 57+16*blocks)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issued.Unix()))
	return base64.URLEncoding.EncodeToString(raw)
}
func jsonBytes(value any) []byte { raw, _ := json.Marshal(value); return raw }
func testHost(account, proxy string) func(string, any, any) error {
	return func(method string, request, result any) error {
		value := any(map[string]any{"files": []any{map[string]string{"id": "auth-a", "auth_index": "a", "provider": "codex"}, map[string]string{"id": "auth-b", "auth_index": "b", "provider": "codex"}}})
		if method == "host.auth.get" {
			value = map[string]any{"json": map[string]any{"type": "codex", "access_token": "synthetic-token", "account_id": account, "proxy_url": proxy}}
		}
		return json.Unmarshal(jsonBytes(value), result)
	}
}
func testRuntime(t *testing.T, extra string) *runtimeState {
	t.Helper()
	state := newRuntimeState()
	state.storageDir = t.TempDir()
	state.hostCall = testHost("account-a", "socks5://proxy.invalid:1080")
	now := time.Now().UTC().Truncate(time.Second)
	state.now = func() time.Time { return now }
	base := map[string]any{"enabled": true, "mode": "observe", "require_model_match": false, "prefer_292": false, "standby_enabled": false, "selection_required": false, "defaults": map[string]any{"accepted_blocks": []int{10}, "models": []string{"gpt-6-astra", "other"}}, "probe": map[string]any{"enabled": false, "continuous": false}}
	var overrides map[string]any
	if err := yaml.Unmarshal([]byte(extra), &overrides); err != nil {
		t.Fatal(err)
	}
	for key, value := range overrides {
		if key == "probe" {
			for name, setting := range value.(map[string]any) {
				base["probe"].(map[string]any)[name] = setting
			}
		} else {
			base[key] = value
		}
	}
	config, err := yaml.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.configure(jsonBytes(lifecycleRequest{ConfigYAML: config, SchemaVersion: 4})); err != nil {
		t.Fatal(err)
	}
	state.stopBackground()
	state.probeCtx, state.probeCancel = context.WithCancel(context.Background())
	t.Cleanup(state.shutdown)
	return state
}
func begin(t *testing.T, state *runtimeState, id, auth, model, incoming string) requestInterceptResponse {
	t.Helper()
	raw, err := state.interceptAfter(jsonBytes(requestInterceptRequest{RequestID: id, ToFormat: "codex", Model: model, Headers: http.Header{turnStateHeader: {incoming}}, Metadata: map[string]any{"selected_auth_id": auth}, Body: []byte(`{"reasoning":{"effort":"medium"}}`)}))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	var result requestInterceptResponse
	_ = json.Unmarshal(env.Result, &result)
	return result
}
func finish(t *testing.T, state *runtimeState, id, outcome string, completed bool) {
	t.Helper()
	if completed {
		state.observeBody(id, []byte(`{"type":"response.completed","response":{"status":"completed"}}`))
	}
	if _, err := state.complete(jsonBytes(requestCompletion{RequestID: id, Outcome: outcome})); err != nil {
		t.Fatal(err)
	}
}
func seed(state *runtimeState, token string) {
	info, _ := parseTurnState(token, 4096)
	state.current[stateKey("auth-a", "gpt-6-astra")] = storedState{Value: token, IssuedAt: info.IssuedAt, Blocks: info.Blocks, Identity: identityHash("auth-a", "account-a")}
}

func TestShapesAndTimestamp(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for blocks, length := range map[int]int{10: 292, 11: 312, 12: 332, 13: 356} {
		token := makeFernetToken(t, now, blocks)
		parsed, err := parseTurnState(token, 4096)
		if err != nil || len(token) != length || parsed.Blocks != blocks || !parsed.IssuedAt.Equal(now) {
			t.Fatal("shape mismatch")
		}
	}
	for _, value := range []string{"", "bad", strings.Repeat("a", 5000), makeFernetToken(t, time.Unix(1, 0), 10), "abc\nxyz"} {
		if _, err := parseTurnState(value, 4096); err == nil {
			t.Fatal("invalid value accepted")
		}
	}
}
func TestModesNoCacheExpiryAndIsolation(t *testing.T) {
	state := testRuntime(t, "")
	token := makeFernetToken(t, state.now(), 10)
	original := makeFernetToken(t, state.now(), 11)
	assertPass := func(result requestInterceptResponse) {
		t.Helper()
		if len(result.Headers) != 0 || len(result.ClearHeaders) != 0 {
			t.Fatal("request unexpectedly mutated")
		}
	}
	assertPass(begin(t, state, "empty", "auth-a", "gpt-6-astra", original))
	seed(state, token)
	assertPass(begin(t, state, "observe", "auth-a", "gpt-6-astra", original))
	state.config.Mode = "replace_only"
	if got := headerValue(begin(t, state, "replace", "auth-a", "gpt-6-astra", original).Headers, turnStateHeader); got != token {
		t.Fatal("replacement missing")
	}
	assertPass(begin(t, state, "short", "auth-a", "gpt-6-astra", token))
	assertPass(begin(t, state, "missing", "auth-a", "gpt-6-astra", ""))
	state.config.Mode = "force"
	if headerValue(begin(t, state, "force", "auth-a", "gpt-6-astra", "").Headers, turnStateHeader) != token {
		t.Fatal("force missing")
	}
	assertPass(begin(t, state, "otheraccount", "auth-b", "gpt-6-astra", original))
	assertPass(begin(t, state, "othermodel", "auth-a", "other", original))
	state.config.DryRun = true
	assertPass(begin(t, state, "dry", "auth-a", "gpt-6-astra", original))
	state.config.DryRun = false
	seed(state, makeFernetToken(t, state.now().Add(-time.Hour), 10))
	assertPass(begin(t, state, "expired", "auth-a", "gpt-6-astra", original))
	seed(state, token)
	state.hostCall = testHost("replaced-account", "socks5://proxy.invalid:1080")
	assertPass(begin(t, state, "replaced", "auth-a", "gpt-6-astra", original))
	if len(state.current) != 0 {
		t.Fatal("account replacement retained active state")
	}
}
func TestPromotionRequiresTerminalSuccessAndArchives(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "archive")
	state := testRuntime(t, "archive_dir: "+dir+"\nstate_file: "+filepath.Join(t.TempDir(), "states.json")+"\n")
	token := makeFernetToken(t, state.now(), 10)
	for _, tc := range []struct {
		id, outcome string
		completed   bool
		failed      bool
	}{{"http200", "succeeded", false, false}, {"failed", "failed", true, false}, {"ssefailure", "succeeded", true, true}, {"ok", "succeeded", true, false}} {
		begin(t, state, tc.id, "auth-a", "gpt-6-astra", "")
		state.captureCandidate(tc.id, http.Header{"x-codex-turn-state": {token}})
		if tc.failed {
			state.observeStreamFailure(tc.id, []byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"unknown\"}}}\n\n"))
		}
		finish(t, state, tc.id, tc.outcome, tc.completed)
		if tc.id != "ok" && len(state.current) > 0 {
			t.Fatal("failed or incomplete response promoted")
		}
	}
	if state.current[stateKey("auth-a", "gpt-6-astra")].Value != token {
		t.Fatal("successful state not promoted")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatal("successful 292 not archived")
	}
	info, _ := files[0].Info()
	if goruntime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("archive permissions")
	}
	persisted, err := loadPersisted(state.config.StateFile, 4096)
	if err != nil || len(persisted) != 1 {
		t.Fatal("persistence failed")
	}
	raw, _ := state.statusResponse()
	if strings.Contains(string(raw), token) {
		t.Fatal("status leaked token")
	}
}
func TestRetryCandidateAndWebsocketMetadata(t *testing.T) {
	state := testRuntime(t, "")
	token := makeFernetToken(t, state.now(), 10)
	begin(t, state, "retry", "auth-a", "gpt-6-astra", "")
	state.captureCandidate("retry", http.Header{turnStateHeader: {token}})
	begin(t, state, "retry", "auth-b", "gpt-6-astra", "")
	finish(t, state, "retry", "succeeded", true)
	if len(state.current) != 0 {
		t.Fatal("candidate crossed retry")
	}
	for _, header := range []any{token, []string{token}} {
		begin(t, state, "ws", "auth-a", "gpt-6-astra", "")
		payload := jsonBytes(map[string]any{"type": "response.metadata", "headers": map[string]any{"x-codex-turn-state": header}})
		_, err := state.observeWebSocket(jsonBytes(map[string]any{"RequestID": "ws", "AuthID": "auth-b", "Model": "gpt-6-astra", "Payload": payload}))
		if err != nil {
			t.Fatal(err)
		}
		if len(state.candidates) != 0 {
			t.Fatal("wrong websocket identity accepted")
		}
		_, _ = state.observeWebSocket(jsonBytes(map[string]any{"RequestID": "ws", "AuthID": "auth-a", "Model": "gpt-6-astra", "Payload": payload}))
		finish(t, state, "ws", "succeeded", true)
		if state.current[stateKey("auth-a", "gpt-6-astra")].Value != token {
			t.Fatal("WS metadata not captured")
		}
	}
}
func TestProbePinnedProxyBudgetAndFailClosed(t *testing.T) {
	state := testRuntime(t, "probe:\n  enabled: true\n  background_refresh: false\n  max_per_hour: 2\n  retry_seconds: 1\n  first_proxy:\n    url: http://must-not-use.invalid:8888\n  proxy_pool:\n    - url: http://must-not-use.invalid:9999\n")
	calls := 0
	state.fetch = func(ctx context.Context, auth probeAuth, model string, first *proxyEndpoint, second proxyEndpoint) (string, string, string) {
		calls++
		if first != nil || second.URL != "socks5://proxy.invalid:1080" || model != "gpt-6-astra" {
			t.Error("escaped credential proxy or model")
		}
		return "", "network_error", ""
	}
	state.ensureProbe("auth-a", "gpt-6-astra")
	state.lastProbe = make(map[string]time.Time)
	state.ensureProbe("auth-a", "gpt-6-astra")
	state.lastProbe = make(map[string]time.Time)
	state.ensureProbe("auth-a", "gpt-6-astra")
	if calls != 2 || state.probeResults[stateKey("auth-a", "gpt-6-astra")] != "hourly_budget_reached" {
		t.Fatal("hourly budget ignored")
	}
	state.hostCall = testHost("account-a", "")
	state.ensureProbe("auth-b", "gpt-6-astra")
	if calls != 2 || state.probeResults[stateKey("auth-b", "gpt-6-astra")] != "credential_proxy_required" {
		t.Fatal("missing proxy did not fail closed")
	}
}
func TestQuotaBackoffCancellationAndSuccess(t *testing.T) {
	state := testRuntime(t, "probe:\n  enabled: true\n  background_refresh: false\n  max_attempts: 3\n")
	calls := 0
	state.fetch = func(context.Context, probeAuth, string, *proxyEndpoint, proxyEndpoint) (string, string, string) {
		calls++
		return "", "upstream_http_429_rate_limit_exceeded", ""
	}
	state.ensureProbe("auth-a", "gpt-6-astra")
	if calls != 1 || !state.blockedUntil[stateKey("auth-a", "gpt-6-astra")].After(state.now()) {
		t.Fatal("429 did not stop retry and back off")
	}
	state.ensureProbe("auth-a", "gpt-6-astra")
	if calls != 1 {
		t.Fatal("quota retry escaped backoff")
	}
	state.fetch = func(context.Context, probeAuth, string, *proxyEndpoint, proxyEndpoint) (string, string, string) {
		return makeFernetToken(t, state.now(), 10), "ok", ""
	}
	state.ensureProbe("auth-b", "gpt-6-astra")
	if state.current[stateKey("auth-b", "gpt-6-astra")].Value == "" {
		t.Fatal("probe did not promote")
	}
}
func TestSSECompletion(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{{"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", true}, {"data: {\"type\":\"response.failed\"}\n\n", false}, {"data: {\"type\":\"response.created\"}\n\n", false}, {"data: [DONE]\n\n", false}} {
		if probeCompleted(strings.NewReader(tc.body)) != tc.want {
			t.Fatal("SSE success misclassified")
		}
	}
}

func TestSeparateSSEEventLinesAndFragmentedData(t *testing.T) {
	state := testRuntime(t, "")
	begin(t, state, "sse-lines", "auth-a", "gpt-6-astra", "")
	token := makeFernetToken(t, state.now(), 10)
	state.captureCandidate("sse-lines", http.Header{turnStateHeader: {token}})
	state.observeStreamFailure("sse-lines", []byte("event: response.created"))
	state.observeStreamFailure("sse-lines", []byte(`data: {"type":"response.created"}`))
	state.observeStreamFailure("sse-lines", []byte("event: response.completed"))
	state.observeStreamFailure("sse-lines", []byte(`data: {"type":"response.completed","response":{"status":"completed"}}`))
	finish(t, state, "sse-lines", "succeeded", false)
	if state.current[stateKey("auth-a", "gpt-6-astra")].Value != token {
		t.Fatal("separate SSE event lines hid terminal completion")
	}
	begin(t, state, "fragment", "auth-b", "gpt-6-astra", "")
	state.captureCandidate("fragment", http.Header{turnStateHeader: {token}})
	state.observeStreamFailure("fragment", []byte(`data: {"type":"response.completed","response":`))
	state.observeStreamFailure("fragment", []byte("{\"status\":\"completed\"}}\n\n"))
	finish(t, state, "fragment", "succeeded", false)
	if state.current[stateKey("auth-b", "gpt-6-astra")].Value != token {
		t.Fatal("fragmented SSE lost completion")
	}
}

func TestAccountReplacementBeforeCompletionRejectsCandidate(t *testing.T) {
	state := testRuntime(t, "")
	begin(t, state, "replaced-owner", "auth-a", "gpt-6-astra", "")
	state.captureCandidate("replaced-owner", http.Header{turnStateHeader: {makeFernetToken(t, state.now(), 10)}})
	state.hostCall = testHost("new-account", "socks5://proxy.invalid:1080")
	finish(t, state, "replaced-owner", "succeeded", true)
	if len(state.current) != 0 {
		t.Fatal("old owner's candidate survived credential replacement")
	}
}

func TestPublicRoutesCannotReadOrMutateState(t *testing.T) {
	state := testRuntime(t, "")
	for _, path := range []string{resourceBase + "/status?api=status", resourceBase + "/mode", resourceBase + "/../status", "/other/status"} {
		raw, err := state.management(managementRequest{Method: "GET", Path: path})
		if err != nil {
			t.Fatal(err)
		}
		var env envelope
		_ = json.Unmarshal(raw, &env)
		var result struct{ StatusCode int }
		_ = json.Unmarshal(env.Result, &result)
		if result.StatusCode != 404 {
			t.Fatal("public route accepted private operation")
		}
	}
	if state.config.Mode != "observe" {
		t.Fatal("public request changed mode")
	}
}
func TestNoNetworkInRequestHookAndShutdownCancelsWorker(t *testing.T) {
	state := testRuntime(t, "probe:\n  enabled: true\n  on_missing: true\n  background_refresh: false\n")
	state.now = time.Now
	started := make(chan struct{})
	ended := make(chan struct{})
	var once sync.Once
	state.fetch = func(ctx context.Context, _ probeAuth, _ string, _ *proxyEndpoint, _ proxyEndpoint) (string, string, string) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		close(ended)
		return "", "cancelled", ""
	}
	state.mu.Lock()
	state.startBackgroundLocked()
	state.mu.Unlock()
	begin(t, state, "async", "auth-a", "gpt-6-astra", "")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	state.shutdown()
	select {
	case <-ended:
	default:
		t.Fatal("worker did not drain")
	}
}
func TestIdentityLookupFailurePreservesHeaders(t *testing.T) {
	state := testRuntime(t, "mode: force\n")
	seed(state, makeFernetToken(t, state.now(), 10))
	state.hostCall = func(string, any, any) error { return errors.New("unavailable") }
	result := begin(t, state, "bad", "auth-a", "gpt-6-astra", "original")
	if len(result.Headers) > 0 || len(result.ClearHeaders) > 0 {
		t.Fatal("identity failure mutated request")
	}
}
