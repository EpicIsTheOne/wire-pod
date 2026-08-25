package epicosbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func epicOSServer(t *testing.T, status int, body string, calls *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		if r.URL.Path != bridgePath {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var req bridgeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunCompletedReturnsSpeech(t *testing.T) {
	srv := epicOSServer(t, 200, `{"version":"v1","run_id":"run_1","status":"completed","speech":"I see a charger.","emotion":"calm"}`, nil)
	r := New(srv.URL, 2*time.Second)
	resp, err := r.Run(context.Background(), AgentRequest{Text: "what do you see", RobotESN: "ESN1"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Status != StatusCompleted || resp.Speech != "I see a charger." || resp.Emotion != "calm" || resp.RunID != "run_1" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestRunDeferredMapsToAcknowledge(t *testing.T) {
	srv := epicOSServer(t, 200, `{"version":"v1","status":"deferred","speech":"On it."}`, nil)
	r := New(srv.URL, 2*time.Second)
	resp, err := r.Run(context.Background(), AgentRequest{Text: "remind me later"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Status != StatusDeferred || resp.Speech != "On it." {
		t.Errorf("resp = %+v", resp)
	}
}

func TestConnectionRefusedIsUnavailableNotCrash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // guaranteed-refused port
	r := New(url, time.Second)
	resp, err := r.Run(context.Background(), AgentRequest{Text: "hello"})
	if err == nil {
		t.Fatal("expected error")
	}
	if resp.Status != StatusUnavailable {
		t.Errorf("status = %q, want unavailable", resp.Status)
	}
}

func TestEpicOSTimeoutBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	r := New(srv.URL, 150*time.Millisecond)
	start := time.Now()
	resp, err := r.Run(context.Background(), AgentRequest{Text: "slow"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if resp.Status != StatusUnavailable {
		t.Errorf("status = %q", resp.Status)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("timeout not bounded: %s", elapsed)
	}
}

func TestMalformedJSONFailsSafe(t *testing.T) {
	srv := epicOSServer(t, 200, `{this is not json`, nil)
	r := New(srv.URL, time.Second)
	resp, err := r.Run(context.Background(), AgentRequest{Text: "hi"})
	if err == nil {
		t.Fatal("expected malformed-response error")
	}
	if resp.Status != StatusUnavailable {
		t.Errorf("status = %q", resp.Status)
	}
}

func TestHTTP500IsUnavailable(t *testing.T) {
	srv := epicOSServer(t, 500, `{"error":"boom"}`, nil)
	r := New(srv.URL, time.Second)
	resp, err := r.Run(context.Background(), AgentRequest{Text: "hi"})
	if err == nil {
		t.Fatal("expected http error")
	}
	if resp.Status != StatusUnavailable {
		t.Errorf("status = %q, want unavailable", resp.Status)
	}
}

func TestPassToNativeStatusPreserved(t *testing.T) {
	srv := epicOSServer(t, 200, `{"version":"v1","status":"pass_to_native"}`, nil)
	r := New(srv.URL, time.Second)
	resp, err := r.Run(context.Background(), AgentRequest{Text: "hi"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Status != StatusPassToNative {
		t.Errorf("status = %q", resp.Status)
	}
}

func TestUnknownStatusFailsSafeToPassToNative(t *testing.T) {
	srv := epicOSServer(t, 200, `{"version":"v9","status":"something_new","speech":"gibberish"}`, nil)
	r := New(srv.URL, time.Second)
	resp, _ := r.Run(context.Background(), AgentRequest{Text: "hi"})
	if resp.Status != StatusPassToNative {
		t.Errorf("unknown status must fail safe; got %q", resp.Status)
	}
}

func TestContextCancelPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	r := New(srv.URL, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := r.Run(ctx, AgentRequest{Text: "x"})
	if err == nil {
		t.Fatal("expected ctx error")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("cancel not prompt")
	}
}
