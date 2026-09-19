package main

import (
	"strings"
	"time"
)

// Exact executed-model matching avoids treating another family as equivalent.
func modelConsistent(expected, reported string) bool {
	return strings.TrimSpace(reported) != "" && strings.EqualFold(strings.TrimSpace(expected), strings.TrimSpace(reported))
}

func (state *runtimeState) modelRejectionLocked(model, reported string) string {
	if !enabledByDefault(state.config.RequireModelMatch) {
		return ""
	}
	if strings.TrimSpace(reported) == "" {
		return "model_evidence_missing"
	}
	if !modelConsistent(model, reported) {
		return "model_mismatch"
	}
	return ""
}

func (state *runtimeState) usableSlotLocked(key string, value storedState) bool {
	auth, model := splitStateKey(key)
	return state.identitySelectedLocked(auth, model, value.Identity) && state.storedSlotValidLocked(key, value)
}

func (state *runtimeState) storedSlotValidLocked(key string, value storedState) bool {
	auth, model := splitStateKey(key)
	policy, ok := credentialFor(state.config, auth)
	return ok && matchesModels(policy.Models, model) &&
		usableGuardState(value, policy, value.Identity, state.now(), state.config.MaxStateBytes) &&
		state.modelRejectionLocked(model, value.ResponseModel) == ""
}

// A preferred length wins over recency; within one length, keep the newer issuance.
func (state *runtimeState) betterSlotLocked(candidate, old storedState) bool {
	if candidate.Value == old.Value {
		return false
	}
	if old.Value == "" {
		return true
	}
	if enabledByDefault(state.config.Prefer292) && len(candidate.Value) != len(old.Value) {
		return len(candidate.Value) == 292
	}
	return !candidate.IssuedAt.Before(old.IssuedAt)
}

func (state *runtimeState) promoteStandbyLocked(key string) bool {
	active, standby := state.current[key], state.standby[key]
	if !state.usableSlotLocked(key, standby) {
		return false
	}
	activeValid := state.usableSlotLocked(key, active)
	upgrade := enabledByDefault(state.config.Prefer292) && len(standby.Value) == 292 && len(active.Value) == 332
	if activeValid && !upgrade {
		return false
	}
	state.current[key] = standby
	delete(state.standby, key)
	if upgrade && activeValid && active.IssuedAt.After(standby.IssuedAt) {
		state.standby[key] = active
	}
	state.journalRevision++
	state.slotsRevision++
	return true
}

// Store only accepted full responses. Echoed values never renew their issuance or provenance.
func (state *runtimeState) acceptStateLocked(key string, candidate storedState) (action string) {
	defer func() {
		if acceptedSlotAction(action) {
			state.slotsRevision++
		}
	}()
	if !state.usableSlotLocked(key, candidate) {
		return "rejected"
	}
	state.promoteStandbyLocked(key)
	active := state.current[key]
	if candidate.Value == active.Value || candidate.Value == state.standby[key].Value {
		return "duplicate"
	}
	if !state.usableSlotLocked(key, active) {
		state.current[key] = candidate
		delete(state.standby, key)
		return "active"
	}
	upgrade := enabledByDefault(state.config.Prefer292) && len(candidate.Value) == 292 && len(active.Value) == 332
	if upgrade {
		state.current[key] = candidate
		if state.betterSlotLocked(active, state.standby[key]) {
			state.standby[key] = active
		}
		return "upgraded_292"
	}
	if !enabledByDefault(state.config.StandbyEnabled) {
		if state.betterSlotLocked(candidate, active) {
			state.current[key] = candidate
			return "active"
		}
		return "older"
	}
	// An older successor cannot extend coverage of the same or preferred active length.
	if candidate.IssuedAt.Before(active.IssuedAt) {
		return "older"
	}
	old := state.standby[key]
	if !state.usableSlotLocked(key, old) || state.betterSlotLocked(candidate, old) {
		state.standby[key] = candidate
		return "standby"
	}
	return "lower_priority"
}

func acceptedSlotAction(action string) bool {
	return action == "active" || action == "upgraded_292" || action == "standby"
}

func (state *runtimeState) acquisitionDueLocked(key string) time.Time {
	state.promoteStandbyLocked(key)
	active := state.current[key]
	if state.refreshRequests[key] != "" || !state.usableSlotLocked(key, active) {
		return time.Time{}
	}
	if enabledByDefault(state.config.Prefer292) && len(active.Value) == 332 {
		return time.Time{}
	}
	due := active.IssuedAt.Add(turnStateTTL - time.Duration(state.config.Probe.RefreshBeforeSeconds)*time.Second)
	standby := state.standby[key]
	if enabledByDefault(state.config.StandbyEnabled) && state.usableSlotLocked(key, standby) && standby.IssuedAt.After(active.IssuedAt) &&
		(!enabledByDefault(state.config.Prefer292) || len(standby.Value) == 292) {
		due = standby.IssuedAt.Add(turnStateTTL - time.Duration(state.config.Probe.RefreshBeforeSeconds)*time.Second)
	}
	return due
}

func (state *runtimeState) invalidateUsedStateLocked(binding requestBinding) bool {
	if binding.UsedStateDigest == "" {
		return false
	}
	changed := false
	if value := state.current[binding.Key]; value.Identity == binding.Identity && digest(value.Value) == binding.UsedStateDigest {
		delete(state.current, binding.Key)
		changed = true
	}
	if value := state.standby[binding.Key]; value.Identity == binding.Identity && digest(value.Value) == binding.UsedStateDigest {
		delete(state.standby, binding.Key)
		changed = true
	}
	if changed {
		state.slotsRevision++
		state.promoteStandbyLocked(binding.Key)
	}
	return changed
}
