package integration

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRealCPA292PriorityBusinessStandbyAndModelGate(t *testing.T) {
	h := startHost(t)
	t.Cleanup(func() {
		if t.Failed() {
			_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
			t.Logf("priority status: %s", raw)
		}
	})
	patch := func(fields map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(fields)
		code, body := h.call(t, "PATCH", "/v0/management/plugins/codex-turn-state-manager/config", raw, managementKey)
		if code != 200 {
			t.Fatalf("configure: %d %s", code, body)
		}
		await(t, func() bool {
			_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
			var status map[string]any
			return json.Unmarshal(raw, &status) == nil && status["require_model_match"] == true && status["mode"] == fields["mode"]
		})
	}
	patch(map[string]any{"require_model_match": true, "prefer_292": true, "standby_enabled": true, "mode": "observe", "block_without_state": true})
	token := func(blocks int, marker byte) string {
		raw := make([]byte, 57+16*blocks)
		raw[0] = 0x80
		binary.BigEndian.PutUint64(raw[1:9], uint64(time.Now().Unix()))
		raw[25] = marker
		return base64.URLEncoding.EncodeToString(raw)
	}
	type slot struct {
		Value  string `json:"state"`
		Source string `json:"source"`
		Model  string `json:"response_model"`
	}
	type bundle struct {
		Active  map[string]slot `json:"credentials"`
		Standby map[string]slot `json:"standby"`
	}
	var saved bundle
	waitSlots := func(active, backup string) {
		t.Helper()
		await(t, func() bool {
			raw, err := os.ReadFile(filepath.Join(h.root, "current.json"))
			saved = bundle{}
			if err != nil || json.Unmarshal(raw, &saved) != nil {
				return false
			}
			if len(saved.Active) != 1 {
				return false
			}
			for key, value := range saved.Active {
				return value.Value == active && saved.Standby[key].Value == backup
			}
			return false
		})
	}
	request := func(value, model, wantHeader string) {
		t.Helper()
		h.mock.mu.Lock()
		h.mock.state = value
		h.mock.responseModel = model
		h.mock.mu.Unlock()
		code, raw := h.call(t, "POST", "/v1/responses", []byte(`{"model":"ts-http","input":"Reply OK","stream":true}`), clientKey)
		if code != 200 {
			t.Fatalf("business response: %d %s", code, raw)
		}
		if got := (<-h.mock.received).Headers.Get("X-Codex-Turn-State"); got != wantHeader {
			t.Fatal("wrong state reached upstream")
		}
	}
	fallback, preferred, backup, laterFallback := token(12, 1), token(10, 2), token(10, 3), token(12, 4)
	request(fallback, "gpt-5.6-sol", "")
	waitSlots(fallback, "")
	patch(map[string]any{"mode": "force"})
	request(preferred, "gpt-5.6-sol", fallback)
	waitSlots(preferred, fallback)
	request(backup, "gpt-5.6-sol", preferred)
	waitSlots(preferred, backup)
	request(laterFallback, "gpt-5.6-sol", preferred)
	waitSlots(preferred, backup)
	request(backup, "gpt-5.6-sol", preferred)
	waitSlots(preferred, backup)
	for _, value := range saved.Standby {
		if value.Source != "business" || value.Model != "gpt-5.6-sol" {
			t.Fatal("missing provenance")
		}
	}
	await(t, func() bool {
		raw, err := os.ReadFile(filepath.Join(h.root, "current.json.runtime.json"))
		if err != nil {
			return false
		}
		var journal struct {
			History []struct {
				Source string `json:"state_source"`
				Action string `json:"cache_action"`
			} `json:"history"`
		}
		if json.Unmarshal(raw, &journal) != nil {
			return false
		}
		found := map[string]bool{}
		for _, row := range journal.History {
			if row.Source == "business" {
				found[row.Action] = true
			}
		}
		return found["upgraded_292"] && found["standby"] && found["duplicate"]
	})
	// A mismatch invalidates only the value actually used; the verified standby takes over.
	request(token(10, 5), "gpt-5.6-luna", preferred)
	waitSlots(backup, "")
	request(token(10, 6), "gpt-5.6-luna", backup)
	await(t, func() bool {
		_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		var status struct {
			Valid   int `json:"valid_count"`
			History []struct {
				Acceptance string `json:"acceptance"`
			} `json:"history"`
		}
		if json.Unmarshal(raw, &status) != nil || status.Valid != 0 {
			return false
		}
		n := 0
		for _, row := range status.History {
			if row.Acceptance == "model_mismatch" {
				n++
			}
		}
		return n == 2
	})
	code, _ := h.call(t, "POST", "/v1/responses", []byte(`{"model":"ts-http","input":"Reply OK"}`), clientKey)
	if code != 503 {
		t.Fatalf("empty verified cache did not block: %d", code)
	}
	select {
	case <-h.mock.received:
		t.Fatal("blocked request reached upstream")
	default:
	}
}
