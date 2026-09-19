package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentProxyBudgets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j := &fastJournal{ctx: ctx, cancel: cancel, path: filepath.Join(t.TempDir(), "report.json"), value: fastReport{report: report{StopReason: "running"}}}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(proxy int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_, _, ok, err := j.reserve(proxy)
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					return
				}
			}
		}(1 + worker%2)
	}
	wg.Wait()
	if len(j.value.Attempts) != 100 || j.counts != [2]int{50, 50} {
		t.Fatal("concurrent cap failed")
	}
	counts := map[string]int{}
	for _, row := range j.value.Attempts {
		counts[row.Effort]++
	}
	if counts["high"] != 50 || counts["medium"] != 50 {
		t.Fatal("effort budget failed")
	}
}
func TestCredentialRejectionStopsBothQueues(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j := &fastJournal{ctx: ctx, cancel: cancel, path: filepath.Join(t.TempDir(), "report.json"), value: fastReport{report: report{StopReason: "running"}}}
	row, slot, _, _ := j.reserve(1)
	if err := j.finish(slot, row, "credential_expired", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := j.reserve(2); ok {
		t.Fatal("new request after credential rejection")
	}
	cases := []struct {
		status     int
		body, want string
	}{{401, `{}`, "authentication_rejected_401"}, {403, `{"error":{"code":"permission_denied"}}`, "access_denied_403_not_proof_of_expiry"}, {503, `{"error":{"message":"auth_unavailable: last upstream error token_expired"}}`, "credential_expired"}, {503, `{"error":{"message":"auth_unavailable: server_is_overloaded"}}`, ""}, {429, `{"error":{"code":"rate_limit_exceeded"}}`, ""}}
	for _, c := range cases {
		if got := credentialIssue(c.status, []byte(c.body)); got != c.want {
			t.Errorf("status %d: got %q want %q", c.status, got, c.want)
		}
	}
}

func TestTenPerProxyCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j := &fastJournal{ctx: ctx, cancel: cancel, path: filepath.Join(t.TempDir(), "report.json"), value: fastReport{report: report{StopReason: "running", Limit: 20}, LimitPerProxy: 10}}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(proxy int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_, _, ok, err := j.reserve(proxy)
				if err != nil {
					t.Error(err)
					return
				}
				if !ok {
					return
				}
			}
		}(1 + worker%2)
	}
	wg.Wait()
	if len(j.value.Attempts) != 20 || j.counts != [2]int{10, 10} {
		t.Fatal("ten-per-proxy cap failed")
	}
	for proxy := 1; proxy <= 2; proxy++ {
		counts := map[string]int{}
		for _, row := range j.value.Attempts {
			if row.Proxy == proxy {
				counts[row.Effort]++
			}
		}
		if counts["high"] != 5 || counts["medium"] != 5 {
			t.Fatal("per-proxy effort counts failed")
		}
	}
}
