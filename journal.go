package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

type runtimeJournal struct {
	Version      int                  `json:"version"`
	History      []observation        `json:"history"`
	LastProbe    map[string]time.Time `json:"last_probe"`
	ProbeResults map[string]string    `json:"probe_results"`
	BlockedUntil map[string]time.Time `json:"blocked_until"`
	ProbeCounts  map[string]int       `json:"probe_counts"`
	BudgetStart  time.Time            `json:"budget_start"`
}

func readPrivateJSON(path string, limit int64, out any) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("private state file unavailable")
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return errors.New("private state file exceeds limits")
	}
	if json.Unmarshal(raw, out) != nil {
		return errors.New("private state file is invalid")
	}
	return nil
}

func loadJournal(path string) (runtimeJournal, error) {
	j := runtimeJournal{Version: 1}
	if err := readPrivateJSON(path, 4<<20, &j); err != nil {
		return j, err
	}
	if j.Version != 1 || len(j.History) > 200 || len(j.LastProbe) > 4096 || len(j.BlockedUntil) > 4096 || len(j.ProbeResults) > 4096 || len(j.ProbeCounts) > 4096 {
		return j, errors.New("unsupported runtime journal")
	}
	return j, nil
}

func cloneMap[V any](in map[string]V) map[string]V {
	out := make(map[string]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (state *runtimeState) journalSnapshotLocked() runtimeJournal {
	return runtimeJournal{Version: 1, History: append([]observation{}, state.history...), LastProbe: cloneMap(state.lastProbe), ProbeResults: cloneMap(state.probeResults), BlockedUntil: cloneMap(state.blockedUntil), ProbeCounts: cloneMap(state.probeCounts), BudgetStart: state.budgetStart}
}

func (state *runtimeState) persistRuntime() error {
	state.journalMu.Lock()
	defer state.journalMu.Unlock()
	state.mu.Lock()
	path, revision := state.config.RuntimeFile, state.journalRevision
	if path == "" || revision == state.journalSaved {
		state.mu.Unlock()
		return nil
	}
	j := state.journalSnapshotLocked()
	state.mu.Unlock()
	err := writePrivateJSON(path, j)
	state.mu.Lock()
	defer state.mu.Unlock()
	if err != nil {
		state.persistenceError = "runtime_save_failed"
		return err
	}
	state.persistenceError = ""
	state.journalSaved = revision
	return nil
}

func (state *runtimeState) restoreJournalLocked(path string, j runtimeJournal) {
	if state.runtimeLoaded == path {
		return
	}
	state.runtimeLoaded = path
	state.history = append([]observation{}, j.History...)
	state.lastProbe = cloneMap(j.LastProbe)
	state.probeResults = cloneMap(j.ProbeResults)
	for key, outcome := range state.probeResults {
		if outcome == "proxies_cooling_or_disabled" {
			delete(state.probeResults, key)
		}
	}
	state.blockedUntil = cloneMap(j.BlockedUntil)
	state.probeCounts = cloneMap(j.ProbeCounts)
	state.budgetStart = j.BudgetStart
	state.journalRevision++
	state.journalSaved = 0
}

func loadStandby(path string, maxBytes int) (map[string]storedState, error) {
	var bundle persistedFile
	if err := readPrivateJSON(path, maxStateFileBytes, &bundle); err != nil {
		return nil, err
	}
	out := make(map[string]storedState)
	for key, value := range bundle.Standby {
		parsed, err := parseTurnState(value.Value, maxBytes)
		if err != nil {
			continue
		}
		value.IssuedAt = parsed.IssuedAt
		value.Blocks = parsed.Blocks
		out[key] = value
	}
	return out, nil
}

func (state *runtimeState) flushPersistence() {
	_ = state.persistRuntime()
	state.mu.Lock()
	path, dirty := state.config.StateFile, state.slotsRevision != state.slotsSaved
	state.mu.Unlock()
	if path != "" && dirty {
		if state.persistCurrent(path) != nil {
			state.mu.Lock()
			state.persistenceError = "state_save_failed"
			state.mu.Unlock()
		}
	}
}
