package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRealCPAScheduleSettingsPersistAndOverrideLegacyYAML(t *testing.T) {
	h := startHost(t)
	type status struct {
		Mode     string `json:"mode"`
		Retry    int    `json:"retry_seconds"`
		Advance  int    `json:"refresh_before_seconds"`
		Schedule struct {
			Revision   uint64 `json:"revision"`
			Persistent bool   `json:"persistent"`
		} `json:"schedule"`
	}
	read := func() status {
		_, raw := h.call(t, "GET", "/v0/management/codex-turn-state-manager/status", nil, managementKey)
		var s status
		if json.Unmarshal(raw, &s) != nil {
			t.Fatal("invalid schedule status")
		}
		return s
	}
	s := read()
	if s.Retry != 7 || s.Advance != 1800 || !s.Schedule.Persistent {
		t.Fatalf("unexpected defaults: %+v", s)
	}
	body, _ := json.Marshal(map[string]any{"revision": s.Schedule.Revision, "advance_minutes": 10, "retry_seconds": 13})
	code, _ := h.call(t, "POST", "/v0/management/codex-turn-state-manager/schedule", body, "")
	if code != 401 {
		t.Fatal("schedule update accepted without authentication")
	}
	code, raw := h.call(t, "POST", "/v0/management/codex-turn-state-manager/schedule", body, managementKey)
	if code != 200 {
		t.Fatalf("save schedule: %d %s", code, raw)
	}
	s = read()
	if s.Retry != 13 || s.Advance != 600 {
		t.Fatal("schedule save did not change running values")
	}
	if _, err := os.Stat(filepath.Join(h.root, "current.json.schedule.json")); err != nil {
		t.Fatal("schedule file not persisted")
	}
	code, _ = h.call(t, "POST", "/v0/management/codex-turn-state-manager/schedule", body, managementKey)
	if code != 409 {
		t.Fatal("stale edit accepted")
	}
	patch := []byte(`{"mode":"force","probe":{"enabled":false,"retry_seconds":15,"refresh_before_seconds":300}}`)
	code, raw = h.call(t, "PATCH", "/v0/management/plugins/codex-turn-state-manager/config", patch, managementKey)
	if code != 200 {
		t.Fatalf("reconfigure: %d %s", code, raw)
	}
	await(t, func() bool { s = read(); return s.Mode == "force" })
	if s.Retry != 13 || s.Advance != 600 || s.Schedule.Revision != 1 {
		t.Fatal("legacy YAML overwrote saved UI settings after host reload")
	}
	t.Log("management auth, persistent schedule, stale revision rejection and host reconfigure precedence verified")
}
