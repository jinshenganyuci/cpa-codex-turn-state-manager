package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const perProxyLimit = 50

var configuredProxyLimit = "50"

type fastReport struct {
	report
	LimitPerProxy    int       `json:"limit_per_proxy"`
	Concurrency      int       `json:"concurrency"`
	CredentialStatus string    `json:"credential_status"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at,omitempty"`
}
type fastJournal struct {
	mu     sync.Mutex
	value  fastReport
	counts [2]int
	path   string
	ctx    context.Context
	cancel context.CancelFunc
}

func (j *fastJournal) saveLocked() error {
	raw, err := json.Marshal(j.value)
	if err != nil {
		return errors.New("report_encode_failed")
	}
	temp := j.path + ".tmp"
	if os.WriteFile(temp, raw, 0600) != nil {
		return errors.New("report_write_failed")
	}
	if os.Rename(temp, j.path) != nil {
		return errors.New("report_commit_failed")
	}
	return nil
}
func (j *fastJournal) reserve(proxy int) (attempt, int, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if proxy < 1 || proxy > 2 || j.ctx.Err() != nil || j.counts[proxy-1] >= j.proxyLimit() || len(j.value.Attempts) >= 2*j.proxyLimit() {
		return attempt{}, 0, false, nil
	}
	effort := "high"
	if j.counts[proxy-1]%2 == 1 {
		effort = "medium"
	}
	row := attempt{Number: len(j.value.Attempts) + 1, Proxy: proxy, Effort: effort, Started: time.Now().UTC(), Error: "in_flight"}
	slot := len(j.value.Attempts)
	j.value.Attempts = append(j.value.Attempts, row)
	j.counts[proxy-1]++
	if err := j.saveLocked(); err != nil {
		j.value.Attempts = j.value.Attempts[:slot]
		j.counts[proxy-1]--
		return attempt{}, 0, false, err
	}
	return row, slot, true, nil
}
func (j *fastJournal) stop(reason string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.value.StopReason == "running" {
		j.value.StopReason = reason
	}
	j.cancel()
	_ = j.saveLocked()
}
func (j *fastJournal) finish(slot int, row attempt, credentialStatus, archive string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.value.Attempts[slot] = row
	if credentialStatus != "" {
		j.value.CredentialStatus = credentialStatus
		if j.value.StopReason == "running" {
			j.value.StopReason = credentialStatus
		}
		j.cancel()
	}
	if archive != "" {
		j.value.Found292 = true
		j.value.Archive = archive
		if j.value.StopReason == "running" {
			j.value.StopReason = "292_preserved"
		}
		j.cancel()
	}
	return j.saveLocked()
}
func credentialIssue(status int, raw []byte) string {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	value := strings.ToLower(body.Error.Code + " " + body.Error.Type + " " + body.Error.Message)
	for _, code := range []string{"token_expired", "expired_token", "access token expired", "token has expired", "token is expired"} {
		if strings.Contains(value, code) {
			return "credential_expired"
		}
	}
	for _, code := range []string{"invalid_api_key", "invalid_token", "token_invalidated", "account_deactivated", "account_suspended", "session_expired", "authentication_error"} {
		if strings.Contains(value, code) {
			return "credential_rejected"
		}
	}
	if status == 401 {
		return "authentication_rejected_401"
	}
	if status == 403 {
		return "access_denied_403_not_proof_of_expiry"
	}
	return ""
}
func accessExpired(token string, now time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var value struct {
		Exp int64 `json:"exp"`
	}
	return json.Unmarshal(raw, &value) == nil && value.Exp > 0 && value.Exp <= now.Unix()
}
func (j *fastJournal) proxyLimit() int {
	if j.value.LimitPerProxy > 0 {
		return j.value.LimitPerProxy
	}
	return perProxyLimit
}

func runFast() error {
	proxyLimit, errLimit := strconv.Atoi(configuredProxyLimit)
	if errLimit != nil || proxyLimit < 1 || proxyLimit > perProxyLimit {
		return errors.New("per_proxy_limit_must_be_1_to_50")
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return errors.New("interfaces_unavailable")
	}
	for _, iface := range interfaces {
		if iface.Name != "lo" {
			return errors.New("network_none_required")
		}
	}
	if os.MkdirAll(runDir, 0700) != nil {
		return errors.New("private_directory_failed")
	}
	library, err := os.ReadFile("/artifact/" + pluginID + ".so")
	if err != nil {
		return errors.New("plugin_unavailable")
	}
	sum := sha256.Sum256(library)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j := &fastJournal{ctx: ctx, cancel: cancel, path: runDir + "/report.json", value: fastReport{report: report{Model: model, Limit: 2 * proxyLimit, NetworkNone: true, PluginSHA256: hex.EncodeToString(sum[:]), StopReason: "running"}, LimitPerProxy: proxyLimit, Concurrency: 2, CredentialStatus: "not_yet_checked", StartedAt: time.Now().UTC()}}
	if raw, err := os.ReadFile(j.path); err == nil {
		if json.Unmarshal(raw, &j.value) != nil || j.value.Model != model || j.value.LimitPerProxy != proxyLimit || j.value.Limit != 2*proxyLimit {
			return errors.New("existing_report_scope_invalid")
		}
		for _, row := range j.value.Attempts {
			if row.Proxy < 1 || row.Proxy > 2 {
				return errors.New("existing_proxy_invalid")
			}
			j.counts[row.Proxy-1]++
		}
		if j.value.Found292 || len(j.value.Attempts) >= 2*proxyLimit || strings.HasPrefix(j.value.CredentialStatus, "credential_") || strings.HasPrefix(j.value.CredentialStatus, "authentication_") || strings.HasPrefix(j.value.CredentialStatus, "access_denied_") {
			emit(map[string]any{"event": "already_finished", "attempts": len(j.value.Attempts), "stop_reason": j.value.StopReason})
			return nil
		}
		j.value.StopReason = "running"
	}
	raw, err := os.ReadFile(private + "/credential.json")
	if err != nil {
		return errors.New("credential_unavailable")
	}
	var credential map[string]any
	if json.Unmarshal(raw, &credential) != nil {
		return errors.New("credential_invalid")
	}
	token, _ := credential["access_token"].(string)
	if token == "" || credential["disabled"] == true || accessExpired(token, time.Now()) {
		j.value.CredentialStatus = "credential_expired_or_disabled"
		j.value.StopReason = j.value.CredentialStatus
		j.value.FinishedAt = time.Now().UTC()
		if err := j.saveLocked(); err != nil {
			return err
		}
		emit(map[string]any{"event": "credential_invalid", "attempts": len(j.value.Attempts), "reason": j.value.CredentialStatus})
		return nil
	}
	j.value.CredentialStatus = "access_token_not_expired_pending_live_check"
	if err := j.saveLocked(); err != nil {
		return err
	}
	var hosts [2]*host
	for proxyID := 1; proxyID <= 2; proxyID++ {
		bridge, err := startBridge(proxyID)
		if err != nil {
			return err
		}
		defer bridge.Close()
		var profile struct {
			URL string `json:"url"`
		}
		raw, err := os.ReadFile(fmt.Sprintf(private+"/proxy-%d.json", proxyID))
		if err != nil || json.Unmarshal(raw, &profile) != nil {
			return errors.New("proxy_profile_invalid")
		}
		proxy, err := url.Parse(profile.URL)
		if err != nil || (proxy.Scheme != "socks5" && proxy.Scheme != "socks5h") {
			return errors.New("proxy_scheme_invalid")
		}
		proxy.Host = bridge.Addr().String()
		h, err := startHost(proxyID, credential, proxy.String(), library)
		if err != nil {
			return err
		}
		hosts[proxyID-1] = h
		defer h.close()
	}
	emit(map[string]any{"event": "started_fast", "limit_per_proxy": proxyLimit, "concurrency": 2, "success_delay_seconds": 0, "prior_attempts": len(j.value.Attempts)})
	var wg sync.WaitGroup
	errorsChannel := make(chan error, 2)
	for proxyID := 1; proxyID <= 2; proxyID++ {
		wg.Add(1)
		go func(proxyID int) {
			defer wg.Done()
			if err := fastWorker(j, hosts[proxyID-1], proxyID, token); err != nil {
				j.stop("test_driver_error")
				errorsChannel <- err
			}
		}(proxyID)
	}
	wg.Wait()
	close(errorsChannel)
	j.mu.Lock()
	if j.value.StopReason == "running" {
		j.value.StopReason = fmt.Sprintf("%d_per_proxy_limit", proxyLimit)
	}
	if j.value.CredentialStatus == "access_token_not_expired_pending_live_check" {
		for _, row := range j.value.Attempts {
			if row.Completed {
				j.value.CredentialStatus = "usable_during_this_run_no_auth_rejection_observed"
				break
			}
		}
	}
	j.value.FinishedAt = time.Now().UTC()
	err = j.saveLocked()
	summary := map[string]any{"event": "finished_fast", "attempts": len(j.value.Attempts), "per_proxy": j.counts, "found_292": j.value.Found292, "stop_reason": j.value.StopReason, "credential_status": j.value.CredentialStatus}
	j.mu.Unlock()
	if err != nil {
		return err
	}
	emit(summary)
	for failure := range errorsChannel {
		return failure
	}
	return nil
}
func fastWorker(j *fastJournal, h *host, proxyID int, token string) error {
	failures := 0
	for {
		if j.ctx.Err() != nil {
			return nil
		}
		if accessExpired(token, time.Now()) {
			j.mu.Lock()
			j.value.CredentialStatus = "credential_expired"
			j.mu.Unlock()
			j.stop("credential_expired")
			return nil
		}
		if name, ok := find292(); ok {
			j.mu.Lock()
			j.value.Found292 = true
			j.value.Archive = name
			j.mu.Unlock()
			j.stop("292_preserved")
			return nil
		}
		before, err := h.snapshot()
		if err != nil {
			return err
		}
		row, slot, ok, err := j.reserve(proxyID)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		body := encoded(map[string]any{"model": model, "stream": false, "store": false, "reasoning": map[string]string{"effort": row.Effort}, "input": []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": "Reply with exactly OK."}}}}})
		status, response, err := h.callFast(j.ctx, "POST", "/v1/responses", body)
		row.HTTPStatus = status
		row.Error = ""
		issue := credentialIssue(status, response)
		if err != nil {
			row.Error = "request_failed"
			if j.ctx.Err() != nil {
				row.Error = "cancelled_after_stop"
			}
		} else {
			var value struct {
				Status    string `json:"status"`
				Model     string `json:"model"`
				Reasoning struct {
					Effort string `json:"effort"`
				} `json:"reasoning"`
			}
			if json.Unmarshal(response, &value) == nil {
				row.Completed = status == 200 && value.Status == "completed"
				row.ResponseModel = value.Model
				row.ResponseEffort = value.Reasoning.Effort
			}
			if !row.Completed {
				row.Error = "upstream_not_completed"
				row.ErrorClass = classifyFailure(response)
				dir := runDir + "/private-errors"
				if os.MkdirAll(dir, 0700) != nil {
					return errors.New("private_error_directory_failed")
				}
				if os.WriteFile(filepath.Join(dir, fmt.Sprintf("attempt-%03d.json", row.Number)), response, 0600) != nil {
					return errors.New("private_error_write_failed")
				}
			}
		}
		until := time.Now().Add(5 * time.Second)
		for status == 200 {
			snap, err := h.snapshot()
			if err != nil {
				return err
			}
			if len(snap.History) > len(before.History) {
				last := snap.History[len(snap.History)-1]
				row.StateLength = last.Length
				row.PluginSuccess = last.Success
				if last.Reasoning != row.Effort {
					row.Error = "plugin_effort_mismatch"
				}
				break
			}
			if time.Now().After(until) {
				row.Error = "plugin_completion_missing"
				break
			}
			<-time.After(20 * time.Millisecond)
		}
		row.Seconds = time.Since(row.Started).Seconds()
		archive, _ := find292()
		if err := j.finish(slot, row, issue, archive); err != nil {
			return err
		}
		emit(row)
		if issue != "" {
			emit(map[string]any{"event": "credential_alert", "proxy": proxyID, "number": row.Number, "status": issue})
			return nil
		}
		if j.ctx.Err() != nil {
			return nil
		}
		if status == 429 {
			j.stop("upstream_429")
			return nil
		}
		if row.ResponseEffort != "" && row.ResponseEffort != row.Effort {
			j.stop("response_effort_mismatch")
			return nil
		}
		if row.Completed && row.Error == "" {
			failures = 0
			continue
		}
		failures++
		delay := time.Duration(failures*5) * time.Second
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-j.ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
func (h *host) callFast(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.New("local_request_invalid")
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, errors.New("local_request_failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	return response.StatusCode, raw, err
}
