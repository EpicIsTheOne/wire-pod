package processreqs

import (
	"context"
	"fmt"
	"strings"

	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/vtt"
	"github.com/kercre123/wire-pod/chipper/pkg/wirepod/epicosbridge"
	ttr "github.com/kercre123/wire-pod/chipper/pkg/wirepod/ttr"
)

// tryEpicOS forwards one unmatched utterance to the EpicOS bridge and, on a
// speakable outcome, sends the response down the existing stream. It returns
// true only when this call fully handled the request (caller must then stop).
//
// Recursion safety: every non-speakable outcome returns false and the caller
// continues into wire-pod's pre-existing fallback chain (knowledge/LLM or
// intent_system_unmatched). Nothing here re-enters intent matching, so an
// EpicOS → native → EpicOS loop is structurally impossible.
//
// Speech capability note: freeform spoken replies (KNOWLEDGE_GRAPH responses)
// exist only on the IntentGraph stream. The legacy 1.6 ProcessIntent path
// cannot speak arbitrary text by design, so there the bridge logs and yields
// to native behavior — modern OS builds (WireOS) always use the graph path.
func (s *Server) tryEpicOS(req interface{}, device, transcribedText string) bool {
	if s.AgentRouter == nil {
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resp, err := s.AgentRouter.Run(ctx, epicosbridge.AgentRequest{
		Text:      transcribedText,
		SessionID: device,
		RobotESN:  device,
	})
	if err != nil {
		logger.Println("EpicOS bridge unavailable for device " + device + ": " + err.Error())
		logger.LogUI("EpicOS bridge unavailable: " + err.Error())
		return false
	}

	switch resp.Status {
	case epicosbridge.StatusCompleted:
		speech := strings.TrimSpace(resp.Speech)
		if speech == "" {
			logger.Println("EpicOS bridge returned completed status with empty speech; yielding to native")
			return false
		}
		logger.Println("Bot " + device + " response served via EpicOS (run " + resp.RunID + ")")
		logger.LogUI("EpicOS run " + resp.RunID + " completed: '" + speech + "'")
		return s.sendEpicOSSpeech(req, speech, transcribedText)
	case epicosbridge.StatusDeferred:
		logger.Println("Bot " + device + " EpicOS deferred run " + resp.RunID + "; acknowledging")
		ack := strings.TrimSpace(resp.Speech)
		if ack == "" {
			ack = "Okay."
		}
		return s.sendEpicOSSpeech(req, ack, transcribedText)
	default:
		// pass_to_native (or unknown): continue existing fallback chain.
		logger.Println("EpicOS bridge yielded to native for device " + device + " (status " + string(resp.Status) + ")")
		return false
	}
}

func (s *Server) sendEpicOSSpeech(req interface{}, speech, queryText string) bool {
	switch r := req.(type) {
	case *vtt.IntentGraphRequest:
		if err := ttr.KnowledgeGraphResponseIG(r, speech, queryText); err != nil {
			logger.Println("EpicOS bridge: failed sending speech: " + err.Error())
			return false
		}
		return true
	default:
		// Legacy stream: no freeform speech channel exists upstream.
		logger.Println(fmt.Sprintf("EpicOS bridge: speech '%s' not sendable on legacy intent stream", speech))
		return false
	}
}
