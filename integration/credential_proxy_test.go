package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestRealCPARetiredProxyPoolCannotOverrideCredentialMode(t *testing.T) {
	h := startHost(t)
	patch := []byte(`{"probe":{"enabled":false,"proxy_mode":"pool","allow_proxy_override":true,"first_proxy":{"url_file":"/does-not-exist/old-first.txt"},"credential_pools":{"old.json":[{"url_file":"/does-not-exist/old-pool.txt"}]},"proxy_pool":[{"url":"socks5://retired.invalid:1080"}],"failure_threshold":1,"cooldown_seconds":3600}}`)
	code, raw := h.call(t, "PATCH", "/v0/management/plugins/codex-turn-state-manager/config", patch, managementKey)
	if code != 200 {
		t.Fatalf("old configuration migration failed: %d %s", code, raw)
	}
	await(t, func() bool {
		code, raw = h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		var status map[string]any
		return code == 200 && json.Unmarshal(raw, &status) == nil && status["version"] == "0.3.2" && status["proxy_mode"] == "credential" && status["probe_parallel"] == true && status["proxies"] == nil
	})
	code, _ = h.call(t, "POST", "/v0/management/codex-turn-state-manager/proxy", []byte(`{"action":"enable","id":"old"}`), managementKey)
	if code != 404 {
		t.Fatalf("retired proxy endpoint still active: %d", code)
	}
	code, raw = h.call(t, "GET", "/v0/resource/plugins/codex-turn-state-manager/status", nil, "")
	if code != 200 || bytes.Contains(raw, []byte("代理池健康")) || !bytes.Contains(raw, []byte("proxyRows")) || !bytes.Contains(raw, []byte("正常 API 请求始终沿用凭据自己的代理")) {
		t.Fatal("probe-only pool UI not deployed")
	}
}

func TestRealCPABusinessRequestsDoNotQueueBehindEachOther(t *testing.T) {
	h := startHost(t)
	requestBody := []byte(`{"model":"ts-http","input":"Reply OK","stream":true}`)
	code, raw := h.call(t, "POST", "/v1/responses", requestBody, clientKey)
	if code != 200 {
		t.Fatalf("seed: %d %s", code, raw)
	}
	<-h.mock.received
	await(t, func() bool {
		_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		var status struct {
			Valid int `json:"valid_count"`
		}
		return json.Unmarshal(raw, &status) == nil && status.Valid > 0
	})
	code, _ = h.call(t, "PATCH", "/v0/management/plugins/codex-turn-state-manager/config", []byte(`{"mode":"force","block_without_state":true}`), managementKey)
	if code != 200 {
		t.Fatal("guard setup failed")
	}
	await(t, func() bool {
		_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		var status struct {
			Guard bool `json:"blocking_active"`
		}
		return json.Unmarshal(raw, &status) == nil && status.Guard
	})
	release := make(chan struct{})
	defer func() {
		if release != nil {
			close(release)
		}
	}()
	h.mock.mu.Lock()
	h.mock.httpBarrier = release
	h.mock.mu.Unlock()
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { code, _ := h.call(t, "POST", "/v1/responses", requestBody, clientKey); codes <- code }()
	}
	for i := 0; i < 2; i++ {
		select {
		case request := <-h.mock.received:
			if len(request.Headers.Get("X-Codex-Turn-State")) != 292 {
				t.Fatal("parallel request omitted valid state")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("second request waited for the first response")
		}
	}
	close(release)
	release = nil
	for i := 0; i < 2; i++ {
		select {
		case code := <-codes:
			if code != http.StatusOK {
				t.Fatalf("parallel request failed: %d", code)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("response did not complete")
		}
	}
}
