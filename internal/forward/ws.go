package forward

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/storm-software/mindctl/internal/accounts"
)

// ProxyWebSocket attaches the caller to the selected account's websocket.
// Protocols that differ are not translated on this path.
func (f *Forwarder) ProxyWebSocket(ctx context.Context, w http.ResponseWriter, r *http.Request, model, callerProtocol, upstreamPath string) error {
	if f == nil || f.Selector == nil {
		return errors.New("forward: account selector is required")
	}
	if !websocketUpgrade(r) {
		http.Error(w, "upgrade required", http.StatusUpgradeRequired)
		return errors.New("forward: upgrade required")
	}
	modelID, provider, err := f.Resolve(model)
	if err != nil {
		return err
	}
	tried := []string{}
	var last error
	for round := 0; round <= 32; round++ {
		selection, selectErr := f.Selector.Select(ctx, accounts.SelectRequest{
			ModelID: modelID, Provider: provider, Round: round, Tried: append([]string(nil), tried...),
		})
		if selectErr != nil {
			if last != nil {
				return last
			}
			return selectErr
		}
		accountProtocol := selection.Account.Protocol
		if accountProtocol == "" {
			accountProtocol = nativeProtocol(selection.Account.Provider)
		}
		if callerProtocol != "" && accounts.ProtocolFamily(callerProtocol) != accounts.ProtocolFamily(accountProtocol) {
			return errors.New("forward: websocket protocol translation is not available")
		}
		err = f.proxyOne(ctx, w, r, selection, upstreamPath)
		if err == nil {
			f.Selector.ReportSuccess(ctx, "", selection.Account.ID)
			return nil
		}
		f.Selector.ReportFailure(ctx, "", selection.Account.ID, accounts.FailureTransient)
		last = err
		tried = append(tried, selection.Account.ID)
	}
	if last == nil {
		last = accounts.ErrNone
	}
	return last
}

func (f *Forwarder) proxyOne(ctx context.Context, w http.ResponseWriter, r *http.Request, selection accounts.Selection, upstreamPath string) error {
	base := selection.Account.BaseURL
	if base == "" {
		return errors.New("forward: account has no base URL")
	}
	target, err := url.Parse(strings.TrimRight(base, "/") + normalizePath(base, upstreamPath))
	if err != nil {
		return err
	}
	if query := r.URL.RawQuery; query != "" {
		target.RawQuery = query
	}
	conn, err := dialUpstream(ctx, target, selection.Account.ProxyURL)
	if err != nil {
		return err
	}
	defer conn.Close()
	outbound := r.Clone(ctx)
	outbound.URL = target
	outbound.RequestURI = ""
	outbound.Host = target.Host
	outbound.Header = r.Header.Clone()
	outbound.Header.Del("Authorization")
	applyUpstreamAuth(outbound, selection)
	if err := outbound.Write(conn); err != nil {
		return err
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return errors.New("forward: websocket hijack is unavailable")
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		return err
	}
	defer client.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(conn, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, conn)
		done <- struct{}{}
	}()
	select {
	case <-ctx.Done():
		return nil
	case <-done:
		return nil
	}
}

func websocketUpgrade(r *http.Request) bool {
	for _, value := range r.Header.Values("Upgrade") {
		if strings.Contains(strings.ToLower(value), "websocket") {
			return true
		}
	}
	return false
}

func dialUpstream(ctx context.Context, target *url.URL, proxyURL string) (net.Conn, error) {
	address := target.Host
	if _, _, err := net.SplitHostPort(address); err != nil {
		port := "80"
		if target.Scheme == "https" || target.Scheme == "wss" {
			port = "443"
		}
		address = net.JoinHostPort(target.Hostname(), port)
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	if proxyURL != "" {
		conn, err = dialProxy(ctx, dialer, proxyURL, address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return nil, err
	}
	if target.Scheme == "https" || target.Scheme == "wss" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: target.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		return tlsConn, nil
	}
	return conn, nil
}

func dialProxy(ctx context.Context, dialer *net.Dialer, proxyURL, address string) (net.Conn, error) {
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("forward: invalid account proxy")
	}
	proxyAddress := parsed.Host
	if _, _, err := net.SplitHostPort(proxyAddress); err != nil {
		proxyAddress = net.JoinHostPort(parsed.Hostname(), "80")
	}
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, err
	}
	connect, err := http.NewRequestWithContext(ctx, http.MethodConnect, "http://"+address, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	connect.Host = address
	if err := connect.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), connect)
	if err != nil {
		conn.Close()
		return nil, err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		conn.Close()
		return nil, errors.New("forward: proxy connect failed")
	}
	return conn, nil
}
