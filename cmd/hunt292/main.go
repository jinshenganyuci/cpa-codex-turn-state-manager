// Command hunt292 runs at most 100 sequential requests in a network-isolated container.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

const maxAttempts = 100
const model = "gpt-6-astra"
const pluginID = "codex-turn-state-manager"
const private = "/private"

var runDir = private + "/hunt-20260918-high-medium"

type attempt struct {
	Number         int       `json:"number"`
	Proxy          int       `json:"proxy"`
	Effort         string    `json:"effort"`
	Started        time.Time `json:"started"`
	Seconds        float64   `json:"seconds"`
	HTTPStatus     int       `json:"http_status"`
	Completed      bool      `json:"completed"`
	StateLength    int       `json:"state_length"`
	PluginSuccess  bool      `json:"plugin_success"`
	ResponseModel  string    `json:"response_model,omitempty"`
	ResponseEffort string    `json:"response_effort,omitempty"`
	Error          string    `json:"error,omitempty"`
	ErrorClass     string    `json:"error_class,omitempty"`
}
type report struct {
	Model        string    `json:"model"`
	Limit        int       `json:"limit"`
	NetworkNone  bool      `json:"network_none"`
	PluginSHA256 string    `json:"plugin_sha256"`
	Found292     bool      `json:"found_292"`
	Archive      string    `json:"archive,omitempty"`
	StopReason   string    `json:"stop_reason"`
	Attempts     []attempt `json:"attempts"`
}
type snapshot struct {
	Entries []struct {
		Length int `json:"last_observed_length"`
	} `json:"entries"`
	History []struct {
		Success   bool   `json:"success"`
		Length    int    `json:"length"`
		Reasoning string `json:"reasoning"`
	} `json:"history"`
}
type host struct {
	base, key, root string
	cmd             *exec.Cmd
	done            chan error
	log             *os.File
}

func schedule(index int) (int, string) {
	effort := "high"
	if index%2 == 1 {
		effort = "medium"
	}
	return 1 + (index/2)%2, effort
}
func encoded(v any) []byte { raw, _ := json.Marshal(v); return raw }
func emit(v any)           { fmt.Println(string(encoded(v))) }
func main() {
	runner := run
	if os.Getenv("CPA_HUNT_FAST") == "true" {
		runner = runFast
	}
	if err := runner(); err != nil {
		emit(map[string]string{"error": err.Error()})
		os.Exit(1)
	}
}
func saveReport(r report) error {
	path := runDir + "/report.json"
	temp := path + ".tmp"
	if err := os.WriteFile(temp, encoded(r), 0600); err != nil {
		return errors.New("write_report_failed")
	}
	if err := os.Rename(temp, path); err != nil {
		return errors.New("commit_report_failed")
	}
	return nil
}
func run() error {
	interfaces, err := net.Interfaces()
	if err != nil {
		return errors.New("interfaces_unavailable")
	}
	for _, iface := range interfaces {
		if iface.Name != "lo" {
			return errors.New("network_none_required")
		}
	}
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return errors.New("private_directory_failed")
	}
	library, err := os.ReadFile("/artifact/" + pluginID + ".so")
	if err != nil {
		return errors.New("plugin_unavailable")
	}
	sum := sha256.Sum256(library)
	r := report{Model: model, Limit: maxAttempts, NetworkNone: true, PluginSHA256: hex.EncodeToString(sum[:]), StopReason: "running"}
	if raw, err := os.ReadFile(runDir + "/report.json"); err == nil {
		if json.Unmarshal(raw, &r) != nil {
			return errors.New("existing_report_invalid")
		}
		if r.Model != model || r.Limit != maxAttempts {
			return errors.New("report_scope_mismatch")
		}
	}
	if r.Found292 || len(r.Attempts) >= maxAttempts {
		emit(map[string]any{"attempts": len(r.Attempts), "found_292": r.Found292, "stop_reason": r.StopReason})
		return nil
	}
	r.StopReason = "running"
	raw, err := os.ReadFile(private + "/credential.json")
	if err != nil {
		return errors.New("credential_unavailable")
	}
	var credential map[string]any
	if json.Unmarshal(raw, &credential) != nil {
		return errors.New("credential_invalid")
	}
	var hosts [2]*host
	for index := 1; index <= 2; index++ {
		bridge, err := startBridge(index)
		if err != nil {
			return err
		}
		defer bridge.Close()
		var profile struct {
			URL string `json:"url"`
		}
		raw, err := os.ReadFile(fmt.Sprintf(private+"/proxy-%d.json", index))
		if err != nil || json.Unmarshal(raw, &profile) != nil {
			return errors.New("proxy_profile_invalid")
		}
		proxy, err := url.Parse(profile.URL)
		if err != nil || (proxy.Scheme != "socks5" && proxy.Scheme != "socks5h") {
			return errors.New("proxy_scheme_invalid")
		}
		proxy.Host = bridge.Addr().String()
		h, err := startHost(index, credential, proxy.String(), library)
		if err != nil {
			return err
		}
		hosts[index-1] = h
		defer h.close()
	}
	emit(map[string]any{"event": "started", "model": model, "limit": maxAttempts, "prior_attempts": len(r.Attempts), "network_interfaces": []string{"lo"}, "schedule": "proxy1 high, proxy1 medium, proxy2 high, proxy2 medium"})
	failures := 0
	for index := len(r.Attempts); index < maxAttempts; index++ {
		if name, ok := find292(); ok {
			r.Found292 = true
			r.Archive = name
			r.StopReason = "292_preserved"
			break
		}
		proxy, effort := schedule(index)
		h := hosts[proxy-1]
		before, err := h.snapshot()
		if err != nil {
			return err
		}
		item := attempt{Number: index + 1, Proxy: proxy, Effort: effort, Started: time.Now().UTC(), Error: "in_flight"}
		// Count and persist before sending so an interrupted run cannot exceed its cap.
		r.Attempts = append(r.Attempts, item)
		if err := saveReport(r); err != nil {
			return err
		}
		body := encoded(map[string]any{"model": model, "stream": false, "store": false, "reasoning": map[string]string{"effort": effort}, "input": []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": "Reply with exactly OK."}}}}})
		status, response, err := h.call("POST", "/v1/responses", body)
		item.HTTPStatus = status
		item.Error = ""
		if err != nil {
			item.Error = "request_failed"
		} else {
			var value struct {
				Status    string `json:"status"`
				Model     string `json:"model"`
				Reasoning struct {
					Effort string `json:"effort"`
				} `json:"reasoning"`
			}
			if json.Unmarshal(response, &value) == nil {
				item.Completed = status == 200 && value.Status == "completed"
				item.ResponseModel = value.Model
				item.ResponseEffort = value.Reasoning.Effort
			}
			if !item.Completed {
				item.Error = "upstream_not_completed"
				item.ErrorClass = classifyFailure(response)
				errorDir := runDir + "/private-errors"
				if err := os.MkdirAll(errorDir, 0700); err != nil {
					return errors.New("private_error_directory_failed")
				}
				if err := os.WriteFile(filepath.Join(errorDir, fmt.Sprintf("attempt-%03d.json", index+1)), response, 0600); err != nil {
					return errors.New("private_error_write_failed")
				}
			}
		}
		// Completion hooks are asynchronous; this only waits on local bookkeeping.
		until := time.Now().Add(5 * time.Second)
		for status == http.StatusOK {
			snap, err := h.snapshot()
			if err != nil {
				return err
			}
			if len(snap.History) > len(before.History) {
				last := snap.History[len(snap.History)-1]
				item.StateLength = last.Length
				item.PluginSuccess = last.Success
				if last.Reasoning != effort {
					item.Error = "plugin_effort_mismatch"
				}
				break
			}
			if time.Now().After(until) {
				item.Error = "plugin_completion_missing"
				break
			}
			<-time.After(50 * time.Millisecond)
		}
		item.Seconds = time.Since(item.Started).Seconds()
		r.Attempts[len(r.Attempts)-1] = item
		if name, ok := find292(); ok {
			r.Found292 = true
			r.Archive = name
			r.StopReason = "292_preserved"
		}
		if err := saveReport(r); err != nil {
			return err
		}
		emit(item)
		if r.Found292 {
			break
		}
		if status == 401 || status == 403 || status == 429 {
			r.StopReason = fmt.Sprintf("upstream_%d", status)
			break
		}
		if item.ResponseEffort != "" && item.ResponseEffort != effort {
			r.StopReason = "response_effort_mismatch"
			break
		}
		if item.Error != "" {
			failures++
		} else {
			failures = 0
		}
		if failures >= 5 {
			r.StopReason = "five_consecutive_failures"
			break
		}
		if index+1 < maxAttempts {
			delay := 3 * time.Second
			if status >= 500 {
				delay = 15 * time.Second
			}
			<-time.After(delay)
		}
	}
	if r.StopReason == "running" {
		r.StopReason = "100_request_limit"
	}
	if err := saveReport(r); err != nil {
		return err
	}
	emit(map[string]any{"event": "finished", "attempts": len(r.Attempts), "found_292": r.Found292, "stop_reason": r.StopReason, "archive": r.Archive})
	return nil
}
func startBridge(proxy int) (net.Listener, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("bridge_failed")
	}
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				remote, err := net.Dial("unix", fmt.Sprintf(private+"/hunt-proxy-%d.sock", proxy))
				if err != nil {
					return
				}
				defer remote.Close()
				go func() { _, _ = io.Copy(remote, client); _ = remote.Close() }()
				_, _ = io.Copy(client, remote)
			}()
		}
	}()
	return listener, nil
}
func startHost(index int, original map[string]any, proxy string, library []byte) (*host, error) {
	root, err := os.MkdirTemp("/tmp", "hunt292-")
	if err != nil {
		return nil, errors.New("temp_failed")
	}
	h := &host{root: root}
	ok := false
	defer func() {
		if !ok {
			h.close()
		}
	}()
	for _, name := range []string{"auths", "plugins"} {
		if os.Mkdir(filepath.Join(root, name), 0700) != nil {
			return nil, errors.New("mkdir_failed")
		}
	}
	credential := make(map[string]any)
	for k, v := range original {
		credential[k] = v
	}
	credential["proxy_url"] = proxy
	credential["request-retry"] = 0
	credential["websockets"] = false
	credential["email"] = "local-verification@example.invalid"
	// The existing access token is enough; this run never initiates an OAuth refresh.
	delete(credential, "refresh_token")
	if os.WriteFile(filepath.Join(root, "auths/local-codex.json"), encoded(credential), 0600) != nil {
		return nil, errors.New("credential_copy_failed")
	}
	if os.WriteFile(filepath.Join(root, "plugins", pluginID+".so"), library, 0600) != nil {
		return nil, errors.New("plugin_copy_failed")
	}
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("port_failed")
	}
	port := reserve.Addr().(*net.TCPAddr).Port
	_ = reserve.Close()
	key := make([]byte, 20)
	if _, err := rand.Read(key); err != nil {
		return nil, errors.New("key_failed")
	}
	h.key = hex.EncodeToString(key)
	h.base = fmt.Sprintf("http://127.0.0.1:%d", port)
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
request-log: false
disable-image-generation: true
streaming:
  bootstrap-retries: 0
plugins:
  enabled: true
  dir: "%s/plugins"
  configs:
    codex-turn-state-manager:
      enabled: true
      mode: observe
      selection_required: false
      state_file: "%s/states-%d.json"
      archive_dir: "%s/archive"
      defaults:
        accepted_blocks: [10, 12]
        models: [gpt-6-astra]
      probe:
        enabled: false
`, port, root, h.key, h.key, root, runDir, index, runDir)
	path := filepath.Join(root, "config.yaml")
	if os.WriteFile(path, []byte(config), 0600) != nil {
		return nil, errors.New("config_failed")
	}
	h.log, err = os.OpenFile(filepath.Join(root, "host.log"), os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return nil, errors.New("log_failed")
	}
	h.cmd = exec.Command("/CLIProxyAPI/CLIProxyAPI", "--config", path, "--local-model")
	h.cmd.Dir = root
	h.cmd.Stdout = h.log
	h.cmd.Stderr = h.log
	if h.cmd.Start() != nil {
		return nil, errors.New("host_start_failed")
	}
	h.done = make(chan error, 1)
	go func() { h.done <- h.cmd.Wait() }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		snap, err := h.snapshot()
		if err == nil && len(snap.Entries) > 0 {
			ok = true
			return h, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("host_not_ready")
		}
		<-time.After(100 * time.Millisecond)
	}
}
func (h *host) close() {
	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Signal(os.Interrupt)
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			_ = h.cmd.Process.Kill()
			<-h.done
		}
	}
	if h.log != nil {
		_ = h.log.Close()
	}
	if h.root != "" {
		_ = os.RemoveAll(h.root)
	}
}
func (h *host) call(method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, h.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.New("local_request_invalid")
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, errors.New("local_request_failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, err
}
func (h *host) snapshot() (snapshot, error) {
	var snap snapshot
	status, raw, err := h.call("GET", "/v0/management/"+pluginID+"/status", nil)
	if err != nil || status != 200 || json.Unmarshal(raw, &snap) != nil {
		return snap, errors.New("snapshot_unavailable")
	}
	return snap, nil
}
func valid292(value string, now time.Time) bool {
	if len(value) != 292 {
		return false
	}
	raw, err := base64.URLEncoding.Strict().DecodeString(value)
	if err != nil || len(raw) != 217 || raw[0] != 0x80 {
		return false
	}
	seconds := binary.BigEndian.Uint64(raw[1:9])
	if seconds > uint64(now.Add(5*time.Minute).Unix()) {
		return false
	}
	return seconds > uint64(now.Add(-time.Hour).Unix())
}
func find292() (string, bool) {
	files, _ := filepath.Glob(runDir + "/archive/*.json")
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var value struct {
			State struct {
				Value string `json:"state"`
			} `json:"state"`
			Model     string `json:"model"`
			Reasoning string `json:"reasoning"`
			Source    string `json:"source"`
			SHA256    string `json:"sha256"`
		}
		if json.Unmarshal(raw, &value) != nil || value.Model != model || (value.Reasoning != "medium" && value.Reasoning != "high") || value.Source != "successful_upstream_response" || !valid292(value.State.Value, time.Now()) {
			continue
		}
		sum := sha256.Sum256([]byte(value.State.Value))
		if value.SHA256 == hex.EncodeToString(sum[:]) {
			return filepath.Base(path), true
		}
	}
	return "", false
}

// Return fixed categories; raw provider errors are retained only in the private run directory.
func classifyFailure(raw []byte) string {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "non_json_error"
	}
	text := strings.ToLower(body.Error.Code + " " + body.Error.Type + " " + body.Error.Message)
	switch {
	case strings.Contains(text, "cooldown") || strings.Contains(text, "cooling"):
		return "credential_cooldown"
	case strings.Contains(text, "auth_unavailable") || strings.Contains(text, "no auth available"):
		return "auth_unavailable"
	case strings.Contains(text, "timeout") || strings.Contains(text, "deadline exceeded"):
		return "connection_timeout"
	case strings.Contains(text, "proxy") || strings.Contains(text, "socks"):
		return "proxy_error"
	case strings.Contains(text, "rate_limit") || strings.Contains(text, "quota"):
		return "rate_or_quota_limit"
	case strings.Contains(text, "invalid_api_key") || strings.Contains(text, "authentication"):
		return "authentication_error"
	case strings.Contains(text, "eof"):
		return "connection_closed"
	default:
		return "other_upstream_error"
	}
}
