package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func displayJWT(email, plan string) string {
	return "e30." + base64.RawURLEncoding.EncodeToString(jsonBytes(map[string]any{"email": email, "https://api.openai.com/auth": map[string]string{"chatgpt_plan_type": plan}})) + ".signature"
}

func TestCredentialDisplayProjection(t *testing.T) {
	for _, plan := range []string{"pro", "plus", "free", "team", "business", "enterprise"} {
		metadata := credentialMetadata{IDToken: displayJWT("display@example.test", plan), AccessToken: "private-access-token"}
		got := credentialPresentation("file.json", "", metadata)
		if got.Email != "display@example.test" || got.PlanType != plan || got.Label != "file.json" {
			t.Fatalf("metadata projection failed for %s", plan)
		}
		raw := string(jsonBytes(got))
		if strings.Contains(raw, metadata.IDToken) || strings.Contains(raw, metadata.AccessToken) {
			t.Fatal("secret leaked in display DTO")
		}
	}
	got := credentialPresentation("someone-pro.json", "", credentialMetadata{IDToken: "invalid"})
	if got.PlanType != "" || got.Email != "" {
		t.Fatal("guessed metadata from filename or malformed JWT")
	}
	got = credentialPresentation("file.json", "fallback@example.test", credentialMetadata{PlanType: " Plus ", IDToken: displayJWT("claim@example.test", "free")})
	if got.Email != "fallback@example.test" || got.PlanType != "plus" {
		t.Fatal("explicit metadata precedence changed")
	}
}

func TestResponseModelEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		upstream         bool
	}{
		{"terminal", `{"type":"response.completed","response":{"model":"gpt-5.6-luna","status":"completed"}}`, "gpt-5.6-luna", false},
		{"json", `{"object":"response","model":"gpt-5.6-luna","status":"completed"}`, "gpt-5.6-luna", false},
		{"chat_json", `{"object":"chat.completion","model":"gpt-5.6-luna"}`, "gpt-5.6-luna", false},
		{"injected_initial", `{"type":"response.created","response":{"model":"gpt-6-astra"}}`, "", false},
		{"translated_chunk", `{"object":"chat.completion.chunk","model":"gpt-6-astra"}`, "", false},
		{"raw_ws", `{"type":"response.created","response":{"model":"gpt-5.6-luna"}}`, "gpt-5.6-luna", true},
		{"missing", `{"type":"response.completed","response":{"status":"completed"}}`, "", false},
		{"assistant_text", `{"type":"response.output_text.delta","delta":"model: gpt-5.6-luna"}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var binding requestBinding
			observeModel(&binding, []byte(tc.body), tc.upstream)
			if binding.ResponseModel != tc.want {
				t.Fatalf("got %q, want %q", binding.ResponseModel, tc.want)
			}
		})
	}
	var binding requestBinding
	observeModel(&binding, []byte(`{"type":"response.completed","response":{"model":"gpt-5.6-luna"}}`), true)
	observeModel(&binding, []byte(`{"object":"response","model":"gpt-6-astra"}`), false)
	if binding.ResponseModel != "gpt-5.6-luna" {
		t.Fatal("translated response overwrote raw upstream evidence")
	}
}

func TestHistoryKeepsRequestedResponseAndCredentialSnapshot(t *testing.T) {
	state := testRuntime(t, "")
	email := "before@example.test"
	base := state.hostCall
	state.hostCall = func(method string, request, result any) error {
		if method == "host.auth.get" {
			return json.Unmarshal(jsonBytes(map[string]any{"json": map[string]any{"account_id": "account-a", "email": email, "id_token": displayJWT(email, "team")}}), result)
		}
		return base(method, request, result)
	}
	req := requestInterceptRequest{RequestID: "display", ToFormat: "codex", Model: "gpt-6-astra", RequestedModel: "my-astra-alias", Metadata: map[string]any{"selected_auth_id": "auth-a"}}
	if _, err := state.interceptAfter(jsonBytes(req)); err != nil {
		t.Fatal(err)
	}
	state.observeStreamFailure("display", []byte("data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.6-luna\","))
	state.observeStreamFailure("display", []byte("\"status\":\"completed\"}}\n\n"))
	email = "after@example.test"
	finish(t, state, "display", "succeeded", false)
	row := state.history[0]
	if row.RequestedModel != "my-astra-alias" || row.Model != "gpt-6-astra" || row.ResponseModel != "gpt-5.6-luna" || row.Email != "before@example.test" || row.PlanType != "team" {
		t.Fatalf("history lost request metadata: %+v", row)
	}
	begin(t, state, "retry", "auth-a", "gpt-6-astra", "")
	state.observeBody("retry", []byte(`{"object":"response","model":"gpt-5.6-luna"}`))
	begin(t, state, "retry", "auth-b", "gpt-6-astra", "")
	finish(t, state, "retry", "succeeded", true)
	if state.history[len(state.history)-1].ResponseModel != "" {
		t.Fatal("retry retained a previous attempt's response model")
	}
}

func TestProbeReportsModelEvenWithoutState(t *testing.T) {
	body := "data: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-6-astra\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.6-luna\",\"status\":\"completed\"}}\n\n"
	completed, model := probeResponse(strings.NewReader(body))
	if !completed || model != "gpt-5.6-luna" {
		t.Fatal("probe lost terminal model")
	}
	state := testRuntime(t, "probe:\n  enabled: true\n  background_refresh: false\n")
	state.fetch = func(context.Context, probeAuth, string, *proxyEndpoint, proxyEndpoint) (string, string, string) {
		return "", "state_missing", model
	}
	state.ensureProbe("auth-a", "gpt-6-astra")
	if len(state.history) != 1 || state.history[0].ResponseModel != model || state.history[0].RequestedModel != "gpt-6-astra" {
		t.Fatal("probe history omitted the response model")
	}
}
