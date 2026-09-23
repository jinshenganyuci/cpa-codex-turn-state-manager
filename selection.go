package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type selectionPolicy struct {
	Version  int               `json:"version"`
	Revision uint64            `json:"revision"`
	Accounts map[string]string `json:"accounts"`
	Models   []string          `json:"models"`
}

func emptySelection() selectionPolicy {
	return selectionPolicy{Version: 1, Accounts: make(map[string]string), Models: []string{}}
}

func loadSelection(path string) (selectionPolicy, error) {
	value := emptySelection()
	if path == "" {
		return value, nil
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return value, nil
	}
	if err != nil {
		return value, errors.New("selection_file could not be opened")
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 || json.Unmarshal(raw, &value) != nil || value.Version != 1 {
		return emptySelection(), errors.New("selection_file is invalid")
	}
	if len(value.Accounts) > 4096 || len(value.Models) > 256 {
		return emptySelection(), errors.New("selection_file exceeds limits")
	}
	for auth, identity := range value.Accounts {
		if auth == "" || len(identity) != 64 {
			return emptySelection(), errors.New("selection_file has invalid credential identity")
		}
	}
	if value.Accounts == nil {
		value.Accounts = make(map[string]string)
	}
	value.Models = canonicalModels(value.Models)
	return value, nil
}

func persistSelection(path string, value selectionPolicy) error {
	if path == "" {
		return errors.New("selection_file is not configured")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("selection directory could not be created")
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return errors.New("selection could not be encoded")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".turn-state-selection-*.tmp")
	if err != nil {
		return errors.New("selection temporary file could not be created")
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(raw, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	errClose := f.Close()
	if err != nil || errClose != nil {
		return errors.New("selection could not be saved")
	}
	if os.Rename(f.Name(), path) != nil {
		return errors.New("selection could not be committed")
	}
	return nil
}

// Caller holds mu. Selection is independent of transient probe budget/state.
func (state *runtimeState) pairSelectedLocked(auth, model string) bool {
	if !state.config.SelectionRequired {
		return true
	}
	if state.selection.Accounts[auth] == "" {
		return false
	}
	for _, selected := range state.selection.Models {
		if selected == model {
			return true
		}
	}
	return false
}

func (state *runtimeState) identitySelectedLocked(auth, model, identity string) bool {
	return state.pairSelectedLocked(auth, model) && (!state.config.SelectionRequired || state.selection.Accounts[auth] == identity)
}

func (state *runtimeState) acquisitionModelsLocked(policy credentialConfig) []string {
	if state.config.SelectionRequired {
		var models []string
		for _, model := range state.selection.Models {
			if matchesModels(policy.Models, model) {
				models = append(models, model)
			}
		}
		return models
	}
	return policy.Models
}

func modelOptions(cfg pluginConfig) []string {
	models := []string{"gpt-6-astra", "gpt-6-sol", "gpt-5.6-sol"}
	if cfg.Defaults != nil {
		models = append(models, cfg.Defaults.Models...)
	}
	for _, policy := range cfg.Credentials {
		models = append(models, policy.Models...)
	}
	var exact []string
	for _, model := range canonicalModels(models) {
		if !strings.Contains(model, "*") {
			exact = append(exact, model)
		}
	}
	return exact
}

func (state *runtimeState) selectionViewLocked(listing authListing) map[string]any {
	accounts := make([]map[string]any, 0)
	for _, auth := range listing.Files {
		if auth.Provider != "codex" || auth.AccountType == "api_key" {
			continue
		}
		accounts = append(accounts, map[string]any{"account": digest(auth.ID)[:12], "label": filepath.Base(auth.ID), "email": auth.Email, "plan_type": auth.PlanType, "disabled": auth.Disabled, "selected": state.selection.Accounts[auth.ID] != ""})
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i]["account"].(string) < accounts[j]["account"].(string) })
	models := append([]string{}, state.selection.Models...)
	return map[string]any{"required": state.config.SelectionRequired, "persistent": state.config.SelectionFile != "", "revision": state.selection.Revision, "models": models, "model_options": modelOptions(state.config), "accounts": accounts}
}

func (state *runtimeState) updateSelection(raw []byte) ([]byte, error) {
	var body struct {
		Revision uint64   `json:"revision"`
		Accounts []string `json:"accounts"`
		Models   []string `json:"models"`
	}
	if len(raw) > 64<<10 || json.Unmarshal(raw, &body) != nil || len(body.Accounts) > 4096 || len(body.Models) > 256 {
		return managementJSON(400, map[string]string{"error": "invalid_selection"})
	}
	state.selectionMu.Lock()
	defer state.selectionMu.Unlock()
	state.mu.Lock()
	cfg := state.config
	revision := state.selection.Revision
	state.mu.Unlock()
	if !cfg.SelectionRequired || cfg.SelectionFile == "" {
		return managementJSON(400, map[string]string{"error": "selection_persistence_not_enabled"})
	}
	if body.Revision != revision {
		return managementJSON(409, map[string]string{"error": "selection_changed_reload_before_saving"})
	}
	allowed := make(map[string]bool)
	for _, model := range modelOptions(cfg) {
		allowed[model] = true
	}
	models := canonicalModels(body.Models)
	for _, model := range models {
		if !allowed[model] || strings.Contains(model, "*") {
			return managementJSON(400, map[string]string{"error": "model_not_available"})
		}
	}
	listing, err := state.listDisplayAuths()
	if err != nil {
		return managementJSON(503, map[string]string{"error": "credentials_unavailable"})
	}
	ids := make(map[string]string)
	for _, auth := range listing.Files {
		if auth.Provider != "codex" || auth.Disabled || auth.AccountType == "api_key" {
			continue
		}
		key := digest(auth.ID)[:12]
		if _, exists := ids[key]; exists {
			return managementJSON(409, map[string]string{"error": "ambiguous_credential_fingerprint"})
		}
		ids[key] = auth.ID
	}
	next := emptySelection()
	next.Revision = revision + 1
	next.Models = models
	for _, account := range body.Accounts {
		auth, ok := ids[account]
		if !ok {
			return managementJSON(400, map[string]string{"error": "credential_not_available"})
		}
		policy, ok := credentialFor(cfg, auth)
		if !ok || !autoUpdateEnabled(cfg, policy) {
			return managementJSON(400, map[string]string{"error": "credential_not_managed"})
		}
		for _, model := range models {
			if !matchesModels(policy.Models, model) {
				return managementJSON(400, map[string]string{"error": "model_out_of_credential_scope"})
			}
		}
		identity, err := state.selectedIdentity(auth)
		if err != nil {
			return managementJSON(409, map[string]string{"error": "credential_changed_reload_before_saving"})
		}
		next.Accounts[auth] = identity
	}
	if persistSelection(cfg.SelectionFile, next) != nil {
		return managementJSON(500, map[string]string{"error": "selection_save_failed"})
	}
	state.mu.Lock()
	state.selection = next
	for key := range state.refreshRequests {
		auth, model := splitStateKey(key)
		if !state.pairSelectedLocked(auth, model) {
			delete(state.refreshRequests, key)
		}
	}
	for id, binding := range state.requests {
		_, model := splitStateKey(binding.Key)
		if !state.identitySelectedLocked(binding.AuthID, model, binding.Identity) {
			delete(state.requests, id)
			delete(state.candidates, id)
		}
	}
	for key := range state.probing {
		auth, model := splitStateKey(key)
		if !state.pairSelectedLocked(auth, model) && state.activeCancels[key] != nil {
			state.activeCancels[key]()
		}
	}
	select {
	case state.wake <- struct{}{}:
	default:
	}
	view := state.selectionViewLocked(listing)
	state.mu.Unlock()
	return managementJSON(200, map[string]any{"ok": true, "selection": view})
}
