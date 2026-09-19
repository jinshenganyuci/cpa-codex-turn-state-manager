// Command livecheck requires an isolated container and an explicit private relay.
// It never prints credential material or complete turn states.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const private = "/private"
const pluginID = "codex-turn-state-manager"

type entry struct {
	Account   string `json:"account"`
	Model     string `json:"model"`
	Length    int    `json:"state_length"`
	Observed  int    `json:"last_observed_length"`
	LastProbe string `json:"last_probe"`
	Probing   bool   `json:"probing"`
	Pending   string `json:"pending"`
}
type observation struct {
	HostOutcome       string `json:"host_outcome,omitempty"`
	TerminalCompleted bool   `json:"terminal_completed"`
	TerminalFailed    bool   `json:"terminal_failed"`
	At                string `json:"at"`
	Length            int    `json:"length"`
	Success           bool   `json:"success"`
	Action            string `json:"action"`
	Reasoning         string `json:"reasoning"`
}
type snapshot struct {
	Entries []entry       `json:"entries"`
	History []observation `json:"history"`
}
type result struct {
	Proxy     int     `json:"proxy"`
	Transport string  `json:"transport"`
	Model     string  `json:"model"`
	Reasoning string  `json:"reasoning"`
	Status    int     `json:"http_status"`
	Completed bool    `json:"completed"`
	Length    int     `json:"state_length"`
	Outcome   string  `json:"outcome"`
	Seconds   float64 `json:"seconds"`
}

func encode(v any) []byte { b, _ := json.Marshal(v); return b }
func emit(v any)          { fmt.Println(string(encode(v))) }
func main() {
	if err := run(); err != nil {
		emit(map[string]any{"error": err.Error()})
		os.Exit(1)
	}
}
func run() error {
	interfaces, err := net.Interfaces()
	if err != nil {
		return errors.New("interfaces_unavailable")
	}
	for _, iface := range interfaces {
		if iface.Name != "lo" {
			return errors.New("requires_network_none")
		}
	}
	emit(map[string]any{"network_interfaces": []string{"lo"}, "model": "gpt-6-astra", "reasoning": "medium"})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return errors.New("relay_listen_failed")
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				peer, err := net.Dial("unix", private+"/proxy.sock")
				if err != nil {
					return
				}
				defer peer.Close()
				done := make(chan struct{}, 1)
				go func() { _, _ = io.Copy(peer, conn); done <- struct{}{} }()
				_, _ = io.Copy(conn, peer)
				_ = conn.Close()
				<-done
			}()
		}
	}()
	raw, err := os.ReadFile(private + "/credential.json")
	if err != nil {
		return errors.New("credential_unavailable")
	}
	var credential map[string]any
	if json.Unmarshal(raw, &credential) != nil {
		return errors.New("credential_invalid")
	}
	var results []result
	phases := 2
	if os.Getenv("CPA_LIVE_TERMINAL_CHECK") == "true" {
		phases = 1
	}
	for phase := 1; phase <= phases; phase++ {
		if phase == 2 {
			files := archived292()
			if len(files) > 0 {
				break
			}
		}
		if err := os.WriteFile(private+"/phase", []byte(fmt.Sprint(phase)), 0600); err != nil {
			return errors.New("phase_write_failed")
		}
		var profile struct {
			URL string `json:"url"`
		}
		raw, err := os.ReadFile(fmt.Sprintf(private+"/proxy-%d.json", phase))
		if err != nil || json.Unmarshal(raw, &profile) != nil {
			return errors.New("proxy_profile_invalid")
		}
		proxy, err := url.Parse(profile.URL)
		if err != nil || (proxy.Scheme != "socks5" && proxy.Scheme != "socks5h") {
			return errors.New("proxy_scheme_invalid")
		}
		proxy.Host = listener.Addr().String()
		phaseResults, err := runPhase(phase, credential, proxy.String())
		results = append(results, phaseResults...)
		if err != nil {
			return err
		}
	}
	files := archived292()
	report := map[string]any{"model": "gpt-6-astra", "reasoning_effort": "medium", "network_none": true, "results": results, "archived_292": len(files), "replay_tested": false}
	raw = encode(report)
	if err := os.WriteFile(private+reportName(), raw, 0600); err != nil {
		return errors.New("report_write_failed")
	}
	emit(report)
	return nil
}
func runPhase(phase int, original map[string]any, proxy string) ([]result, error) {
	root, err := os.MkdirTemp("/tmp", "turn-state-live-")
	if err != nil {
		return nil, errors.New("temp_failed")
	}
	defer func() {
		for _, name := range []string{"cpa.log"} {
			raw, _ := os.ReadFile(filepath.Join(root, name))
			_ = os.WriteFile(fmt.Sprintf(private+"/debug-%d.log", phase), raw, 0600)
		}
		_ = os.RemoveAll(root)
	}()
	for _, dir := range []string{"auths", "plugins"} {
		if os.Mkdir(filepath.Join(root, dir), 0700) != nil {
			return nil, errors.New("mkdir_failed")
		}
	}
	credential := make(map[string]any)
	for k, v := range original {
		credential[k] = v
	}
	credential["proxy_url"] = proxy
	credential["websockets"] = true
	credential["email"] = "local-verification@example.invalid"
	credential["headers"] = map[string]string{"X-Codex-Turn-State": "$X-Codex-Turn-State"}
	if err := os.WriteFile(filepath.Join(root, "auths/local-codex.json"), encode(credential), 0600); err != nil {
		return nil, errors.New("auth_write_failed")
	}
	library, err := os.ReadFile("/artifact/" + pluginID + ".so")
	if err != nil {
		return nil, errors.New("plugin_read_failed")
	}
	if os.WriteFile(filepath.Join(root, "plugins", pluginID+".so"), library, 0600) != nil {
		return nil, errors.New("plugin_copy_failed")
	}
	reserve, _ := net.Listen("tcp", "127.0.0.1:0")
	port := reserve.Addr().(*net.TCPAddr).Port
	_ = reserve.Close()
	keyBytes := make([]byte, 20)
	_, _ = rand.Read(keyBytes)
	key := hex.EncodeToString(keyBytes)
	config := fmt.Sprintf(`host: "127.0.0.1"
port: %d
auth-dir: "%s/auths"
remote-management:
  secret-key: "%s"
  allow-remote: false
  disable-control-panel: true
  disable-auto-update-panel: true
api-keys: ["%s"]
request-retry: 0
max-retry-credentials: 1
request-log: true
disable-image-generation: true
plugins:
  enabled: true
  dir: "%s/plugins"
  configs:
    codex-turn-state-manager:
      enabled: true
      mode: observe
      selection_required: false
      state_file: "/private/states-%d.json"
      archive_dir: "/private/archive"
      defaults:
        accepted_blocks: [10, 12]
        models: [gpt-6-astra]
      probe:
        enabled: true
        proxy_mode: credential
        reasoning_effort: medium
        on_missing: false
        background_refresh: false
        refresh_on_errors: false
        max_attempts: 1
        retry_seconds: 1
        max_per_hour: 4
`, port, root, key, key, root, phase)
	configPath := filepath.Join(root, "config.yaml")
	if os.WriteFile(configPath, []byte(config), 0600) != nil {
		return nil, errors.New("config_failed")
	}
	logfile, err := os.OpenFile(filepath.Join(root, "cpa.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errors.New("log_failed")
	}
	defer logfile.Close()
	binary := os.Getenv("CPA_TEST_BINARY")
	if binary == "" {
		binary = "/CLIProxyAPI/CLIProxyAPI"
	}
	cmd := exec.Command(binary, "--config", configPath, "--local-model")
	cmd.Dir = root
	cmd.Stdout = logfile
	cmd.Stderr = logfile
	if cmd.Start() != nil {
		return nil, errors.New("host_start_failed")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	call := func(method, path string, body []byte) (int, []byte, error) {
		req, _ := http.NewRequest(method, base+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, errors.New("local_request_failed")
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return resp.StatusCode, data, err
	}
	getSnapshot := func() snapshot {
		_, raw, _ := call("GET", "/v0/management/"+pluginID+"/status", nil)
		var value snapshot
		_ = json.Unmarshal(raw, &value)
		return value
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if len(getSnapshot().Entries) > 0 {
			break
		}
		if time.Now().After(deadline) {
			status, raw, _ := call("GET", "/v0/management/"+pluginID+"/status", nil)
			emit(map[string]any{"readiness_status": status, "snapshot_bytes": len(raw)})
			return nil, errors.New("host_or_plugin_not_ready")
		}
		select {
		case <-done:
			return nil, errors.New("host_exited")
		case <-time.After(150 * time.Millisecond):
		}
	}
	var results []result
	record := func(r result) { r.Model = "gpt-6-astra"; r.Reasoning = "medium"; results = append(results, r); emit(r) }
	requestCount := 3
	if os.Getenv("CPA_LIVE_TERMINAL_CHECK") == "true" {
		requestCount = 2
	}
	if os.Getenv("CPA_LIVE_PROBES_ONLY") == "true" {
		requestCount = 0
	}
	for index := 0; index < requestCount; index++ {
		start := time.Now()
		r := result{Proxy: phase, Transport: []string{"http_sse", "http_json", "websocket"}[index]}
		if index < 2 {
			status, body, err := call("POST", "/v1/responses", encode(map[string]any{"model": "gpt-6-astra", "input": []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": "Reply with exactly OK."}}}}, "reasoning": map[string]string{"effort": "medium"}, "stream": index == 0}))
			r.Status = status
			if err != nil {
				r.Outcome = "read_failed"
			} else if status == 200 {
				r.Completed = completed(body)
				r.Outcome = "received"
			} else {
				r.Outcome = "upstream_rejected"
			}
		} else {
			conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer " + key}})
			if err != nil {
				r.Outcome = "websocket_connect_failed"
				if resp != nil {
					r.Status = resp.StatusCode
				}
			} else {
				r.Status = 101
				err = conn.WriteMessage(websocket.TextMessage, encode(map[string]any{"type": "response.create", "model": "gpt-6-astra", "input": []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": "Reply with exactly OK."}}}}, "reasoning": map[string]string{"effort": "medium"}}))
				for err == nil {
					var raw []byte
					_, raw, err = conn.ReadMessage()
					if err != nil {
						break
					}
					var ev struct {
						Type   string `json:"type"`
						Status int    `json:"status"`
					}
					_ = json.Unmarshal(raw, &ev)
					if ev.Type == "response.completed" {
						r.Completed = completed(raw)
						r.Outcome = "received"
						break
					}
					if ev.Type == "error" || ev.Type == "response.failed" || ev.Type == "response.incomplete" {
						r.Outcome = "upstream_rejected"
						if ev.Status != 0 {
							r.Status = ev.Status
						}
						break
					}
				}
				_ = conn.Close()
				if r.Outcome == "" {
					r.Outcome = "websocket_incomplete"
				}
			}
		}
		snap := getSnapshot()
		if len(snap.Entries) > 0 {
			r.Length = snap.Entries[0].Observed
		}
		r.Seconds = time.Since(start).Seconds()
		record(r)
		if !r.Completed {
			break
		}
		if index < 2 {
			<-time.After(3 * time.Second)
		}
	}
	files := archived292()
	if os.Getenv("CPA_LIVE_TERMINAL_CHECK") != "true" && len(files) == 0 && (len(results) == 0 || results[len(results)-1].Completed) {
		snap := getSnapshot()
		account := snap.Entries[0].Account
		for attempt := 0; attempt < 2; attempt++ {
			before := len(getSnapshot().History)
			start := time.Now()
			status, _, err := call("POST", "/v0/management/"+pluginID+"/probe", encode(map[string]string{"account": account, "model": "gpt-6-astra"}))
			if err != nil || status != 202 {
				return results, errors.New("manual_probe_not_queued")
			}
			for {
				snap = getSnapshot()
				if len(snap.Entries) > 0 && !snap.Entries[0].Probing && snap.Entries[0].Pending == "" && (len(snap.History) > before || snap.Entries[0].LastProbe != "") {
					break
				}
				<-time.After(300 * time.Millisecond)
			}
			var last observation
			if len(snap.History) > before {
				last = snap.History[len(snap.History)-1]
			}
			r := result{Proxy: phase, Transport: "plugin_probe", Completed: last.Success, Length: last.Length, Outcome: snap.Entries[0].LastProbe, Seconds: time.Since(start).Seconds()}
			if last.Success {
				r.Status = 200
			}
			record(r)
			files = archived292()
			if len(files) > 0 || !last.Success {
				break
			}
			<-time.After(3 * time.Second)
		}
	}
	snapshotName := fmt.Sprintf(private+"/snapshot-%d.json", phase)
	if os.Getenv("CPA_LIVE_TERMINAL_CHECK") == "true" {
		snapshotName = private + "/snapshot-terminal.json"
	}
	if err := os.WriteFile(snapshotName, encode(getSnapshot()), 0600); err != nil {
		return results, errors.New("snapshot_failed")
	}
	// Only retain the sanitized report; request logs are destroyed with the sandbox.
	return results, nil
}
func completed(raw []byte) bool {
	check := func(data []byte) bool {
		var ev struct {
			Type     string `json:"type"`
			Status   string `json:"status"`
			Response struct {
				Status string `json:"status"`
			} `json:"response"`
		}
		if json.Unmarshal(data, &ev) != nil {
			return false
		}
		return ev.Status == "completed" || ev.Type == "response.completed" && ev.Response.Status == "completed"
	}
	if check(raw) {
		return true
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.HasPrefix(line, []byte("data: ")) && check(line[6:]) {
			return true
		}
	}
	return false
}

func reportName() string {
	if os.Getenv("CPA_LIVE_TERMINAL_CHECK") == "true" {
		return "/live-report-terminal.json"
	}
	return "/live-report.json"
}

// Both lengths are archived; this legacy acquisition driver targets only 292.
func archived292() []string {
	paths, _ := filepath.Glob(private + "/archive/*.json")
	var matches []string
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var entry struct {
			State struct {
				Value string `json:"state"`
			} `json:"state"`
		}
		if json.Unmarshal(raw, &entry) == nil && len(entry.State.Value) == 292 {
			matches = append(matches, path)
		}
	}
	return matches
}
