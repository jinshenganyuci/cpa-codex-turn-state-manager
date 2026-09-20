package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"encoding/base64"
	"encoding/binary"
	"github.com/gorilla/websocket"
)

const managementKey = "turn-state-local-management-test"
const clientKey = "turn-state-local-client-test"

type capture struct {
	Body      []byte
	Websocket bool
	Headers   http.Header
}
type mockUpstream struct {
	server        *httptest.Server
	received      chan capture
	mu            sync.Mutex
	retried       bool
	state         string
	responseModel string
	httpBarrier   <-chan struct{}
}

func (m *mockUpstream) responseState() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != "" {
		return m.state
	}
	return syntheticToken(10)
}

func (m *mockUpstream) responseEvents() [][]byte {
	m.mu.Lock()
	model := m.responseModel
	m.mu.Unlock()
	events := responseEvents()
	if model != "" {
		for i, event := range events {
			events[i] = bytes.ReplaceAll(event, []byte("gpt-5.6-sol"), []byte(model))
		}
	}
	return events
}

func newMock(t *testing.T) *mockUpstream {
	t.Helper()
	m := &mockUpstream{received: make(chan capture, 256)}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cdn-cgi/trace" {
			if r.Header.Get("Authorization") != "" || r.Header.Get("ChatGPT-Account-ID") != "" {
				t.Error("egress lookup carried account credentials")
			}
			_, _ = io.WriteString(w, "h=chatgpt.com\nip=203.0.113.24\n")
			return
		}
		if websocket.IsWebSocketUpgrade(r) {
			conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			for {
				_, body, errRead := conn.ReadMessage()
				if errRead != nil {
					return
				}
				m.received <- capture{body, true, r.Header.Clone()}
				metadata, _ := json.Marshal(map[string]any{"type": "response.metadata", "headers": map[string]string{"x-codex-turn-state": m.responseState()}})
				_ = conn.WriteMessage(websocket.TextMessage, metadata)
				for _, event := range m.responseEvents() {
					if errWrite := conn.WriteMessage(websocket.TextMessage, event); errWrite != nil {
						return
					}
				}
			}
		}
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			w.WriteHeader(400)
			return
		}
		m.received <- capture{body, false, r.Header.Clone()}
		m.mu.Lock()
		barrier := m.httpBarrier
		m.mu.Unlock()
		if barrier != nil {
			select {
			case <-barrier:
			case <-r.Context().Done():
				return
			}
		}
		if bytes.Contains(body, []byte("retry-once")) {
			m.mu.Lock()
			first := !m.retried
			m.retried = true
			m.mu.Unlock()
			if first {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(502)
				_, _ = w.Write([]byte(`{"error":{"message":"local injected failure","type":"server_error"}}`))
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-local","object":"chat.completion","model":"other-model","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Turn-State", m.responseState())
		for _, event := range m.responseEvents() {
			var eventType struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(event, &eventType)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType.Type, event)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func responseEvents() [][]byte {
	response := map[string]any{"id": "resp_local", "object": "response", "status": "completed", "model": "gpt-5.6-sol", "output": []any{map[string]any{"id": "msg_local", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "OK", "annotations": []any{}}}}}, "usage": map[string]any{"input_tokens": 4, "output_tokens": 1, "total_tokens": 5}}
	events := []any{
		map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_local", "object": "response", "status": "in_progress", "output": []any{}}},
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "msg_local", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}},
		map[string]any{"type": "response.content_part.added", "output_index": 0, "content_index": 0, "item_id": "msg_local", "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
		map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": "msg_local", "delta": "OK"},
		map[string]any{"type": "response.output_text.done", "output_index": 0, "content_index": 0, "item_id": "msg_local", "text": "OK"},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": response["output"].([]any)[0]},
		map[string]any{"type": "response.completed", "response": response},
	}
	out := make([][]byte, 0, len(events))
	for _, event := range events {
		raw, _ := json.Marshal(event)
		out = append(out, raw)
	}
	return out
}

type host struct {
	url, root string
	mock      *mockUpstream
}

func startHost(t *testing.T) *host {
	t.Helper()
	binary := os.Getenv("CPA_TEST_BINARY")
	library := os.Getenv("CPA_TEST_PLUGIN")
	if binary == "" || library == "" {
		t.Skip("set CPA_TEST_BINARY and CPA_TEST_PLUGIN for real CPA integration")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	library, err = filepath.Abs(library)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	pluginDir := filepath.Join(root, "plugins")
	if err := os.MkdirAll(pluginDir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(library)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "codex-turn-state-manager.so"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CPA_TEST_STORE_INSTALL") == "true" {
		if err := os.Remove(filepath.Join(pluginDir, "codex-turn-state-manager.so")); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	m := newMock(t)
	config := fmt.Sprintf(`host: "127.0.0.1"
port: %d
auth-dir: "%s/auths"
remote-management:
  allow-remote: false
  secret-key: "%s"
  disable-control-panel: true
  disable-auto-update-panel: true
api-keys: ["%s"]
request-retry: 1
max-retry-interval: 1
disable-image-generation: true
plugins:
  enabled: true
  dir: "%s"
  configs:
    codex-turn-state-manager:
      enabled: true
      priority: 1
      mode: observe
      selection_required: false
      require_model_match: false
      prefer_292: false
      standby_enabled: false
      state_file: "%s/current.json"
      archive_dir: "%s/archive"
      defaults:
        accepted_blocks: [10, 12]
        models: [gpt-5.6-sol, gpt-6-astra]
      probe:
        enabled: false
codex-api-key:
  - api-key: "local-http-a"
    base-url: "%s"
    priority: 2
    headers: {X-Codex-Turn-State: "$X-Codex-Turn-State"}
    models: [{name: "gpt-5.6-sol", alias: "ts-http"}]
  - api-key: "local-http-b"
    base-url: "%s"
    headers: {X-Codex-Turn-State: "$X-Codex-Turn-State"}
    models: [{name: "gpt-5.6-sol", alias: "ts-http"}]
  - api-key: "local-ws"
    base-url: "%s"
    websockets: true
    models: [{name: "gpt-6-astra", alias: "ts-ws"}]
openai-compatibility:
  - name: "local-other"
    base-url: "%s/v1"
    api-key-entries: [{api-key: "local-other-key"}]
    models: [{name: "other-model", alias: "ts-other"}]
`, port, root, managementKey, clientKey, pluginDir, root, root, m.server.URL, m.server.URL, m.server.URL, m.server.URL)
	if os.Getenv("CPA_TEST_FRESH_INSTALL") == "true" {
		start := strings.Index(config, "      enabled: true\n")
		end := strings.Index(config[start:], "codex-api-key:") + start
		config = config[:start] + "      enabled: true\n" + config[end:]
	}
	if os.Getenv("CPA_TEST_LEGACY_PLUGIN") == "true" {
		config = strings.ReplaceAll(config, "      selection_required: false\n", "")
		for _, flag := range []string{"require_model_match", "prefer_292", "standby_enabled"} {
			config = strings.ReplaceAll(config, "      "+flag+": false\n", "")
		}
	}
	if os.Getenv("CPA_TEST_NO_HEADER_MAPPING") == "true" {
		config = strings.ReplaceAll(config, "    headers: {X-Codex-Turn-State: \"$X-Codex-Turn-State\"}\n", "")
	}
	if registry := os.Getenv("CPA_TEST_REGISTRY_URL"); registry != "" {
		config = strings.Replace(config, "  configs:\n", "  store-sources: [\""+registry+"\"]\n  configs:\n", 1)
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(root, "cpa.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--config", configPath, "--local-model")
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TZ=UTC"}
	if cert := os.Getenv("CPA_TEST_CA_FILE"); cert != "" {
		cmd.Env = append(cmd.Env, "SSL_CERT_FILE="+cert)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		if errClose := logFile.Close(); errClose != nil {
			t.Errorf("close CPA log: %v", errClose)
		}
		if t.Failed() {
			raw, _ := os.ReadFile(filepath.Join(root, "cpa.log"))
			if len(raw) > 6000 {
				raw = append(append(raw[:4500:4500], []byte("\n... log truncated ...\n")...), raw[len(raw)-1500:]...)
			}
			t.Logf("CPA test log:\n%s", raw)
		}
	})
	h := &host{url: fmt.Sprintf("http://127.0.0.1:%d", port), root: root, mock: m}
	await(t, func() bool {
		select {
		case err := <-done:
			done <- err
			t.Fatalf("CPA exited before readiness: %v", err)
		default:
		}
		response, err := http.Get(h.url + "/")
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == 200
	})
	if os.Getenv("CPA_TEST_STORE_INSTALL") != "true" {
		await(t, func() bool {
			status, _ := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
			return status == 200
		})
	}
	// The listener and management routes become ready before the initial client
	// load and watcher setup finish. Do not race that configuration lifecycle.
	await(t, func() bool {
		status, raw := h.call(t, "GET", "/v1/models", nil, clientKey)
		if status != 200 || !bytes.Contains(raw, []byte(`"ts-http"`)) || !bytes.Contains(raw, []byte(`"ts-ws"`)) {
			return false
		}
		log, err := os.ReadFile(filepath.Join(root, "cpa.log"))
		return err == nil && bytes.Contains(log, []byte("file watcher started for config and auth directory changes"))
	})
	return h
}

func await(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for local integration condition")
		case <-tick.C:
		}
	}
}

func (h *host) call(t *testing.T, method, path string, body []byte, key string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, h.url+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close response: %v", err)
		}
	}()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func syntheticToken(blocks int) string {
	raw := make([]byte, 57+16*blocks)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(time.Now().Unix()))
	return base64.URLEncoding.EncodeToString(raw)
}
func TestRealCPAPluginLifecycle(t *testing.T) {
	h := startHost(t)
	t.Cleanup(func() {
		if t.Failed() {
			_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
			t.Logf("plugin status after failure: %s", raw)
		}
	})
	for _, path := range []string{"/v0/management/codex-turn-state-manager/status", "/v0/management/codex-turn-state-manager/mode"} {
		status, _ := h.call(t, "GET", path, nil, "")
		if status == 200 {
			t.Fatal("unauthenticated management access")
		}
	}
	status, html := h.call(t, "GET", "/v0/resource/plugins/codex-turn-state-manager/status", nil, "")
	if status != 200 || !bytes.Contains(html, []byte("回合状态")) {
		t.Fatal("dashboard unavailable")
	}
	original := syntheticToken(11)
	send := func(stream bool) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"model": "ts-http", "input": "Reply OK", "reasoning": map[string]string{"effort": "medium"}, "stream": stream})
		req, _ := http.NewRequest("POST", h.url+"/v1/responses", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+clientKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Codex-Turn-State", original)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request failed %d %s", resp.StatusCode, raw)
		}
	}
	send(true)
	first := <-h.mock.received
	if first.Headers.Get("X-Codex-Turn-State") != original {
		t.Fatal("observe mode changed original header")
	}
	await(t, func() bool {
		_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		var data struct {
			Valid int `json:"valid_count"`
		}
		_ = json.Unmarshal(raw, &data)
		return data.Valid > 0
	})
	status, _ = h.call(t, "POST", "/v0/management/codex-turn-state-manager/mode", []byte(`{"mode":"replace_only"}`), managementKey)
	if status != 200 {
		t.Fatal("mode update failed")
	}
	send(false)
	second := <-h.mock.received
	if len(second.Headers.Get("X-Codex-Turn-State")) != 292 {
		t.Fatalf("upstream replacement not observed, length=%d", len(second.Headers.Get("X-Codex-Turn-State")))
	}
	// A downstream WebSocket drives the upstream WebSocket executor and raw observer.
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.url, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer " + clientKey}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ts-ws","input":[{"role":"user","content":[{"type":"input_text","text":"Reply OK"}]}],"reasoning":{"effort":"medium"}}`)); err != nil {
		t.Fatal(err)
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var event struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &event)
		if event.Type == "error" {
			t.Fatalf("websocket failed %s", raw)
		}
		if event.Type == "response.completed" {
			break
		}
	}
	captured := <-h.mock.received
	if !captured.Websocket {
		t.Fatal("upstream was not websocket")
	}
	_ = conn.Close()
	await(t, func() bool {
		_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		var data struct {
			Entries []struct {
				Model  string `json:"model"`
				Length int    `json:"state_length"`
			} `json:"entries"`
		}
		_ = json.Unmarshal(raw, &data)
		for _, entry := range data.Entries {
			if entry.Model == "gpt-6-astra" && entry.Length == 292 {
				return true
			}
		}
		return false
	})
}

func TestRealCPADualLengthCaptureArchiveAndReplay(t *testing.T) {
	h := startHost(t)
	status, _ := h.call(t, "POST", "/v0/management/codex-turn-state-manager/mode", []byte(`{"mode":"force"}`), managementKey)
	if status != 200 {
		t.Fatal("force mode update failed")
	}
	// One timestamp exercises the equal-second handover without timer sleeps.
	issued := uint64(time.Now().Unix())
	token := func(blocks int, marker byte) string {
		raw := make([]byte, 57+16*blocks)
		raw[0] = 0x80
		binary.BigEndian.PutUint64(raw[1:9], issued)
		raw[25] = marker
		return base64.URLEncoding.EncodeToString(raw)
	}
	values := []string{token(12, 1), token(10, 2), token(12, 3)}
	previous := ""
	for index, next := range values {
		h.mock.mu.Lock()
		h.mock.state = next
		h.mock.mu.Unlock()
		body, _ := json.Marshal(map[string]any{"model": "ts-http", "input": "Reply OK", "stream": index%2 == 0, "reasoning": map[string]string{"effort": "medium"}})
		code, raw := h.call(t, "POST", "/v1/responses", body, clientKey)
		if code != 200 {
			t.Fatalf("local upstream request failed: %d %s", code, raw)
		}
		captured := <-h.mock.received
		if got := captured.Headers.Get("X-Codex-Turn-State"); got != previous {
			t.Fatalf("request %d did not forward the preceding full state, length %d", index+1, len(got))
		}
		await(t, func() bool {
			raw, err := os.ReadFile(filepath.Join(h.root, "current.json"))
			if err != nil {
				return false
			}
			var saved struct {
				Credentials map[string]struct {
					Value string `json:"state"`
				} `json:"credentials"`
			}
			if json.Unmarshal(raw, &saved) != nil {
				return false
			}
			for _, v := range saved.Credentials {
				if v.Value == next {
					return true
				}
			}
			return false
		})
		previous = next
	}
	archives, err := filepath.Glob(filepath.Join(h.root, "archive", "*.json"))
	if err != nil || len(archives) != 3 {
		t.Fatalf("expected 3 complete archives, got %d", len(archives))
	}
	found := map[string]bool{}
	for _, path := range archives {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var saved struct {
			State struct {
				Value string `json:"state"`
			} `json:"state"`
			Length int `json:"length"`
		}
		if json.Unmarshal(raw, &saved) != nil || len(saved.State.Value) != saved.Length {
			t.Fatal("archive value/length mismatch")
		}
		found[saved.State.Value] = true
	}
	for _, v := range values {
		if !found[v] {
			t.Fatalf("missing complete %d-character archive", len(v))
		}
	}
	_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
	for _, v := range values {
		if bytes.Contains(raw, []byte(v)) {
			t.Fatal("dashboard API leaked a state")
		}
	}
	_, html := h.call(t, "GET", "/v0/resource/plugins/codex-turn-state-manager/status", nil, "")
	if !bytes.Contains(html, []byte("最近成功的 332")) || !bytes.Contains(html, []byte("预计刷新")) {
		t.Fatal("dual-state dashboard is missing")
	}
}

func TestRealCPASelectionManagementPersists(t *testing.T) {
	h := startHost(t)
	auth := []byte(`{"type":"codex","access_token":"selection-synthetic-token","account_id":"selection-synthetic-account","email":"selection@example.invalid","proxy_url":"socks5://127.0.0.1:1"}`)
	if err := os.WriteFile(filepath.Join(h.root, "auths", "selection.json"), auth, 0600); err != nil {
		t.Fatal(err)
	}
	patch, _ := json.Marshal(map[string]any{"selection_required": true, "selection_file": filepath.Join(h.root, "selection.json")})
	code, raw := h.call(t, "PATCH", "/v0/management/plugins/codex-turn-state-manager/config", patch, managementKey)
	if code != 200 {
		t.Fatalf("selection configuration failed: %d %s", code, raw)
	}
	type selectionView struct {
		Required bool     `json:"required"`
		Revision uint64   `json:"revision"`
		Models   []string `json:"models"`
		Accounts []struct {
			Account  string `json:"account"`
			Selected bool   `json:"selected"`
		} `json:"accounts"`
	}
	var state struct {
		Selection selectionView `json:"selection"`
	}
	read := func() {
		_, body := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		if bytes.Contains(body, []byte("selection-synthetic-token")) || bytes.Contains(body, []byte("selection-synthetic-account")) {
			t.Fatal("selection status exposed credential content")
		}
		if json.Unmarshal(body, &state) != nil {
			t.Fatal("invalid selection status")
		}
	}
	await(t, func() bool { read(); return state.Selection.Required && len(state.Selection.Accounts) == 1 })
	account := state.Selection.Accounts[0].Account
	body, _ := json.Marshal(map[string]any{"revision": state.Selection.Revision, "accounts": []string{account}, "models": []string{"gpt-6-astra", "gpt-5.6-sol"}})
	code, _ = h.call(t, "POST", "/v0/management/codex-turn-state-manager/selection", body, "")
	if code != 401 {
		t.Fatal("selection save accepted missing authentication")
	}
	code, raw = h.call(t, "POST", "/v0/management/codex-turn-state-manager/selection", body, managementKey)
	if code != 200 {
		t.Fatalf("selection save failed: %d %s", code, raw)
	}
	read()
	if len(state.Selection.Models) != 2 || !state.Selection.Accounts[0].Selected {
		t.Fatal("saved selections missing")
	}
	info, err := os.Stat(filepath.Join(h.root, "selection.json"))
	if err != nil || goruntime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("selection was not saved privately")
	}
	code, _ = h.call(t, "PATCH", "/v0/management/plugins/codex-turn-state-manager/config", []byte(`{"dry_run":true}`), managementKey)
	if code != 200 {
		t.Fatal("reconfigure failed")
	}
	await(t, func() bool { read(); return state.Selection.Revision == 1 && len(state.Selection.Models) == 2 })
	code, _ = h.call(t, "POST", "/v0/management/codex-turn-state-manager/selection", body, managementKey)
	if code != 409 {
		t.Fatal("stale save was not rejected")
	}
	clear, _ := json.Marshal(map[string]any{"revision": state.Selection.Revision, "accounts": []string{}, "models": []string{}})
	code, _ = h.call(t, "POST", "/v0/management/codex-turn-state-manager/selection", clear, managementKey)
	if code != 200 {
		t.Fatal("uncheck all failed")
	}
	read()
	if len(state.Selection.Models) != 0 || state.Selection.Accounts[0].Selected {
		t.Fatal("uncheck all did not persist")
	}
}

func TestRealCPANativeHotUpgradeSelection(t *testing.T) {
	old := os.Getenv("CPA_TEST_OLD_PLUGIN")
	if old == "" {
		t.Skip("set CPA_TEST_OLD_PLUGIN for native hot-upgrade test")
	}
	current := os.Getenv("CPA_TEST_PLUGIN")
	t.Setenv("CPA_TEST_PLUGIN", old)
	t.Setenv("CPA_TEST_LEGACY_PLUGIN", "true")
	h := startHost(t)
	data, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.root, "plugins", "codex-turn-state-manager-v0.3.2.so"), data, 0600); err != nil {
		t.Fatal(err)
	}
	patch, _ := json.Marshal(map[string]any{"selection_required": true, "selection_file": filepath.Join(h.root, "selection.json")})
	code, body := h.call(t, "PATCH", "/v0/management/plugins/codex-turn-state-manager/config", patch, managementKey)
	if code != 200 {
		t.Fatalf("hot upgrade failed: %d %s", code, body)
	}
	await(t, func() bool {
		code, body := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		var state struct {
			Version   string `json:"version"`
			Selection struct {
				Required bool `json:"required"`
			} `json:"selection"`
		}
		return code == 200 && json.Unmarshal(body, &state) == nil && state.Version == "0.3.2" && state.Selection.Required
	})
}
