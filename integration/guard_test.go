package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRealCPAGuardStopsBeforeUpstreamAndResumes(t *testing.T) {
	h := startHost(t)
	patch := func(mode string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"block_without_state": true, "mode": mode})
		code, raw := h.call(t, "PATCH", "/v0/management/plugins/codex-turn-state-manager/config", body, managementKey)
		if code != 200 {
			t.Fatalf("configure guard: %d %s", code, raw)
		}
		await(t, func() bool {
			_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
			var status struct {
				Mode   string `json:"mode"`
				Active bool   `json:"blocking_active"`
			}
			return json.Unmarshal(raw, &status) == nil && status.Mode == mode && status.Active == (mode == "force")
		})
	}
	assertNoUpstream := func() {
		t.Helper()
		select {
		case <-h.mock.received:
			t.Fatal("blocked request reached upstream")
		default:
		}
	}
	patch("force")
	for _, stream := range []bool{false, true} {
		for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
			body := []byte(fmt.Sprintf(`{"model":"ts-http","input":"ping","messages":[{"role":"user","content":"ping"}],"stream":%t}`, stream))
			code, raw := h.call(t, "POST", path, body, clientKey)
			if code != 503 || !bytes.Contains(raw, []byte("valid_turn_state_required")) {
				t.Fatalf("%s stream=%v did not block: %d %s", path, stream, code, raw)
			}
			assertNoUpstream()
		}
	}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.url, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer " + clientKey}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ts-ws","input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	// This deadline belongs only to the local test client.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err == nil && !bytes.Contains(raw, []byte("valid_turn_state_required")) {
		t.Fatalf("WebSocket was not stopped: %s", raw)
	}
	if err != nil && !websocket.IsCloseError(err, websocket.CloseAbnormalClosure, websocket.ClosePolicyViolation) {
		t.Fatalf("unexpected WebSocket failure: %v", err)
	}
	assertNoUpstream()
	// This CPA closes an upgraded socket when an interceptor terminates. Prove
	// the closure is our gate, not an unrelated transport or authentication error.
	_, raw = h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
	var blockedStatus struct {
		History []struct {
			Model       string `json:"model"`
			Action      string `json:"action"`
			HostOutcome string `json:"host_outcome"`
		} `json:"history"`
	}
	if err := json.Unmarshal(raw, &blockedStatus); err != nil {
		t.Fatal(err)
	}
	wsBlocked := false
	for _, row := range blockedStatus.History {
		if row.Model == "gpt-6-astra" && row.Action == "blocked" && row.HostOutcome == "blocked_before_upstream" {
			wsBlocked = true
		}
	}
	if !wsBlocked {
		t.Fatal("missing WebSocket gate record")
	}
	_ = conn.Close()
	// Learn from the local synthetic upstream in explicit observation mode.
	// Background acquisition uses the same cache promotion path (unit tested).
	patch("observe")
	h.mock.mu.Lock()
	h.mock.state = syntheticToken(12)
	want := h.mock.state
	h.mock.mu.Unlock()
	code, raw := h.call(t, "POST", "/v1/responses", []byte(`{"model":"ts-http","input":"ping"}`), clientKey)
	if code != 200 {
		t.Fatalf("capture failed: %d %s", code, raw)
	}
	<-h.mock.received
	await(t, func() bool {
		_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		var status struct {
			Valid int `json:"valid_count"`
		}
		return json.Unmarshal(raw, &status) == nil && status.Valid > 0
	})
	patch("force")
	code, raw = h.call(t, "POST", "/v1/responses", []byte(`{"model":"ts-http","input":"ping"}`), clientKey)
	if code != 200 {
		t.Fatalf("fresh cache did not unblock: %d %s", code, raw)
	}
	if got := (<-h.mock.received).Headers.Get("X-Codex-Turn-State"); got != want {
		t.Fatal("unblocked request did not forward the cached state")
	}
}
