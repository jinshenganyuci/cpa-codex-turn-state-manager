package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestEgressTraceUsesOriginalConnectionWithoutOAuth(t *testing.T) {
	for _, mode := range []string{"success", "closed", "lost", "redirect", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			var calls, traces atomic.Int32
			var remote string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/backend-api/codex/responses" {
					remote = r.RemoteAddr
					if r.Header.Get("Authorization") != "Bearer synthetic" {
						t.Error("primary authorization lost")
					}
					if mode == "closed" {
						w.Header().Set("Connection", "close")
					}
					_, _ = io.WriteString(w, "completed")
					return
				}
				traces.Add(1)
				if r.URL.Path != "/cdn-cgi/trace" || r.RemoteAddr != remote {
					t.Error("trace did not use the original connection and target")
				}
				for _, header := range []string{"Authorization", "ChatGPT-Account-ID", "Proxy-Authorization", "X-Codex-Turn-State"} {
					if r.Header.Get(header) != "" {
						t.Errorf("trace leaked %s", header)
					}
				}
				if mode == "redirect" {
					http.Redirect(w, r, "https://elsewhere.invalid/trace", 302)
					return
				}
				if mode == "unavailable" {
					w.WriteHeader(503)
					return
				}
				_, _ = io.WriteString(w, "h=chatgpt.com\nip=203.0.113.24\nloc=IL\n")
			}))
			defer server.Close()
			var tunnels atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tunnels.Add(1)
				if r.Method != http.MethodConnect || r.Host != "chatgpt.com:443" {
					t.Error("unexpected tunnel target")
					w.WriteHeader(502)
					return
				}
				upstream, err := net.Dial("tcp", server.Listener.Addr().String())
				if err != nil {
					w.WriteHeader(502)
					return
				}
				defer func() { _ = upstream.Close() }()
				conn, buf, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				_, _ = buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
				_ = buf.Flush()
				go func() { _, _ = io.Copy(upstream, conn); _ = upstream.Close() }()
				_, _ = io.Copy(conn, upstream)
			}))
			defer proxy.Close()
			ctx := context.Background()
			client := newProbeHTTPClient(ctx, nil, proxyEndpoint{URL: proxy.URL})
			defer client.transport.CloseIdleConnections()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			client.transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12}
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader("ping"))
			req.Header.Set("Authorization", "Bearer synthetic")
			req.Header.Set("ChatGPT-Account-ID", "synthetic-account")
			req.Header.Set(turnStateHeader, "synthetic-state")
			resp, err := client.do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			if string(body) != "completed" {
				t.Fatal("primary response changed")
			}
			if mode == "lost" {
				server.CloseClientConnections()
			}
			ip := client.finish(ctx, resp)
			if mode == "success" && ip != "203.0.113.24" || mode != "success" && ip != "" {
				t.Fatalf("unexpected address %q", ip)
			}
			if tunnels.Load() != 1 {
				t.Fatal("IP lookup opened another proxy connection")
			}
			if mode == "closed" || mode == "lost" {
				if traces.Load() != 0 || calls.Load() != 1 {
					t.Fatal("closed connection produced a trace")
				}
			} else if traces.Load() != 1 || calls.Load() != 2 {
				t.Fatal("unexpected trace request count")
			}
		})
	}
}

func TestEgressTraceRejectsUnverifiedAddresses(t *testing.T) {
	for _, raw := range []string{
		"h=chatgpt.com\nip=<script>alert(1)</script>\n",
		"h=other.invalid\nip=203.0.113.24\n",
		"h=chatgpt.com\nip=127.0.0.1\n",
		"h=chatgpt.com\nip=10.1.2.3\n",
		"h=chatgpt.com\nip=::1\n",
		"h=chatgpt.com\nip=203.0.113.24\nip=203.0.113.25\n",
	} {
		if parseEgressTrace(raw) != "" {
			t.Fatal("unverified address accepted")
		}
	}
	if parseEgressTrace("h=chatgpt.com\nip=2001:db8::24\n") != "2001:db8::24" {
		t.Fatal("IPv6 address rejected")
	}
}
