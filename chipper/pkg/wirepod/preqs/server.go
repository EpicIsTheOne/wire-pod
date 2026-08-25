package processreqs

import (
	"fmt"
	"time"

	"github.com/kercre123/wire-pod/chipper/pkg/logger"
	"github.com/kercre123/wire-pod/chipper/pkg/vars"
	"github.com/kercre123/wire-pod/chipper/pkg/wirepod/epicosbridge"
	sr "github.com/kercre123/wire-pod/chipper/pkg/wirepod/speechrequest"
	ttr "github.com/kercre123/wire-pod/chipper/pkg/wirepod/ttr"
)

// Server stores the config
type Server struct {
	// AgentRouter forwards unmatched speech to EpicOS when configured.
	// Nil means the bridge is disabled; injected for testability.
	AgentRouter epicosbridge.AgentRouter
}

var VoiceProcessor = ""

type JsonIntent struct {
	Name              string   `json:"name"`
	Keyphrases        []string `json:"keyphrases"`
	RequireExactMatch bool     `json:"requiresexact"`
}

var sttLanguage string = "en-US"

// speech-to-text
var sttHandler func(sr.SpeechRequest) (string, error)

// speech-to-intent (rhino)
var stiHandler func(sr.SpeechRequest) (string, map[string]string, error)

var isSti bool = false

func ReloadVosk() {
	if vars.APIConfig.STT.Service == "vosk" || vars.APIConfig.STT.Service == "whisper.cpp" {
		vars.SttInitFunc()
		vars.IntentList, _ = vars.LoadIntents()
	}
}

// New returns a new server
func New(InitFunc func() error, SttHandler interface{}, voiceProcessor string) (*Server, error) {

	// Decide the TTS language
	if voiceProcessor != "vosk" && voiceProcessor != "whisper.cpp" {
		vars.APIConfig.STT.Language = "en-US"
	}
	sttLanguage = vars.APIConfig.STT.Language
	vars.IntentList, _ = vars.LoadIntents()
	logger.Println("Initiating " + voiceProcessor + " voice processor with language " + sttLanguage)
	vars.SttInitFunc = InitFunc
	err := InitFunc()
	if err != nil {
		return nil, err
	}

	// SttHandler can either be `func(sr.SpeechRequest) (string, error)` or `func (sr.SpeechRequest) (string, map[string]string, error)`
	// second one exists to accomodate Rhino

	// check function type
	if str, is := SttHandler.(func(sr.SpeechRequest) (string, error)); is {
		sttHandler = str
	} else if str, is := SttHandler.(func(sr.SpeechRequest) (string, map[string]string, error)); is {
		stiHandler = str
		isSti = true
	} else {
		return nil, fmt.Errorf("stthandler not of correct type")
	}

	// Initiating the chosen voice processor and load intents from json
	VoiceProcessor = voiceProcessor

	// Load plugins
	ttr.LoadPlugins()

	// EpicOS bridge: inject router only when explicitly enabled in config.
	var router epicosbridge.AgentRouter
	if vars.APIConfig.EpicOS.Enable {
		timeout := time.Duration(vars.APIConfig.EpicOS.TimeoutMS) * time.Millisecond
		router = epicosbridge.New(vars.APIConfig.EpicOS.URL, timeout)
		logger.Println("EpicOS bridge enabled: " + vars.APIConfig.EpicOS.URL)
	}

	return &Server{AgentRouter: router}, err
}
