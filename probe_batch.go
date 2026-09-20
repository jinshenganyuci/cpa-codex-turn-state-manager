package main

import (
	"context"
	"strings"
	"time"
)

const defaultProxyConcurrency = 3
const maxProxyConcurrency = 128

type probeRoute struct {
	endpoint proxyEndpoint
	ticket   poolTicket
}

type probeAttempt struct {
	route                  probeRoute
	value, status, outcome string
	responseModel, exitIP  string
	completedAt            time.Time
	candidate              storedState
}

func (state *runtimeState) releaseProbeSlots(auth string, slots int) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.credentialInFlight[auth] -= slots
	if state.credentialInFlight[auth] <= 0 {
		delete(state.credentialInFlight, auth)
	}
	select {
	case state.wake <- struct{}{}:
	default:
	}
}

// All models sharing a credential reserve from the same non-blocking limit.
// Each wave contains distinct pool entries and owns all of its child requests.
func (state *runtimeState) probeWave(task *probeTask, auth probeAuth) ([]probeAttempt, string) {
	state.mu.Lock()
	width := task.slots
	// A saved lower limit applies to subsequent waves of an existing task too.
	available := state.config.Probe.ProxyConcurrency - (state.credentialInFlight[task.authID] - task.slots)
	if available <= 0 {
		state.mu.Unlock()
		return nil, "credential_probe_limit"
	}
	width = min(width, available)
	if !task.config.Probe.Continuous {
		remaining := task.config.Probe.MaxPerHour - state.probeCounts[task.authID]
		if remaining < width {
			width = remaining
		}
	}
	if width <= 0 {
		state.mu.Unlock()
		return nil, "hourly_budget_reached"
	}
	routes, routeError := state.pool.routes(task.authID, auth.ProxyURL, width, state.now())
	if routeError != "" {
		state.mu.Unlock()
		return nil, routeError
	}
	state.probeCounts[task.authID] += len(routes)
	state.mu.Unlock()
	ctx, cancel := context.WithCancel(task.ctx)
	defer cancel()
	results := make(chan probeAttempt, len(routes))
	for _, route := range routes {
		go func(route probeRoute) {
			observed := &probeEgress{}
			fetchCtx := context.WithValue(ctx, probeEgressKey{}, observed)
			value, status, model := state.fetch(fetchCtx, auth, task.model, nil, route.endpoint)
			result := probeAttempt{route: route, value: value, status: status, outcome: status, responseModel: model, exitIP: observed.IP, completedAt: state.now()}
			parsed, err := parseTurnState(value, task.config.MaxStateBytes)
			if status == "ok" {
				switch {
				case enabledByDefault(task.config.RequireModelMatch) && !modelConsistent(task.model, model):
					result.outcome = "model_mismatch"
					if model == "" {
						result.outcome = "model_evidence_missing"
					}
				case err != nil || !normalBlockCount(task.policy, parsed.Blocks) || parsed.IssuedAt.After(state.now().Add(5*time.Minute)) || !state.now().Before(parsed.IssuedAt.Add(turnStateTTL)):
					result.outcome = "state_rejected"
				default:
					result.candidate = storedState{Value: value, IssuedAt: parsed.IssuedAt, Blocks: parsed.Blocks, Identity: identityHash(task.authID, auth.AccountID), ResponseModel: model, Source: "probe", CapturedAt: result.completedAt}
				}
			}
			// A fully completed response may arrive concurrently with a sibling's
			// success. Keep it eligible unless the entire task was cancelled.
			if task.ctx.Err() != nil || ctx.Err() != nil && status != "ok" {
				result.outcome, result.candidate = "cancelled", storedState{}
			}
			results <- result
		}(route)
	}
	out := make([]probeAttempt, 0, len(routes))
	for range routes {
		result := <-results
		if !state.pool.finish(result.route.ticket, result.outcome, len(result.value), state.now()) {
			result.outcome, result.candidate = "proxy_changed", storedState{}
		}
		out = append(out, result)
		if result.outcome == "ok" || credentialProbeFailure(result.outcome) {
			cancel()
		}
	}
	return out, summarizeProbeAttempts(out)
}

func summarizeProbeAttempts(results []probeAttempt) string {
	outcome := "cancelled"
	for _, result := range results {
		if result.outcome == "ok" {
			return "ok"
		}
		if credentialProbeFailure(result.outcome) {
			outcome = result.outcome
		} else if !credentialProbeFailure(outcome) && result.outcome != "cancelled" {
			outcome = result.outcome
		}
	}
	return outcome
}

func probeNeedsBackoff(outcome string) bool {
	return credentialProbeFailure(outcome) || strings.Contains(outcome, "insufficient_quota")
}
