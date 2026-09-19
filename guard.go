package main

import (
	"net/http"
	"strconv"
	"time"
)

// Caller holds mu. Observation and dry-run modes do not stop business traffic.
func (state *runtimeState) blockWithoutStateLocked() bool {
	return state.config.BlockWithoutState && state.config.Mode == "force" && !state.config.DryRun
}

func usableGuardState(current storedState, policy credentialConfig, identity string, now time.Time, maxBytes int) bool {
	if identity == "" || current.Identity != identity || (len(current.Value) != 292 && len(current.Value) != 332) {
		return false
	}
	parsed, err := parseTurnState(current.Value, maxBytes)
	return err == nil && normalBlockCount(policy, parsed.Blocks) && parsed.Blocks == current.Blocks &&
		parsed.IssuedAt.Equal(current.IssuedAt) && !parsed.IssuedAt.After(now.Add(5*time.Minute)) && now.Before(parsed.IssuedAt.Add(turnStateTTL))
}

// Return an explicit host termination, not an interceptor error: host errors fail open.
// Probes use their own transport and remain independent of this business-request gate.
func (state *runtimeState) blockRequestLocked(req requestInterceptRequest, key, code string) ([]byte, error) {
	state.queueRefreshLocked(key, "missing_state")
	delete(state.requests, req.RequestID)
	delete(state.candidates, req.RequestID)
	state.recordLocked(key, 0, false, "blocked", requestReasoning(req.Body))
	entry := &state.history[len(state.history)-1]
	entry.RequestedModel = requestedModel(req)
	entry.HostOutcome = "blocked_before_upstream"
	retry := state.config.Probe.RetrySeconds
	if retry < 5 {
		retry = 5
	}
	body := jsonBytesForGuard(code)
	return okEnvelope(requestInterceptResponse{
		Terminate: true, StatusCode: http.StatusServiceUnavailable,
		ResponseHeaders: http.Header{
			"Content-Type": {"application/json"}, "Cache-Control": {"no-store"},
			"Retry-After": {strconv.Itoa(retry)},
		},
		ResponseBody: body,
	})
}

func jsonBytesForGuard(code string) []byte {
	// Both codes are fixed internal strings; no credential or state data is included.
	if code == "credential_identity_unavailable" {
		return []byte(`{"error":{"message":"Credential identity could not be verified. Request blocked before upstream. Recheck the selected credential.","type":"server_error","code":"credential_identity_unavailable"}}`)
	}
	return []byte(`{"error":{"message":"No valid cached 292/332 turn state is available for the selected credential and model. Request blocked before upstream; retry after acquisition succeeds.","type":"server_error","code":"valid_turn_state_required"}}`)
}
