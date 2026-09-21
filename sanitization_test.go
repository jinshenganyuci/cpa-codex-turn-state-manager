package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestInboundClearHeadersAndOutboundSanitization(t *testing.T) {
	state := testRuntime(t, "mode: force\nrequire_model_match: false\n")
	token292 := makeFernetToken(t, state.now(), 10) // length 292
	token312 := makeFernetToken(t, state.now(), 11) // length 312

	seed(state, token292)

	// 1. 客户端发送 312 请求头，插件应返回 ClearHeaders 且注入 292
	res := begin(t, state, "req-with-312", "auth-a", "gpt-6-astra", token312)
	if len(res.ClearHeaders) == 0 || res.ClearHeaders[0] != turnStateHeader {
		t.Fatalf("expected ClearHeaders containing %s, got %v", turnStateHeader, res.ClearHeaders)
	}
	if got := headerValue(res.Headers, turnStateHeader); got != token292 {
		t.Fatalf("expected injected 292, got %s", got)
	}

	// 2. 上游返回 312 时，出站响应拦截应将 312 重写为缓存的 292
	outboundRaw, err := state.interceptResponse(jsonBytes(responseInterceptRequest{
		RequestID:       "req-with-312",
		ResponseHeaders: http.Header{turnStateHeader: {token312}},
	}))
	if err != nil {
		t.Fatalf("interceptResponse failed: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(outboundRaw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var outResp responseInterceptResponse
	if err := json.Unmarshal(env.Result, &outResp); err != nil {
		t.Fatalf("unmarshal responseInterceptResponse: %v", err)
	}
	if len(outResp.ClearHeaders) == 0 {
		t.Fatalf("expected ClearHeaders on outbound 312")
	}
	if got := headerValue(outResp.Headers, turnStateHeader); got != token292 {
		t.Fatalf("expected outbound 312 to be replaced with 292, got %s", got)
	}

	// 3. 当没有可用 292 缓存时，入站客户端 312 依然会被放入 ClearHeaders 剥除
	delete(state.current, stateKey("auth-a", "gpt-6-astra"))
	resNoCache := begin(t, state, "req-nocache-312", "auth-a", "gpt-6-astra", token312)
	if len(resNoCache.ClearHeaders) == 0 {
		t.Fatalf("expected ClearHeaders for inbound 312 even without cache")
	}

	// 出站上游 312 也应被 ClearHeaders 剥离，且不注入任何 Headers
	outboundRawNoCache, err := state.interceptResponse(jsonBytes(responseInterceptRequest{
		RequestID:       "req-nocache-312",
		ResponseHeaders: http.Header{turnStateHeader: {token312}},
	}))
	if err != nil {
		t.Fatalf("interceptResponse failed: %v", err)
	}
	var envNoCache envelope
	if err := json.Unmarshal(outboundRawNoCache, &envNoCache); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var outRespNoCache responseInterceptResponse
	if err := json.Unmarshal(envNoCache.Result, &outRespNoCache); err != nil {
		t.Fatalf("unmarshal responseInterceptResponse: %v", err)
	}
	if len(outRespNoCache.ClearHeaders) == 0 {
		t.Fatalf("expected ClearHeaders on outbound 312 without cache")
	}
	if len(outRespNoCache.Headers) != 0 {
		t.Fatalf("expected no Headers injected when no 292 available, got %v", outRespNoCache.Headers)
	}
}
