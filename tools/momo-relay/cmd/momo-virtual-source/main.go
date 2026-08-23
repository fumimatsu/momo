package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	virtualSourcePrefix    = "/ws/"
	maximumVirtualSources  = 64
	virtualSerialLabel     = "serial"
	telemetryHighWatermark = 1024 * 1024
)

var virtualH264Codec = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeH264,
	ClockRate:   90000,
	SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	RTCPFeedback: []webrtc.RTCPFeedback{
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
	},
}

type signalMessage struct {
	Type  string                   `json:"type"`
	SDP   string                   `json:"sdp,omitempty"`
	ICE   *webrtc.ICECandidateInit `json:"ice,omitempty"`
	Error string                   `json:"error,omitempty"`
}

type virtualSourceServer struct {
	api      *webrtc.API
	allowed  map[string]struct{}
	playback map[string]playbackProfile
	runtime  map[string]*sourceRuntimeStatus

	mu     sync.Mutex
	active map[string]context.CancelFunc
}

type playbackProfile struct {
	asset             *videoAsset
	startIndex        int
	telemetry         []timedReplayMessage
	telemetryPath     string
	captureReplayRate float64
}

type playbackProfileStatus struct {
	SourceID            string  `json:"sourceId"`
	InputPath           string  `json:"inputPath"`
	StartFrame          int     `json:"startFrame"`
	StartOffsetMS       int64   `json:"startOffsetMs"`
	CaptureReplayRate   float64 `json:"captureReplayRate"`
	TelemetryPath       string  `json:"telemetryPath,omitempty"`
	TelemetryEventCount int     `json:"telemetryEventCount"`
	SerialOpen          bool    `json:"serialOpen"`
	TelemetrySent       uint64  `json:"telemetrySent"`
	TelemetryDropped    uint64  `json:"telemetryDropped"`
	TelemetrySendErrors uint64  `json:"telemetrySendErrors"`
	CommandsReceived    uint64  `json:"commandsReceived"`
}

type sourceRuntimeStatus struct {
	serialOpen          atomic.Bool
	telemetrySent       atomic.Uint64
	telemetryDropped    atomic.Uint64
	telemetrySendErrors atomic.Uint64
	commandsReceived    atomic.Uint64
}

func main() {
	var listen string
	var input string
	var sourceList string
	var fps int
	var spreadStarts bool
	var spreadStartMaxPercent int
	var profileManifest string
	flag.StringVar(&listen, "listen", "127.0.0.1:18880", "HTTP and WebSocket listen address")
	flag.StringVar(&input, "input", "", "H.264 Annex-B input with AUD NAL units")
	flag.StringVar(&sourceList, "sources", "virtual-01,virtual-02,virtual-03,virtual-04,virtual-05", "comma-separated source IDs")
	flag.IntVar(&fps, "fps", 30, "playback frame rate")
	flag.BoolVar(&spreadStarts, "spread-starts", false, "spread source start positions across input keyframes")
	flag.IntVar(&spreadStartMaxPercent, "spread-start-max-percent", 0, "optional inclusive maximum input position for spread starts (1-100; 0 uses the full legacy spread)")
	flag.StringVar(&profileManifest, "profile-manifest", "", "optional per-source replay profile manifest")
	flag.Parse()

	if fps < 1 || fps > 120 || spreadStartMaxPercent < 0 || spreadStartMaxPercent > 100 {
		log.Fatal("-fps must be between 1 and 120, and -spread-start-max-percent must be 0 to 100")
	}
	var profiles map[string]playbackProfile
	if profileManifest != "" {
		var err error
		profiles, err = loadReplayManifest(profileManifest)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		if input == "" {
			log.Fatal("-input is required when -profile-manifest is not configured")
		}
		asset, err := loadVideoAsset(input, fps)
		if err != nil {
			log.Fatal(err)
		}
		sourceIDs, err := parseSourceIDs(sourceList)
		if err != nil {
			log.Fatal(err)
		}
		profiles = buildPlaybackProfiles(asset.accessUnits, sourceIDs, spreadStarts, spreadStartMaxPercent)
		for sourceID, profile := range profiles {
			profile.asset = asset
			profile.captureReplayRate = 1
			profiles[sourceID] = profile
		}
	}
	sourceIDs := make([]string, 0, len(profiles))
	for sourceID := range profiles {
		sourceIDs = append(sourceIDs, sourceID)
	}
	sort.Strings(sourceIDs)
	api, err := newVirtualSourceAPI()
	if err != nil {
		log.Fatal(err)
	}
	server := &virtualSourceServer{
		api:      api,
		allowed:  make(map[string]struct{}, len(sourceIDs)),
		playback: profiles,
		runtime:  make(map[string]*sourceRuntimeStatus, len(sourceIDs)),
		active:   make(map[string]context.CancelFunc),
	}
	for _, sourceID := range sourceIDs {
		server.allowed[sourceID] = struct{}{}
		server.runtime[sourceID] = &sourceRuntimeStatus{}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.serveHealth)
	mux.HandleFunc(virtualSourcePrefix, server.serveWebSocket)
	httpServer := &http.Server{Addr: listen, Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	log.Printf("virtual Momo source listening on http://%s for %s (profile manifest=%t)", listen, strings.Join(sourceIDs, ", "), profileManifest != "")
	for _, sourceID := range sourceIDs {
		profile := server.playback[sourceID]
		log.Printf("source %q input=%q playback starts at frame %d (%s), telemetry events=%d", sourceID, profile.asset.inputPath, profile.startIndex, time.Duration(profile.startIndex)*profile.asset.frameDuration, len(profile.telemetry))
	}
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func parseSourceIDs(value string) ([]string, error) {
	seen := make(map[string]struct{})
	var result []string
	for _, raw := range strings.Split(value, ",") {
		sourceID := strings.TrimSpace(raw)
		if sourceID == "" || strings.ContainsAny(sourceID, "/?#\\") {
			return nil, fmt.Errorf("invalid source ID: %q", raw)
		}
		if _, exists := seen[sourceID]; exists {
			return nil, fmt.Errorf("duplicate source ID: %s", sourceID)
		}
		seen[sourceID] = struct{}{}
		result = append(result, sourceID)
	}
	if len(result) == 0 || len(result) > maximumVirtualSources {
		return nil, fmt.Errorf("sources must contain 1 to %d IDs", maximumVirtualSources)
	}
	sort.Strings(result)
	return result, nil
}

func buildPlaybackProfiles(units []h264AccessUnit, sourceIDs []string, spread bool, maximumPercent int) map[string]playbackProfile {
	profiles := make(map[string]playbackProfile, len(sourceIDs))
	if len(sourceIDs) == 0 {
		return profiles
	}
	keyframes := make([]int, 0)
	for index, unit := range units {
		if unit.keyframe {
			keyframes = append(keyframes, index)
		}
	}
	if len(keyframes) == 0 {
		keyframes = append(keyframes, 0)
	}
	for index, sourceID := range sourceIDs {
		startIndex := keyframes[0]
		if spread {
			keyframeIndex := index * len(keyframes) / len(sourceIDs)
			if maximumPercent > 0 && len(sourceIDs) > 1 {
				maximumKeyframeIndex := (len(keyframes) - 1) * maximumPercent / 100
				keyframeIndex = index * maximumKeyframeIndex / (len(sourceIDs) - 1)
			}
			if keyframeIndex >= len(keyframes) {
				keyframeIndex = len(keyframes) - 1
			}
			startIndex = keyframes[keyframeIndex]
		}
		profiles[sourceID] = playbackProfile{startIndex: startIndex}
	}
	return profiles
}

func newVirtualSourceAPI() (*webrtc.API, error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: virtualH264Codec,
		PayloadType:        102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, fmt.Errorf("register H.264 codec: %w", err)
	}
	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, registry); err != nil {
		return nil, fmt.Errorf("register WebRTC interceptors: %w", err)
	}
	return webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(registry),
	), nil
}

func (server *virtualSourceServer) serveHealth(w http.ResponseWriter, _ *http.Request) {
	server.mu.Lock()
	active := make([]string, 0, len(server.active))
	for sourceID := range server.active {
		active = append(active, sourceID)
	}
	server.mu.Unlock()
	sort.Strings(active)
	profiles := make([]playbackProfileStatus, 0, len(server.allowed))
	for sourceID := range server.allowed {
		profile := server.playback[sourceID]
		runtime := server.runtime[sourceID]
		profiles = append(profiles, playbackProfileStatus{
			SourceID:            sourceID,
			InputPath:           profile.asset.inputPath,
			StartFrame:          profile.startIndex,
			StartOffsetMS:       (time.Duration(profile.startIndex) * profile.asset.frameDuration).Milliseconds(),
			CaptureReplayRate:   profile.captureReplayRate,
			TelemetryPath:       profile.telemetryPath,
			TelemetryEventCount: len(profile.telemetry),
			SerialOpen:          runtime.serialOpen.Load(),
			TelemetrySent:       runtime.telemetrySent.Load(),
			TelemetryDropped:    runtime.telemetryDropped.Load(),
			TelemetrySendErrors: runtime.telemetrySendErrors.Load(),
			CommandsReceived:    runtime.commandsReceived.Load(),
		})
	}
	sort.Slice(profiles, func(left, right int) bool { return profiles[left].SourceID < profiles[right].SourceID })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"active":   active,
		"sources":  len(server.allowed),
		"playback": profiles,
	})
}

func (server *virtualSourceServer) serveWebSocket(w http.ResponseWriter, request *http.Request) {
	sourceID := strings.TrimPrefix(request.URL.Path, virtualSourcePrefix)
	if _, allowed := server.allowed[sourceID]; !allowed || sourceID == "" {
		http.Error(w, "unknown virtual source", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithCancel(request.Context())
	server.mu.Lock()
	if _, active := server.active[sourceID]; active {
		server.mu.Unlock()
		cancel()
		http.Error(w, "virtual source already connected", http.StatusConflict)
		return
	}
	server.active[sourceID] = cancel
	server.mu.Unlock()
	defer func() {
		cancel()
		server.mu.Lock()
		delete(server.active, sourceID)
		server.mu.Unlock()
	}()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	connection, err := upgrader.Upgrade(w, request, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	if err := server.runPeer(ctx, sourceID, connection); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("source %q disconnected: %v", sourceID, err)
	}
}

func (server *virtualSourceServer) runPeer(ctx context.Context, sourceID string, connection *websocket.Conn) error {
	peer, err := server.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return fmt.Errorf("create peer connection: %w", err)
	}
	defer peer.Close()
	track, err := webrtc.NewTrackLocalStaticSample(virtualH264Codec, "video", sourceID)
	if err != nil {
		return fmt.Errorf("create H.264 track: %w", err)
	}
	sender, err := peer.AddTrack(track)
	if err != nil {
		return fmt.Errorf("add H.264 track: %w", err)
	}

	var writeMu sync.Mutex
	send := func(message signalMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return connection.WriteJSON(message)
	}
	peer.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		value := candidate.ToJSON()
		if err := send(signalMessage{Type: "candidate", ICE: &value}); err != nil {
			log.Printf("source %q send ICE candidate: %v", sourceID, err)
		}
	})
	peer.OnDataChannel(func(channel *webrtc.DataChannel) {
		log.Printf("source %q DataChannel %q opened by Relay", sourceID, channel.Label())
		if channel.Label() != virtualSerialLabel {
			return
		}
		runtime := server.runtime[sourceID]
		channel.OnOpen(func() {
			runtime.serialOpen.Store(true)
			if len(server.playback[sourceID].telemetry) > 0 {
				go server.playTelemetry(ctx, sourceID, channel)
			}
		})
		channel.OnMessage(func(webrtc.DataChannelMessage) {
			runtime.commandsReceived.Add(1)
		})
		channel.OnClose(func() { runtime.serialOpen.Store(false) })
	})

	done := make(chan struct{})
	var doneOnce sync.Once
	closeDone := func() { doneOnce.Do(func() { close(done) }) }
	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("source %q peer state: %s", sourceID, state)
		switch state {
		case webrtc.PeerConnectionStateConnected:
			go server.play(ctx, sourceID, track, sender)
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			closeDone()
		}
	})

	remoteDescriptionSet := false
	var pending []webrtc.ICECandidateInit
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return nil
		default:
		}
		_, data, err := connection.ReadMessage()
		if err != nil {
			return err
		}
		var message signalMessage
		if err := json.Unmarshal(data, &message); err != nil {
			continue
		}
		switch message.Type {
		case "offer":
			if remoteDescriptionSet {
				return fmt.Errorf("renegotiation is not supported")
			}
			if err := peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: message.SDP}); err != nil {
				return fmt.Errorf("set offer: %w", err)
			}
			remoteDescriptionSet = true
			for _, candidate := range pending {
				if err := peer.AddICECandidate(candidate); err != nil {
					return fmt.Errorf("apply pending ICE candidate: %w", err)
				}
			}
			pending = nil
			answer, err := peer.CreateAnswer(nil)
			if err != nil {
				return fmt.Errorf("create answer: %w", err)
			}
			if err := peer.SetLocalDescription(answer); err != nil {
				return fmt.Errorf("set answer: %w", err)
			}
			if err := send(signalMessage{Type: "answer", SDP: answer.SDP}); err != nil {
				return fmt.Errorf("send answer: %w", err)
			}
		case "candidate":
			if message.ICE == nil {
				continue
			}
			if !remoteDescriptionSet {
				pending = append(pending, *message.ICE)
				continue
			}
			if err := peer.AddICECandidate(*message.ICE); err != nil {
				return fmt.Errorf("add ICE candidate: %w", err)
			}
		case "close", "bye":
			return nil
		}
	}
}

func (server *virtualSourceServer) play(
	ctx context.Context,
	sourceID string,
	track *webrtc.TrackLocalStaticSample,
	sender *webrtc.RTPSender,
) {
	keyframeRequests := make(chan struct{}, 1)
	go func() {
		for {
			packets, _, err := sender.ReadRTCP()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					log.Printf("source %q read RTCP: %v", sourceID, err)
				}
				return
			}
			for _, packet := range packets {
				if _, ok := packet.(*rtcp.PictureLossIndication); ok {
					select {
					case keyframeRequests <- struct{}{}:
					default:
					}
				}
			}
		}
	}()

	profile := server.playback[sourceID]
	asset := profile.asset
	ticker := time.NewTicker(asset.frameDuration)
	defer ticker.Stop()
	index := profile.startIndex
	for {
		select {
		case <-ctx.Done():
			return
		case <-keyframeRequests:
			index = nextKeyframe(asset.accessUnits, index)
		case <-ticker.C:
			unit := asset.accessUnits[index]
			if err := track.WriteSample(media.Sample{Data: unit.data, Duration: asset.frameDuration}); err != nil {
				if !errors.Is(err, io.ErrClosedPipe) {
					log.Printf("source %q write H.264 sample: %v", sourceID, err)
				}
				return
			}
			index = (index + 1) % len(asset.accessUnits)
		}
	}
}

func (server *virtualSourceServer) playTelemetry(ctx context.Context, sourceID string, channel *webrtc.DataChannel) {
	profile := server.playback[sourceID]
	runtime := server.runtime[sourceID]
	if len(profile.telemetry) == 0 {
		return
	}
	loopDuration := profile.asset.duration()
	loopStartedAt := time.Now()
	index := 0
	loopVariant := 0
	for {
		due := loopStartedAt.Add(profile.telemetry[index].offset)
		delay := time.Until(due)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
		message := profile.telemetry[index].data
		if loopVariant == 1 && profile.telemetry[index].alternateData != "" {
			message = profile.telemetry[index].alternateData
		}
		if channel.BufferedAmount() >= telemetryHighWatermark {
			runtime.telemetryDropped.Add(1)
		} else if err := channel.SendText(message); err != nil {
			runtime.telemetrySendErrors.Add(1)
			return
		} else {
			runtime.telemetrySent.Add(1)
		}
		index++
		if index == len(profile.telemetry) {
			index = 0
			loopVariant = 1 - loopVariant
			loopStartedAt = loopStartedAt.Add(loopDuration)
			if time.Since(loopStartedAt) > loopDuration {
				loopStartedAt = time.Now()
			}
		}
	}
}

func nextKeyframe(units []h264AccessUnit, current int) int {
	for offset := 0; offset < len(units); offset++ {
		index := (current + offset) % len(units)
		if units[index].keyframe {
			return index
		}
	}
	return 0
}
