package eval

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync"
)

const containerRelayHost = "host.docker.internal"

// Relay is a temporary authenticated reverse proxy to a gremlord router.
// URL is suitable for processes on the host; ContainerURL addresses the same
// listener from Docker Desktop containers.
type Relay struct {
	URL          string
	ContainerURL string

	server   *http.Server
	listener net.Listener
	done     chan struct{}
	stopOnce sync.Once
	stopErr  error
}

// StartRelay starts a relay on an ephemeral host port. Docker Desktop's
// host.docker.internal gateway cannot reach a host listener bound only to
// 127.0.0.1, so the short-lived relay listens on all host interfaces. The
// random port, existing per-install router token, and /v1/-only path gate are
// therefore all part of the security boundary; the normal router itself stays
// loopback-only. All request headers and streaming responses are passed
// through by the standard reverse proxy.
func StartRelay(ctx context.Context, upstreamURL, token string) (*Relay, error) {
	if ctx == nil {
		return nil, errors.New("eval relay: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("eval relay: %w", err)
	}
	if token == "" {
		return nil, errors.New("eval relay: token is required")
	}
	upstream, err := url.Parse(upstreamURL)
	if err != nil || upstream.Scheme == "" || upstream.Host == "" {
		return nil, fmt.Errorf("eval relay: invalid upstream URL %q", upstreamURL)
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("eval relay: listen: %w", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(upstream)
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || !strings.HasPrefix(req.URL.Path, "/v1/") || req.URL.Path != path.Clean(req.URL.Path) {
			http.NotFound(w, req)
			return
		}
		got := req.Header.Get("x-api-key")
		if got == "" {
			auth := req.Header.Get("Authorization")
			if strings.HasPrefix(auth, "Bearer ") {
				got = strings.TrimPrefix(auth, "Bearer ")
			}
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		proxy.ServeHTTP(w, req)
	})

	port := listener.Addr().(*net.TCPAddr).Port
	r := &Relay{
		URL:          fmt.Sprintf("http://127.0.0.1:%d", port),
		ContainerURL: fmt.Sprintf("http://%s:%d", containerRelayHost, port),
		listener:     listener,
		done:         make(chan struct{}),
	}
	r.server = &http.Server{Handler: handler}
	go func() {
		defer close(r.done)
		_ = r.server.Serve(listener)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = r.Close()
		case <-r.done:
		}
	}()
	return r, nil
}

// Close shuts the relay down and waits for its serve loop to exit. It is safe
// to call more than once.
func (r *Relay) Close() error {
	if r == nil {
		return nil
	}
	r.stopOnce.Do(func() {
		r.stopErr = r.server.Close()
		if errors.Is(r.stopErr, http.ErrServerClosed) {
			r.stopErr = nil
		}
		<-r.done
	})
	return r.stopErr
}
