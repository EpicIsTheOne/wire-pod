// Package epicosbridge is a thin HTTP client that lets wire-pod forward
// unmatched speech to a local EpicOS agent server. It intentionally contains
// no agent logic, no model logic, and no secrets — EpicOS owns all of that.
//
// Protocol: POST {BaseURL}/bridge/v1/handle-text with JSON body
//
//	{"text": "...", "session_id": "...", "robot_esn": "..."}
//
// Response (any 2xx): JSON with fields version, run_id, status, speech,
// emotion, error. Status is one of the EpicOS run statuses; this package
// normalizes them into the four routing outcomes wire-pod understands:
// completed, deferred, pass_to_native, unavailable.
package epicosbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kercre123/wire-pod/chipper/pkg/logger"
)

const (
	ProtocolVersion = "v1"
	DefaultTimeout  = 10 * time.Second
	bridgePath      = "/bridge/v1/handle-text"
)

// Routing outcomes for wire-pod.
type Status string

const (
	StatusCompleted    Status = "completed"      // speak resp.Speech now
	StatusDeferred     Status = "deferred"       // speak ack; EpicOS task system finishes later
	StatusPassToNative Status = "pass_to_native" // continue native/fallback path, no recursion
	StatusUnavailable  Status = "unavailable"    // EpicOS unreachable/invalid; fail gracefully
)

type AgentRequest struct {
	Text      string
	SessionID string
	RobotESN  string
}

type AgentResponse struct {
	Version string
	RunID   string
	Status  Status
	Speech  string
	Emotion string
	Error   string
}

// AgentRouter is the injection surface wire-pod consumes. preqs holds one of
// these; tests supply fakes.
type AgentRouter interface {
	Run(ctx context.Context, req AgentRequest) (AgentResponse, error)
}

type Router struct {
	BaseURL string
	Timeout time.Duration
	HTTP    *http.Client
}

func New(baseURL string, timeout time.Duration) *Router {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Router{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Timeout: timeout,
		HTTP:    &http.Client{}, // per-request deadlines via ctx only
	}
}

type bridgeRequest struct {
	Text      string `json:"text"`
	SessionID string `json:"session_id,omitempty"`
	RobotESN  string `json:"robot_esn,omitempty"`
}

type bridgeResponse struct {
	Version string `json:"version"`
	RunID   string `json:"run_id"`
	Status  string `json:"status"`
	Speech  string `json:"speech"`
	Emotion string `json:"emotion"`
	Error   string `json:"error"`
}

// Run forwards one utterance to EpicOS. It never panics and never blocks
// beyond the configured timeout; any failure maps to StatusUnavailable so
// callers can fall through safely.
func (r *Router) Run(ctx context.Context, req AgentRequest) (AgentResponse, error) {
	resp := AgentResponse{Version: ProtocolVersion, Status: StatusUnavailable}
	if r == nil || r.BaseURL == "" {
		return resp, fmt.Errorf("epicosbridge: router not configured")
	}
	if strings.TrimSpace(req.Text) == "" {
		return resp, fmt.Errorf("epicosbridge: empty text")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(bridgeRequest{Text: req.Text, SessionID: req.SessionID, RobotESN: req.RobotESN})
	if err != nil {
		return resp, fmt.Errorf("epicosbridge: encode: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.BaseURL, "/")+bridgePath, bytes.NewReader(body))
	if err != nil {
		return resp, fmt.Errorf("epicosbridge: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := r.HTTP.Do(httpReq)
	if err != nil {
		// connection refused, timeout, ctx cancelled — all just "unavailable"
		return resp, fmt.Errorf("epicosbridge: post: %w", err)
	}
	defer httpResp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))

	var br bridgeResponse
	if decodeErr := json.Unmarshal(data, &br); decodeErr != nil {
		return resp, fmt.Errorf("epicosbridge: malformed response (http %d): %w", httpResp.StatusCode, decodeErr)
	}
	resp.RunID = br.RunID
	resp.Speech = br.Speech
	resp.Emotion = br.Emotion
	resp.Error = br.Error

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// Non-2xx: treat as unavailable regardless of body, but keep details.
		if br.Status != "" {
			resp.Status = normalizeStatus(br.Status)
		}
		return resp, fmt.Errorf("epicosbridge: http %d: %s", httpResp.StatusCode, br.Error)
	}

	resp.Status = normalizeStatus(br.Status)
	if br.Version != "" && br.Version != ProtocolVersion {
		logger.Println("EpicOS bridge: protocol version mismatch (got " + br.Version + ", want " + ProtocolVersion + ")")
	}
	return resp, nil
}

// normalizeStatus maps raw EpicOS run statuses onto wire-pod routing outcomes.
// Unknown statuses fail safe as pass_to_native (never speak unknown text).
func normalizeStatus(s string) Status {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "completed":
		return StatusCompleted
	case "deferred":
		return StatusDeferred
	case "pass_to_native":
		return StatusPassToNative
	default:
		return StatusPassToNative
	}
}
