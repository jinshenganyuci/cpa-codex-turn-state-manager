package integration

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRealCPACredentialDisplayAndModelReports(t *testing.T) {
	h := startHost(t)
	h.mock.mu.Lock()
	h.mock.responseModel = "gpt-5.6-luna"
	h.mock.mu.Unlock()
	for _, stream := range []bool{false, true} {
		body, _ := json.Marshal(map[string]any{"model": "ts-http", "input": "ping", "stream": stream})
		code, raw := h.call(t, "POST", "/v1/responses", body, clientKey)
		if code != 200 {
			t.Fatalf("Responses failed: %d %s", code, raw)
		}
		<-h.mock.received
	}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.url, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer " + clientKey}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ts-ws","input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(`"type":"response.completed"`)) {
			break
		}
	}
	<-h.mock.received
	await(t, func() bool {
		_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		var status struct {
			History []struct {
				Requested string `json:"requested_model"`
				Model     string `json:"model"`
				Response  string `json:"response_model"`
				Source    string `json:"response_model_source"`
			} `json:"history"`
		}
		if json.Unmarshal(raw, &status) != nil || len(status.History) != 3 {
			return false
		}
		for i, row := range status.History {
			if row.Response != "gpt-5.6-luna" {
				t.Fatalf("response model inferred or missing: %s", raw)
			}
			if i < 2 && (row.Requested != "ts-http" || row.Model != "gpt-5.6-sol") {
				t.Fatalf("request alias lost: %s", raw)
			}
			if i == 2 && (row.Requested != "ts-ws" || !strings.HasPrefix(row.Source, "upstream.")) {
				t.Fatalf("raw WebSocket evidence lost: %s", raw)
			}
		}
		return true
	})
	claims, _ := json.Marshal(map[string]any{"email": "display@example.test", "https://api.openai.com/auth": map[string]string{"chatgpt_plan_type": "plus"}})
	credential, _ := json.Marshal(map[string]any{"type": "codex", "email": "display@example.test", "access_token": "synthetic-never-use-upstream", "account_id": "display-account", "id_token": "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature", "disabled": true})
	if err := os.WriteFile(filepath.Join(h.root, "auths", "display.json"), credential, 0600); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		if bytes.Contains(raw, []byte("synthetic-never-use-upstream")) || bytes.Contains(raw, []byte(".signature")) {
			t.Fatal("credential secret leaked")
		}
		var status struct {
			Selection struct {
				Accounts []struct {
					Email string `json:"email"`
					Plan  string `json:"plan_type"`
				} `json:"accounts"`
			} `json:"selection"`
		}
		if json.Unmarshal(raw, &status) != nil {
			return false
		}
		for _, entry := range status.Selection.Accounts {
			if entry.Email == "display@example.test" && entry.Plan == "plus" {
				return true
			}
		}
		return false
	})
}
