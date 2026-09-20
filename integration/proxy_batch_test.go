package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRealCPAConcurrentProxyBatches(t *testing.T) {
	if os.Getenv("CPA_TEST_PLUGIN") == "" || os.Getenv("CPA_TEST_BINARY") == "" {
		t.Skip("set native integration paths")
	}
	started := make(chan int, 12)
	gates := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	var closes [2]sync.Once
	// Release blocked handlers before httptest cleanup even if an assertion fails.
	defer func() {
		for i := range gates {
			closes[i].Do(func() { close(gates[i]) })
		}
	}()
	var calls atomic.Int32
	var proxies []string
	var bundle []byte
	for i := range 4 {
		proxy, ca := freshInstallProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/cdn-cgi/trace" {
				_, _ = fmt.Fprintf(w, "ip=203.0.113.%d\n", i+1)
				return
			}
			n := calls.Add(1)
			started <- i
			wave := min(int(n-1)/3, 1)
			select {
			case <-gates[wave]:
			case <-r.Context().Done():
				return
			}
			blocks := 13
			if wave == 1 {
				blocks = 12
			}
			w.Header().Set("X-Codex-Turn-State", syntheticToken(blocks))
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-6-astra\"}}\n\n")
		}))
		proxies = append(proxies, proxy)
		pem, err := os.ReadFile(ca)
		if err != nil {
			t.Fatal(err)
		}
		bundle = append(bundle, pem...)
	}
	ca := filepath.Join(t.TempDir(), "pool-ca.pem")
	if err := os.WriteFile(ca, bundle, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CPA_TEST_CA_FILE", ca)
	t.Setenv("CPA_TEST_FRESH_INSTALL", "true")
	t.Setenv("CPA_TEST_NO_HEADER_MAPPING", "true")
	h := startHost(t)
	auth, _ := json.Marshal(map[string]any{"type": "codex", "email": "batch@example.test", "access_token": "synthetic-batch-token", "account_id": "batch-account", "proxy_url": "http://127.0.0.1:1"})
	if err := os.WriteFile(filepath.Join(h.root, "auths", "batch.json"), auth, 0600); err != nil {
		t.Fatal(err)
	}
	type status struct {
		Valid       int `json:"valid_count"`
		Concurrency int `json:"proxy_concurrency"`
		Selection   struct {
			Revision uint64 `json:"revision"`
			Accounts []struct {
				Account string `json:"account"`
			} `json:"accounts"`
		} `json:"selection"`
		Pool struct {
			Revision uint64 `json:"revision"`
		} `json:"proxy_pool"`
		History []struct {
			Acceptance string `json:"acceptance"`
		} `json:"history"`
	}
	read := func() status {
		_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		var s status
		if json.Unmarshal(raw, &s) != nil {
			t.Fatal("invalid status")
		}
		return s
	}
	post := func(path string, body any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		code, result := h.call(t, "POST", "/v0/management/codex-turn-state-manager/"+path, raw, managementKey)
		if code != 200 {
			t.Fatalf("%s: %d %s", path, code, result)
		}
	}
	await(t, func() bool { return len(read().Selection.Accounts) == 1 })
	if read().Concurrency != 3 {
		t.Fatal("wrong default concurrency")
	}
	for i, proxy := range proxies {
		post("proxy-pool/save", map[string]any{"revision": read().Pool.Revision, "label": fmt.Sprintf("Pool %d", i), "url": proxy, "enabled": true, "rotating": true})
	}
	post("proxy-pool/mode", map[string]any{"revision": read().Pool.Revision, "enabled": true})
	s := read()
	post("selection", map[string]any{"revision": s.Selection.Revision, "accounts": []string{s.Selection.Accounts[0].Account}, "models": []string{"gpt-6-astra"}})
	previous := make(map[int]bool)
	for wave := range 2 {
		seen := make(map[int]bool)
		for range 3 {
			select {
			case id := <-started:
				if seen[id] {
					t.Fatal("duplicate concurrent proxy")
				}
				seen[id] = true
			case <-time.After(12 * time.Second):
				t.Fatal("three network requests were not concurrent")
			}
		}
		if calls.Load() != int32((wave+1)*3) {
			t.Fatal("credential cap exceeded")
		}
		if wave == 1 {
			for i := range 4 {
				if !previous[i] && !seen[i] {
					t.Fatal("next wave skipped omitted proxy")
				}
			}
		}
		previous = seen
		closes[wave].Do(func() { close(gates[wave]) })
		await(t, func() bool { return len(read().History) == (wave+1)*3 })
	}
	if read().Valid != 1 || calls.Load() != 6 {
		t.Fatal("second wave did not cache 332")
	}
	t.Log("real CPA: four CONNECT proxies, exactly three simultaneous requests, next wave includes omitted proxy, valid 332 cached")
}
