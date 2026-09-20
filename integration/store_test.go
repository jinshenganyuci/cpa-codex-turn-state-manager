package integration

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test-only CONNECT endpoint terminates TLS for a synthetic chatgpt.com upstream.
// Its CA is trusted only by the isolated CPA child process.
func freshInstallProxy(t *testing.T, handler http.Handler) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "chatgpt.com"}, DNSNames: []string{"chatgpt.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pair, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(t.TempDir(), "test-ca.pem")
	if err := os.WriteFile(ca, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	target := httptest.NewUnstartedServer(handler)
	target.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	target.EnableHTTP2 = true
	target.StartTLS()
	t.Cleanup(target.Close)
	var mu sync.Mutex
	var conns []net.Conn
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "chatgpt.com:443" {
			http.Error(w, "synthetic upstream only", 502)
			return
		}
		remote, err := net.Dial("tcp", target.Listener.Addr().String())
		if err != nil {
			http.Error(w, "synthetic upstream unavailable", 502)
			return
		}
		defer func() { _ = remote.Close() }()
		conn, b, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		mu.Lock()
		conns = append(conns, conn)
		mu.Unlock()
		_, _ = b.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = b.Flush()
		go func() { _, _ = io.Copy(remote, b); _ = remote.Close() }()
		_, _ = io.Copy(conn, remote)
	}))
	t.Cleanup(func() {
		proxy.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	return proxy.URL, ca
}

func TestRealCPAStoreInstallWithoutPluginParameters(t *testing.T) {
	if os.Getenv("CPA_TEST_PLUGIN") == "" || os.Getenv("CPA_TEST_BINARY") == "" {
		t.Skip("set native integration paths")
	}
	var upstream *mockUpstream
	proxy, ca := freshInstallProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { upstream.server.Config.Handler.ServeHTTP(w, r) }))
	library, err := os.ReadFile(os.Getenv("CPA_TEST_PLUGIN"))
	if err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	archive := zip.NewWriter(&payload)
	file, err := archive.Create("codex-turn-state-manager.so")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write(library)
	if archive.Close() != nil {
		t.Fatal("archive")
	}
	sum := sha256.Sum256(payload.Bytes())
	var registryURL string
	store := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plugin.zip" {
			_, _ = w.Write(payload.Bytes())
			return
		}
		data := map[string]any{"schema_version": 2, "plugins": []any{map[string]any{"id": "codex-turn-state-manager", "name": "Codex Turn State Manager", "description": "Fresh install test", "author": "jinshenganyuci", "version": "0.3.4", "repository": "https://github.com/jinshenganyuci/cpa-codex-turn-state-manager", "install": map[string]any{"type": "direct", "artifacts": []any{map[string]any{"goos": "linux", "goarch": "amd64", "url": strings.TrimSuffix(registryURL, "/registry.json") + "/plugin.zip", "sha256": fmt.Sprintf("%x", sum)}}}}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(data)
	}))
	defer store.Close()
	caFile, err := os.OpenFile(ca, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(caFile, &pem.Block{Type: "CERTIFICATE", Bytes: store.Certificate().Raw}); err != nil {
		t.Fatal(err)
	}
	if err := caFile.Close(); err != nil {
		t.Fatal(err)
	}
	registryURL = store.URL + "/registry.json"
	if public := os.Getenv("CPA_TEST_PUBLIC_REGISTRY_URL"); public != "" {
		registryURL = public
	}
	t.Setenv("CPA_TEST_CA_FILE", ca)
	t.Setenv("CPA_TEST_REGISTRY_URL", registryURL)
	t.Setenv("CPA_TEST_FRESH_INSTALL", "true")
	t.Setenv("CPA_TEST_STORE_INSTALL", "true")
	t.Setenv("CPA_TEST_NO_HEADER_MAPPING", "true")
	h := startHost(t)
	upstream = h.mock
	sourceHash := sha256.Sum256([]byte(registryURL))
	code, raw := h.call(t, "POST", fmt.Sprintf("/v0/management/plugin-store/codex-turn-state-manager/install?source=source-%x", sourceHash[:6]), nil, managementKey)
	if code != 200 {
		t.Fatalf("store install: %d %s", code, raw)
	}
	await(t, func() bool {
		code, _ := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		return code == 200
	})
	type statusView struct {
		Version    string `json:"version"`
		Mode       string `json:"mode"`
		Guard      bool   `json:"blocking_active"`
		Probe      bool   `json:"probe_enabled"`
		Continuous bool   `json:"continuous"`
		Parallel   bool   `json:"probe_parallel"`
		Valid      int    `json:"valid_count"`
		Selection  struct {
			Revision   uint64 `json:"revision"`
			Persistent bool   `json:"persistent"`
			Accounts   []struct {
				Account string `json:"account"`
				Email   string `json:"email"`
			} `json:"accounts"`
		} `json:"selection"`
	}
	read := func() statusView {
		_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		var s statusView
		if json.Unmarshal(raw, &s) != nil {
			t.Fatal("status")
		}
		return s
	}
	s := read()
	if s.Version != "0.3.4" || s.Mode != "force" || s.Guard || !s.Probe || !s.Continuous || !s.Parallel || !s.Selection.Persistent {
		t.Fatalf("fresh install needs manual parameters: %+v", s)
	}
	claims, _ := json.Marshal(map[string]any{"email": "fresh@example.test", "https://api.openai.com/auth": map[string]string{"chatgpt_plan_type": "team"}})
	auth, _ := json.Marshal(map[string]any{"type": "codex", "email": "fresh@example.test", "access_token": "fresh-synthetic-token", "account_id": "fresh-account", "proxy_url": proxy, "id_token": "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"})
	if err := os.WriteFile(filepath.Join(h.root, "auths", "fresh.json"), auth, 0600); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { s = read(); return len(s.Selection.Accounts) == 1 })
	// Unknown reported models prevent business recovery, demonstrating a real cache miss after selection.
	upstream.mu.Lock()
	upstream.responseModel = "gpt-5.6-luna"
	upstream.mu.Unlock()
	selection, _ := json.Marshal(map[string]any{"revision": s.Selection.Revision, "accounts": []string{s.Selection.Accounts[0].Account}, "models": []string{"gpt-6-astra"}})
	code, raw = h.call(t, "POST", "/v0/management/codex-turn-state-manager/selection", selection, managementKey)
	if code != 200 {
		t.Fatalf("UI selection: %d %s", code, raw)
	}
	code, raw = h.call(t, "POST", "/v1/responses", []byte(`{"model":"gpt-6-astra","input":"Reply OK","stream":true}`), clientKey)
	if code != 200 {
		t.Fatalf("missing cache blocked: %d %s", code, raw)
	}
	upstream.mu.Lock()
	upstream.responseModel = "gpt-6-astra"
	upstream.state = syntheticToken(12)
	upstream.mu.Unlock()
	await(t, func() bool { return read().Valid == 1 })
	for len(upstream.received) > 0 {
		<-upstream.received
	}
	code, raw = h.call(t, "POST", "/v1/responses", []byte(`{"model":"gpt-6-astra","input":"Reply OK","stream":true}`), clientKey)
	if code != 200 {
		t.Fatalf("cached request: %d %s", code, raw)
	}
	captured := <-upstream.received
	if len(captured.Headers.Get("X-Codex-Turn-State")) != 332 {
		t.Fatal("state not forwarded without manual mapping")
	}
	path := filepath.Join(h.root, "plugins", ".codex-turn-state-manager", "current.json.selection.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatal("selection not saved in mounted plugin directory")
	}
	_, raw = h.call(t, "GET", "/v0/management/plugins/codex-turn-state-manager/config", nil, managementKey)
	var config map[string]any
	_ = json.Unmarshal(raw, &config)
	for _, field := range []string{"state_file", "selection_file", "probe", "defaults", "block_without_state"} {
		if _, exists := config[field]; exists {
			t.Fatalf("manual parameter injected: %s", field)
		}
	}
	// Observe two scheduler intervals; cache age/expiry boundaries are covered with fake clocks in unit tests.
	timer := time.NewTimer(11 * time.Second)
	defer timer.Stop()
	select {
	case <-upstream.received:
		t.Fatal("fresh 332 triggered another automatic upstream request")
	case <-timer.C:
	}
	t.Log("store install, UI selection, credential proxy, passthrough, cached 332 forwarding and acquisition pause passed without plugin parameters")
}
