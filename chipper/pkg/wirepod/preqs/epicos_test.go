package processreqs

// Phase 4 regression tests: prove native intent matching bypasses EpicOS,
// unmatched text reaches EpicOS exactly once, and bridge failures fall back
// to wire-pod's existing behavior without crashing the stream.

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kercre123/wire-pod/chipper/pkg/vars"
	"github.com/kercre123/wire-pod/chipper/pkg/vtt"
	"github.com/kercre123/wire-pod/chipper/pkg/wirepod/epicosbridge"
	sr "github.com/kercre123/wire-pod/chipper/pkg/wirepod/speechrequest"

	pb "github.com/digital-dream-labs/api/go/chipperpb"
	"google.golang.org/grpc"
)

// ---- test doubles ----

type fakeIGStream struct {
	grpc.ServerStream
	mu   sync.Mutex
	sent []*pb.IntentGraphResponse
}

func (f *fakeIGStream) Send(resp *pb.IntentGraphResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, resp)
	return nil
}

// Recv is part of the generated stream interface; unused in these tests.
func (f *fakeIGStream) Recv() (*pb.StreamingIntentGraphRequest, error) {
	return nil, io.EOF
}

func (f *fakeIGStream) responses() []*pb.IntentGraphResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*pb.IntentGraphResponse, len(f.sent))
	copy(out, f.sent)
	return out
}

type fakeRouter struct {
	mu      sync.Mutex
	calls   int
	lastReq epicosbridge.AgentRequest
	resp    epicosbridge.AgentResponse
	err     error
}

func (fr *fakeRouter) Run(_ context.Context, req epicosbridge.AgentRequest) (epicosbridge.AgentResponse, error) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.calls++
	fr.lastReq = req
	return fr.resp, fr.err
}

func (fr *fakeRouter) callCount() int {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return fr.calls
}

// setSTT installs a deterministic stt handler for the duration of one test.
func setSTT(t *testing.T, transcript string) {
	t.Helper()
	prev := sttHandler
	sttHandler = func(_ sr.SpeechRequest) (string, error) { return transcript, nil }
	t.Cleanup(func() { sttHandler = prev })
	isSti = false
}

func graphRequest(stream *fakeIGStream) *vtt.IntentGraphRequest {
	return &vtt.IntentGraphRequest{
		Device:  "ESN-TEST-1",
		Session: "ses-test",
		Stream:  stream,
		FirstReq: &pb.StreamingIntentGraphRequest{
			InputAudio: []byte{0x01, 0x02},
		},
	}
}

func newTestServer(router epicosbridge.AgentRouter) *Server {
	vars.APIConfig.Knowledge.Enable = false
	vars.APIConfig.Knowledge.IntentGraph = false
	return &Server{AgentRouter: router}
}

// ---- tests ----

func TestNativeIntentBypassesEpicOS(t *testing.T) {
	setSTT(t, "go home")
	vars.IntentList = []vars.JsonIntent{
		{Name: "intent_imperial_home", Keyphrases: []string{"go home"}, RequireExactMatch: false},
	}
	t.Cleanup(func() { vars.IntentList = nil })

	router := &fakeRouter{resp: epicosbridge.AgentResponse{Status: epicosbridge.StatusCompleted, Speech: "should not be spoken"}}
	s := newTestServer(router)
	stream := &fakeIGStream{}

	if _, err := s.ProcessIntentGraph(graphRequest(stream)); err != nil {
		t.Fatalf("ProcessIntentGraph: %v", err)
	}
	if n := router.callCount(); n != 0 {
		t.Fatalf("EpicOS was called %d times on a native intent match; must be 0", n)
	}
	resps := stream.responses()
	if len(resps) != 1 {
		t.Fatalf("stream got %d responses, want 1", len(resps))
	}
	r := resps[0]
	if r.ResponseType != pb.IntentGraphMode_INTENT || r.IntentResult.Action != "intent_imperial_home" {
		t.Errorf("native intent response wrong: %+v", r.IntentResult)
	}
}

// TestNativeIntentsBypassMatrix extends the single bypass proof to a spread
// of native utterance shapes (motion, clock, media, attention).
func TestNativeIntentsBypassMatrix(t *testing.T) {
	cases := []struct{ transcript, intent string }{
		{"come here", "intent_motion_moveclose"},
		{"what time is it", "intent_clock_time"},
		{"play some music", "intent_play_anything"},
		{"look at my face", "intent_face_attention"},
	}
	for _, tc := range cases {
		t.Run(tc.transcript, func(t *testing.T) {
			setSTT(t, tc.transcript)
			vars.IntentList = []vars.JsonIntent{
				{Name: tc.intent, Keyphrases: []string{tc.transcript}, RequireExactMatch: false},
			}
			router := &fakeRouter{}
			s := newTestServer(router)
			stream := &fakeIGStream{}
			if _, err := s.ProcessIntentGraph(graphRequest(stream)); err != nil {
				t.Fatalf("ProcessIntentGraph: %v", err)
			}
			if n := router.callCount(); n != 0 {
				t.Errorf("EpicOS called %d times for native match", n)
			}
			resps := stream.responses()
			if len(resps) != 1 || resps[0].ResponseType != pb.IntentGraphMode_INTENT || resps[0].IntentResult.Action != tc.intent {
				t.Errorf("native response wrong: %+v", resps)
			}
		})
	}
}

func TestUnmatchedReachesEpicOSAndSpeaks(t *testing.T) {
	setSTT(t, "look around and tell me what you notice")
	vars.IntentList = nil

	router := &fakeRouter{resp: epicosbridge.AgentResponse{
		Status: epicosbridge.StatusCompleted, RunID: "run_x", Speech: "I notice a desk and a lamp.",
	}}
	s := newTestServer(router)
	stream := &fakeIGStream{}

	if _, err := s.ProcessIntentGraph(graphRequest(stream)); err != nil {
		t.Fatalf("ProcessIntentGraph: %v", err)
	}
	if n := router.callCount(); n != 1 {
		t.Fatalf("EpicOS calls = %d, want exactly 1", n)
	}
	if router.lastReq.Text != "look around and tell me what you notice" {
		t.Errorf("bridge received wrong text: %q", router.lastReq.Text)
	}
	resps := stream.responses()
	if len(resps) != 1 {
		t.Fatalf("stream got %d responses, want 1", len(resps))
	}
	r := resps[0]
	if r.ResponseType != pb.IntentGraphMode_KNOWLEDGE_GRAPH || r.SpokenText != "I notice a desk and a lamp." || !r.IsFinal {
		t.Errorf("spoken response wrong: type=%v speech=%q final=%v",
			r.ResponseType, r.SpokenText, r.IsFinal)
	}
}

func TestEpicOSUnavailableFallsBackToUnmatchedIntent(t *testing.T) {
	setSTT(t, "flurb the wozzle")
	vars.IntentList = nil

	router := &fakeRouter{err: errors.New("connection refused")}
	s := newTestServer(router)
	stream := &fakeIGStream{}

	if _, err := s.ProcessIntentGraph(graphRequest(stream)); err != nil {
		t.Fatalf("ProcessIntentGraph returned error (must not): %v", err)
	}
	if n := router.callCount(); n != 1 {
		t.Errorf("EpicOS calls = %d, want 1", n)
	}
	resps := stream.responses()
	if len(resps) == 0 {
		t.Fatal("no response sent; voice stream would hang")
	}
	last := resps[len(resps)-1]
	if last.ResponseType != pb.IntentGraphMode_INTENT || last.IntentResult.Action != "intent_system_unmatched" {
		t.Errorf("fallback response wrong: %+v", last.IntentResult)
	}
}

func TestPassToNativeYieldsToExistingFallback(t *testing.T) {
	setSTT(t, "mystery phrase")
	vars.IntentList = nil

	router := &fakeRouter{resp: epicosbridge.AgentResponse{Status: epicosbridge.StatusPassToNative}}
	s := newTestServer(router)
	stream := &fakeIGStream{}

	if _, err := s.ProcessIntentGraph(graphRequest(stream)); err != nil {
		t.Fatalf("ProcessIntentGraph: %v", err)
	}
	resps := stream.responses()
	if len(resps) == 0 {
		t.Fatal("no response after pass_to_native")
	}
	if resps[len(resps)-1].IntentResult.Action != "intent_system_unmatched" {
		t.Errorf("expected native unmatched handling, got %+v", resps[len(resps)-1].IntentResult)
	}
}

func TestBridgeDisabledNeverConsulted(t *testing.T) {
	setSTT(t, "anything at all")
	vars.IntentList = nil

	router := &fakeRouter{}
	s := newTestServer(nil) // disabled: no router injected
	stream := &fakeIGStream{}

	if _, err := s.ProcessIntentGraph(graphRequest(stream)); err != nil {
		t.Fatalf("ProcessIntentGraph: %v", err)
	}
	if n := router.callCount(); n != 0 {
		t.Errorf("router consulted while disabled")
	}
	if len(stream.responses()) == 0 {
		t.Error("no response for unmatched text when bridge disabled")
	}
}

func TestLegacyPathCannotSpeakButDoesNotCrash(t *testing.T) {
	setSTT(t, "legacy unmatched phrase")
	vars.IntentList = nil

	router := &fakeRouter{resp: epicosbridge.AgentResponse{
		Status: epicosbridge.StatusCompleted, Speech: "freeform text",
	}}
	s := newTestServer(router)

	legacyStream := &fakeLegacyStream{}
	req := &vtt.IntentRequest{
		Device:  "ESN-TEST-2",
		Session: "ses-test",
		Stream:  legacyStream,
		FirstReq: &pb.StreamingIntentRequest{
			InputAudio: []byte{0x01},
		},
	}
	if _, err := s.ProcessIntent(req); err != nil {
		t.Fatalf("ProcessIntent: %v", err)
	}
	if n := router.callCount(); n != 1 {
		t.Errorf("EpicOS calls = %d, want 1 (consulted, then yields)", n)
	}
	if len(legacyStream.sent) == 0 {
		t.Fatal("legacy stream got no response")
	}
	last := legacyStream.sent[len(legacyStream.sent)-1]
	if last.IntentResult.Action != "intent_system_unmatched" {
		t.Errorf("legacy fallback action = %q, want intent_system_unmatched", last.IntentResult.Action)
	}
}

// TestLiveEpicOSBridgeE2E runs the routing path against a REAL EpicOS
// process. Opt-in (needs a running EpicOS):
//
//	EPICOS_LIVE_BRIDGE=1 EPICOS_LIVE_URL=http://127.0.0.1:8787 \
//	  go test ./pkg/wirepod/preqs -run TestLiveEpicOSBridgeE2E -v
func TestLiveEpicOSBridgeE2E(t *testing.T) {
	url := os.Getenv("EPICOS_LIVE_URL")
	if os.Getenv("EPICOS_LIVE_BRIDGE") != "1" || url == "" {
		t.Skip("live bridge E2E disabled (set EPICOS_LIVE_BRIDGE=1 and EPICOS_LIVE_URL)")
	}
	setSTT(t, "look around and tell me what you notice")
	vars.IntentList = nil

	s := newTestServer(epicosbridge.New(url, 15*time.Second))
	stream := &fakeIGStream{}
	if _, err := s.ProcessIntentGraph(graphRequest(stream)); err != nil {
		t.Fatalf("ProcessIntentGraph: %v", err)
	}
	resps := stream.responses()
	if len(resps) != 1 {
		t.Fatalf("stream responses = %d, want 1", len(resps))
	}
	r := resps[0]
	if r.ResponseType != pb.IntentGraphMode_KNOWLEDGE_GRAPH || strings.TrimSpace(r.SpokenText) == "" {
		t.Fatalf("no spoken EpicOS response: type=%v speech=%q", r.ResponseType, r.SpokenText)
	}
	t.Logf("LIVE E2E OK: EpicOS spoke %q via KNOWLEDGE_GRAPH response", r.SpokenText)
}

type fakeLegacyStream struct {
	grpc.ServerStream
	sent []*pb.IntentResponse
}

func (f *fakeLegacyStream) Send(resp *pb.IntentResponse) error {
	f.sent = append(f.sent, resp)
	return nil
}

// Recv is part of the generated stream interface; unused in these tests.
func (f *fakeLegacyStream) Recv() (*pb.StreamingIntentRequest, error) {
	return nil, io.EOF
}
