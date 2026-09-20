package integration

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestRealCPAProxyPoolNeverChangesBusinessRoute(t *testing.T) {
	if os.Getenv("CPA_TEST_PLUGIN") == "" || os.Getenv("CPA_TEST_BINARY") == "" {
		t.Skip("set native integration paths")
	}
	var upstream *mockUpstream
	var businessCount, poolCount, testCount atomic.Int32
	businessHeaders := make(chan string, 8)
	businessProxy, businessCA := freshInstallProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		businessCount.Add(1)
		businessHeaders <- r.Header.Get("X-Codex-Turn-State")
		upstream.server.Config.Handler.ServeHTTP(w, r)
	}))
	poolProxy, poolCA := freshInstallProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			testCount.Add(1)
			if r.Header.Get("Authorization") != "" || r.Header.Get("Chatgpt-Account-Id") != "" {
				t.Error("connection test leaked OAuth credentials")
			}
			w.WriteHeader(401)
			return
		}
		poolCount.Add(1)
		upstream.server.Config.Handler.ServeHTTP(w, r)
	}))
	bundle, _ := os.ReadFile(businessCA)
	second, _ := os.ReadFile(poolCA)
	if err := os.WriteFile(businessCA, append(bundle, second...), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CPA_TEST_CA_FILE", businessCA)
	t.Setenv("CPA_TEST_FRESH_INSTALL", "true")
	t.Setenv("CPA_TEST_NO_HEADER_MAPPING", "true")
	h := startHost(t)
	upstream = h.mock
	upstream.mu.Lock()
	upstream.responseModel = "gpt-6-astra"
	upstream.state = syntheticToken(12)
	upstream.mu.Unlock()
	auth, _ := json.Marshal(map[string]any{"type": "codex", "email": "route@example.test", "access_token": "route-synthetic-token", "account_id": "route-account", "proxy_url": businessProxy})
	authPath := filepath.Join(h.root, "auths", "routing.json")
	if err := os.WriteFile(authPath, auth, 0600); err != nil {
		t.Fatal(err)
	}
	type status struct {
		Valid     int `json:"valid_count"`
		Selection struct {
			Revision uint64 `json:"revision"`
			Accounts []struct {
				Account string `json:"account"`
			}
		} `json:"selection"`
		Pool struct {
			Enabled  bool   `json:"enabled"`
			Revision uint64 `json:"revision"`
			Entries  []struct {
				ID       string `json:"id"`
				Label    string `json:"label"`
				Attempts int    `json:"attempts"`
				Test     struct {
					HTTP int `json:"http_status"`
				} `json:"test"`
			}
		} `json:"proxy_pool"`
	}
	read := func() status {
		_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		var s status
		if json.Unmarshal(raw, &s) != nil {
			t.Fatal("status")
		}
		return s
	}
	mutate := func(action string, body map[string]any) {
		t.Helper()
		body["revision"] = read().Pool.Revision
		raw, _ := json.Marshal(body)
		code, result := h.call(t, "POST", "/v0/management/codex-turn-state-manager/proxy-pool/"+action, raw, managementKey)
		if code != 200 {
			t.Fatalf("pool %s: %d %s", action, code, result)
		}
	}
	await(t, func() bool { return len(read().Selection.Accounts) == 1 })
	mutate("save", map[string]any{"label": "Unavailable", "url": "http://127.0.0.1:1", "enabled": true})
	mutate("save", map[string]any{"label": "Acquisition only", "url": poolProxy, "enabled": true})
	s := read()
	id := s.Pool.Entries[1].ID
	code, raw := h.call(t, "POST", "/v0/management/codex-turn-state-manager/proxy-pool/test", []byte(`{"id":"`+id+`"}`), "")
	if code != 401 {
		t.Fatal("unauthenticated proxy test accepted")
	}
	code, raw = h.call(t, "POST", "/v0/management/codex-turn-state-manager/proxy-pool/test", []byte(`{"id":"`+id+`"}`), managementKey)
	if code != 202 {
		t.Fatalf("test start: %d %s", code, raw)
	}
	await(t, func() bool { return read().Pool.Entries[1].Test.HTTP == 401 })
	if testCount.Load() != 1 || poolCount.Load() != 0 || businessCount.Load() != 0 {
		t.Fatal("connectivity test invoked a model or business route")
	}
	mutate("mode", map[string]any{"enabled": true})
	s = read()
	selection, _ := json.Marshal(map[string]any{"revision": s.Selection.Revision, "accounts": []string{s.Selection.Accounts[0].Account}, "models": []string{"gpt-6-astra"}})
	code, raw = h.call(t, "POST", "/v0/management/codex-turn-state-manager/selection", selection, managementKey)
	if code != 200 {
		t.Fatalf("selection: %d %s", code, raw)
	}
	await(t, func() bool { return read().Valid == 1 })
	s = read()
	if businessCount.Load() != 0 || poolCount.Load() != 1 || s.Pool.Entries[0].Attempts != 1 || s.Pool.Entries[1].Attempts != 1 {
		t.Fatal("acquisition did not rotate exclusively inside the proxy pool")
	}
	request := func() {
		t.Helper()
		code, raw := h.call(t, "POST", "/v1/responses", []byte(`{"model":"gpt-6-astra","input":"Reply OK","stream":true}`), clientKey)
		if code != 200 {
			t.Fatalf("business: %d %s", code, raw)
		}
		if len(<-businessHeaders) != 332 {
			t.Fatal("cached state was not forwarded via credential proxy")
		}
	}
	request()
	if businessCount.Load() != 1 || poolCount.Load() != 1 {
		t.Fatal("normal API request used pool instead of credential proxy")
	}
	mutate("delete", map[string]any{"id": id})
	mutate("delete", map[string]any{"id": s.Pool.Entries[0].ID})
	request()
	if businessCount.Load() != 2 || poolCount.Load() != 1 || !read().Pool.Enabled || len(read().Pool.Entries) != 0 {
		t.Fatal("deleting pool changed business routing or failed to persist")
	}
	saved, _ := os.ReadFile(authPath)
	var savedAuth map[string]any
	_ = json.Unmarshal(saved, &savedAuth)
	if savedAuth["proxy_url"] != businessProxy || savedAuth["access_token"] != "route-synthetic-token" {
		t.Fatal("credential proxy or authentication changed")
	}
	t.Log("two distinct CONNECT proxies verified: acquisition uses pool, business uses credential, test sends no OAuth/model request, failed endpoint rotates, deletion preserves business route")
}
