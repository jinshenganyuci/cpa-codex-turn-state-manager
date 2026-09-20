package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
)

type probeEgressKey struct{}
type probeEgress struct{ IP string }

// One transport belongs to one acquisition. The trace must reuse its exact TLS
// connection: a new connection through a rotating gateway can have another IP.
type probeHTTPClient struct {
	client    *http.Client
	transport *http.Transport
	noDial    atomic.Bool
	mu        sync.Mutex
	conn      net.Conn
}

func newProbeHTTPClient(ctx context.Context, first *proxyEndpoint, second proxyEndpoint) *probeHTTPClient {
	p := &probeHTTPClient{transport: chainTransport(first, second)}
	p.transport.DisableKeepAlives = false
	p.transport.MaxIdleConnsPerHost = 1
	dial := p.transport.DialContext
	p.transport.DialContext = func(_ context.Context, network, address string) (net.Conn, error) {
		if p.noDial.Load() {
			return nil, context.Canceled
		}
		return dial(ctx, network, address)
	}
	p.client = &http.Client{Transport: p.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return p
}

func (p *probeHTTPClient) do(req *http.Request) (*http.Response, error) {
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		p.mu.Lock()
		p.conn = info.Conn
		p.mu.Unlock()
	}}
	return p.client.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
}

// Finish the primary response before looking up its egress. Never redial, send
// OAuth headers, follow redirects, or use a previous request's observed address.
func (p *probeHTTPClient) finish(ctx context.Context, response *http.Response) string {
	p.noDial.Store(true)
	defer p.transport.CloseIdleConnections()
	if response == nil {
		return ""
	}
	const drainLimit = 16 << 10
	n, err := io.Copy(io.Discard, io.LimitReader(response.Body, drainLimit+1))
	_ = response.Body.Close()
	if err != nil || n > drainLimit || response.Close || ctx.Err() != nil {
		return ""
	}
	p.mu.Lock()
	conn := p.conn
	p.mu.Unlock()
	if conn == nil {
		return ""
	}
	traceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var sameConnection atomic.Bool
	traceCtx = httptrace.WithClientTrace(traceCtx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if info.Reused && info.Conn == conn {
			sameConnection.Store(true)
		} else {
			cancel()
		}
	}})
	req, err := http.NewRequestWithContext(traceCtx, http.MethodGet, "https://chatgpt.com/cdn-cgi/trace", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := p.client.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || !sameConnection.Load() {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(raw) > 4096 {
		return ""
	}
	return parseEgressTrace(string(raw))
}

func parseEgressTrace(raw string) string {
	var ip, host string
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ip":
			if ip != "" {
				return ""
			}
			ip = value
		case "h":
			host = value
		}
	}
	address, err := netip.ParseAddr(ip)
	if err != nil || address.Zone() != "" || !address.IsGlobalUnicast() || address.IsPrivate() || host != "chatgpt.com" {
		return ""
	}
	return address.Unmap().String()
}
