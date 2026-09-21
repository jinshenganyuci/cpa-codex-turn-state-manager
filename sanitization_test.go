package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func decodeResponseIntercept(t *testing.T, raw []byte) responseInterceptResponse {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var out responseInterceptResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("unmarshal responseInterceptResponse: %v", err)
	}
	return out
}

func mustInterceptResponse(t *testing.T, state *runtimeState, requestID, token string) []byte {
	t.Helper()
	raw, err := state.interceptResponse(jsonBytes(responseInterceptRequest{
		RequestID:       requestID,
		ResponseHeaders: http.Header{turnStateHeader: {token}},
	}))
	if err != nil {
		t.Fatalf("interceptResponse failed: %v", err)
	}
	return raw
}

func TestOutboundTurnStateSanitization(t *testing.T) {
	state := testRuntime(t, "mode: force\nrequire_model_match: false\n")
	token292 := makeFernetToken(t, state.now(), 10) // length 292
	token312 := makeFernetToken(t, state.now(), 11) // length 312
	token332 := makeFernetToken(t, state.now(), 12) // length 332

	seed(state, token292)

	// Inbound 312 with a usable cache: force mode replaces it with the cached 292, as before.
	res := begin(t, state, "req-with-312", "auth-a", "gpt-6-astra", token312)
	if got := headerValue(res.Headers, turnStateHeader); got != token292 {
		t.Fatalf("expected injected 292, got %s", got)
	}

	// Upstream 312 is stripped from the client response; the private cache is never emitted.
	out := decodeResponseIntercept(t, mustInterceptResponse(t, state, "req-with-312", token312))
	if len(out.ClearHeaders) != 1 || out.ClearHeaders[0] != turnStateHeader {
		t.Fatalf("expected ClearHeaders [%s], got %v", turnStateHeader, out.ClearHeaders)
	}

	// Accepted lengths pass through untouched.
	for _, token := range []string{token292, token332} {
		if out := decodeResponseIntercept(t, mustInterceptResponse(t, state, "req-with-312", token)); len(out.ClearHeaders) != 0 {
			t.Fatalf("accepted length %d unexpectedly cleared", len(token))
		}
	}

	// Without a usable cache the inbound request passes through unchanged (original contract),
	// while a degraded upstream response header is still stripped.
	delete(state.current, stateKey("auth-a", "gpt-6-astra"))
	resNoCache := begin(t, state, "req-nocache-312", "auth-a", "gpt-6-astra", token312)
	if len(resNoCache.Headers) != 0 || len(resNoCache.ClearHeaders) != 0 {
		t.Fatalf("request unexpectedly mutated without cache: %+v", resNoCache)
	}
	if out := decodeResponseIntercept(t, mustInterceptResponse(t, state, "req-nocache-312", token312)); len(out.ClearHeaders) == 0 {
		t.Fatalf("expected outbound 312 to be stripped without cache")
	}
}
