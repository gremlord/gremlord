package responsesbe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/config"
)

func nativeCall(url string) *backend.Call {
	return &backend.Call{Raw: []byte(`{"model":"gpt-test","input":[],"store":false}`), Route: config.Resolved{Provider: config.Provider{BaseURL: url, APIKey: "private"}}}
}

func TestClientCancellationStopsUpstream(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ":ping\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer up.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan backend.Result, 1)
	go New().Forward(ctx, nativeCall(up.URL), httptest.NewRecorder(), "/responses", func(r backend.Result) { result <- r })
	<-started
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("upstream continued after cancellation")
	}
	select {
	case r := <-result:
		if r.Status != 499 || r.ErrType != "client_disconnect" {
			t.Fatalf("result=%+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("no cancellation record")
	}
}

func TestDoesNotFollowCredentialRedirect(t *testing.T) {
	var leaked atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer destination.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer up.Close()
	w := httptest.NewRecorder()
	New().Forward(context.Background(), nativeCall(up.URL), w, "/responses", func(r backend.Result) {
		if r.Status != 307 || r.ErrType != "upstream_http_error" {
			t.Errorf("result=%+v", r)
		}
	})
	if leaked.Load() {
		t.Fatal("followed upstream redirect")
	}
}

func TestUsageRecordedOnceBeforeTerminalDelivery(t *testing.T) {
	stream := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":12,\"output_tokens\":2}}}\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream+stream)
	}))
	defer up.Close()
	w := httptest.NewRecorder()
	n := 0
	New().Forward(context.Background(), nativeCall(up.URL), w, "/responses", func(r backend.Result) {
		n++
		if r.Usage.InputTokens != 12 || r.Usage.OutputTokens != 2 {
			t.Errorf("usage=%+v", r)
		}
		if strings.Contains(w.Body.String(), "\n\n") {
			t.Error("terminal delivered before budget update")
		}
	})
	if n != 1 || w.Body.String() != stream+stream {
		t.Errorf("records=%d body=%q", n, w.Body.String())
	}
}
