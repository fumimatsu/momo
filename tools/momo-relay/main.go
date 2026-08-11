package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

//go:embed web
var webAssets embed.FS

const (
	commandLabel   = "momo-command"
	telemetryLabel = "momo-telemetry"
	raceLabel      = "momo-race"
	driveLabel     = "momo-drive"
	eventsLabel    = "momo-events"
	upstreamLabel  = "serial"

	defaultRTPStallTimeout       = 5 * time.Second
	defaultUpstreamStartTimeout  = 20 * time.Second
	defaultPilotCommandTimeout   = 250 * time.Millisecond
	commandDropLogInterval       = time.Second
	telemetryDeliveryLogInterval = 5 * time.Second
	telemetryDataHighWatermark   = uint64(64 * 1024)
	raceSnapshotRefreshInterval  = 500 * time.Millisecond
	keyframeRecoveryGrace        = 2 * time.Second
	defaultVideoTimestampStep    = uint32(90000 / 50)
	operationsPollWindow         = time.Second
)

var h264Codec = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeH264,
	ClockRate:   90000,
	SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
}

type signalMessage struct {
	Type        string                   `json:"type"`
	Data        string                   `json:"data,omitempty"`
	SDP         string                   `json:"sdp,omitempty"`
	ICE         *webrtc.ICECandidateInit `json:"ice,omitempty"`
	Error       string                   `json:"error,omitempty"`
	Reason      string                   `json:"reason,omitempty"`
	RoomID      string                   `json:"roomId,omitempty"`
	ClientID    string                   `json:"clientId,omitempty"`
	Key         string                   `json:"key,omitempty"`
	IsExistUser bool                     `json:"isExistUser,omitempty"`
	ICEServers  []webrtc.ICEServer       `json:"iceServers,omitempty"`
}

type viewer struct {
	id                  uint64
	role                string
	remoteAddr          string
	pc                  *webrtc.PeerConnection
	state               atomic.Int32
	telemetry           atomic.Pointer[webrtc.DataChannel]
	command             atomic.Pointer[webrtc.DataChannel]
	race                atomic.Pointer[webrtc.DataChannel]
	drive               atomic.Pointer[webrtc.DataChannel]
	events              atomic.Pointer[webrtc.DataChannel]
	lastCommandUnixNano atomic.Int64
	lastCommandDropLog  atomic.Int64
	lastTelemetryLog    atomic.Int64
	telemetryMessages   atomic.Uint64
	telemetryBytes      atomic.Uint64
	telemetrySendErrors atomic.Uint64
	telemetryDropped    atomic.Uint64
	telemetryWS         chan string
	eventsWS            chan string
	raceSendMu          sync.Mutex
	eventsSendMu        sync.Mutex
}

type sourceLifecycle int32

const (
	sourceWaiting sourceLifecycle = iota
	sourceConnecting
	sourceConnected
	sourceRetryWait
	sourceRecovering
)

func (state sourceLifecycle) String() string {
	switch state {
	case sourceWaiting:
		return "waiting"
	case sourceConnecting:
		return "connecting"
	case sourceConnected:
		return "connected"
	case sourceRetryWait:
		return "retry_wait"
	case sourceRecovering:
		return "recovering"
	default:
		return "waiting"
	}
}

type sourceVideoHealth int32

const (
	videoNotStarted sourceVideoHealth = iota
	videoReceiving
	videoStalled
)

func (health sourceVideoHealth) String() string {
	switch health {
	case videoReceiving:
		return "receiving"
	case videoStalled:
		return "stalled"
	default:
		return "not_started"
	}
}

type viewerConnectionState int32

const (
	viewerNegotiating viewerConnectionState = iota
	viewerConnected
)

type frameRateWindow struct {
	mu          sync.Mutex
	ingress     []time.Time
	relayWrites []time.Time
}

func (window *frameRateWindow) recordIngress(now time.Time) {
	window.mu.Lock()
	defer window.mu.Unlock()
	window.ingress = append(window.ingress, now)
	window.ingress = pruneFrameTimes(window.ingress, now)
}

func (window *frameRateWindow) recordRelayWrite(now time.Time) {
	window.mu.Lock()
	defer window.mu.Unlock()
	window.relayWrites = append(window.relayWrites, now)
	window.relayWrites = pruneFrameTimes(window.relayWrites, now)
}

func (window *frameRateWindow) snapshot(now time.Time) (float64, float64) {
	window.mu.Lock()
	defer window.mu.Unlock()
	window.ingress = pruneFrameTimes(window.ingress, now)
	window.relayWrites = pruneFrameTimes(window.relayWrites, now)
	return float64(len(window.ingress)), float64(len(window.relayWrites))
}

func pruneFrameTimes(samples []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-operationsPollWindow)
	first := 0
	for first < len(samples) && !samples[first].After(cutoff) {
		first++
	}
	return samples[first:]
}

type relay struct {
	name                 string
	upstreamURL          string
	raceCarID            string
	allowObserverCommand bool
	recorder             *telemetryRecorder

	videoTrack *webrtc.TrackLocalStaticRTP
	api        *webrtc.API

	viewersMu sync.RWMutex
	viewers   map[uint64]*viewer
	nextID    atomic.Uint64
	pilotID   uint64

	upstreamMu   sync.RWMutex
	upstreamPC   *webrtc.PeerConnection
	upstreamDC   *webrtc.DataChannel
	upstreamSSRC atomic.Uint32

	rtpStallTimeout        time.Duration
	upstreamStartTimeout   time.Duration
	upstreamGeneration     atomic.Uint64
	pilotCommandTimeout    time.Duration
	lastVideoFrameUnixNano atomic.Int64
	lastRTPTimestamp       atomic.Uint32
	lifecycle              atomic.Int32
	videoHealth            atomic.Int32
	upstreamPeerState      atomic.Value
	lastErrorCode          atomic.Value
	connectionAttempts     atomic.Uint64
	pliNewTrack            atomic.Uint64
	pliViewerConnect       atomic.Uint64
	pliWatchdog            atomic.Uint64
	rtpStalls              atomic.Uint64
	telemetryTextTEL       atomic.Uint64
	telemetryBinaryTEL     atomic.Uint64
	telemetryBinaryAudio   atomic.Uint64
	telemetryOther         atomic.Uint64
	frameRate              frameRateWindow
	vehicleHealth          *vehicleHealth
	pitPresence            *pitPresenceState
	vehicleEvents          *vehicleEventStore
	eventDispatch          chan string

	rtpRewriteMu          sync.Mutex
	rtpRewriteInitialized bool
	rtpRewriteGeneration  uint64
	rtpSequenceOffset     uint16
	rtpTimestampOffset    uint32
	lastOutputSequence    uint16
	lastOutputTimestamp   uint32
	lastInputTimestamp    uint32
	lastTimestampStep     uint32

	raceStateMu sync.RWMutex
	raceState   string

	driveLoggingEnabled atomic.Bool
	driveOwnerID        atomic.Uint64
}

type relayServer struct {
	sources     map[string]*relay
	sourceOrder []string
	recorder    *telemetryRecorder
	raceMu      sync.RWMutex
	raceContext relayRaceContext
	pitEventsMu sync.Mutex
	pitEvents   map[string]pitPresenceReceipt
	pitEventIDs []string
}

type raceStateEnvelope struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	RaceID    string `json:"raceId"`
	RaceRunID string `json:"raceRunId"`
	Phase     string `json:"phase"`
	Flag      string `json:"flag"`
	Sequence  uint64 `json:"sequence"`
}

type sourceFlag []string

func (values *sourceFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *sourceFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

type operationsAccessPolicy struct {
	networks []*net.IPNet
}

func parseOperationsAccessPolicy(values []string) (operationsAccessPolicy, error) {
	if len(values) == 0 {
		values = []string{"127.0.0.1/32", "::1/128"}
	}
	policy := operationsAccessPolicy{networks: make([]*net.IPNet, 0, len(values))}
	for _, value := range values {
		_, network, err := net.ParseCIDR(strings.TrimSpace(value))
		if err != nil {
			return operationsAccessPolicy{}, fmt.Errorf("invalid operations allow CIDR %q: %w", value, err)
		}
		policy.networks = append(policy.networks, network)
	}
	return policy, nil
}

func (policy operationsAccessPolicy) allows(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, network := range policy.networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func (policy operationsAccessPolicy) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if !policy.allows(req.RemoteAddr) {
			http.Error(w, "operations access denied", http.StatusForbidden)
			return
		}
		next(w, req)
	}
}

type operationsStatus struct {
	Version    int                     `json:"version"`
	ServerTime time.Time               `json:"serverTime"`
	Sources    []sourceOperationsState `json:"sources"`
}

// pilotDevicesStatus は Pilot 用の車両選択画面だけに渡す縮約状態です。
// 運営用 status の復旧回数・エラーコード・DataChannel 診断は含めません。
type pilotDevicesStatus struct {
	Version    int                 `json:"version"`
	ServerTime time.Time           `json:"serverTime"`
	Devices    []pilotDeviceStatus `json:"devices"`
}

type pilotDeviceStatus struct {
	Device       string  `json:"device"`
	CarID        string  `json:"carId"`
	Availability string  `json:"availability"`
	State        string  `json:"state"`
	VideoFPS     float64 `json:"videoFps"`
	PilotInUse   bool    `json:"pilotInUse"`
}

type sourceOperationsState struct {
	ID            string                       `json:"id"`
	RaceCarID     string                       `json:"raceCarId,omitempty"`
	State         string                       `json:"state"`
	Lifecycle     string                       `json:"lifecycle"`
	VideoHealth   string                       `json:"videoHealth"`
	VehicleHealth vehicleHealthOperationsState `json:"vehicleHealth"`
	Upstream      upstreamOperationsState      `json:"upstream"`
	Telemetry     telemetryOperationsState     `json:"telemetry"`
	Downstream    downstreamOperationsState    `json:"downstream"`
	Recovery      recoveryOperationsState      `json:"recovery"`
}

type upstreamOperationsState struct {
	PeerState               string  `json:"peerState"`
	SerialOpen              bool    `json:"serialOpen"`
	LastRtpAgeMs            *int64  `json:"lastRtpAgeMs"`
	IngressAccessUnitFPS    float64 `json:"ingressAccessUnitFps"`
	RelayWriteAccessUnitFPS float64 `json:"relayWriteAccessUnitFps"`
	Generation              uint64  `json:"generation"`
	StallTimeoutMs          int64   `json:"stallTimeoutMs"`
	StartTimeoutMs          int64   `json:"startTimeoutMs"`
}

// DataChannel の payload 種別を source ごとに数える。音声追加後に TEL が
// binary 化されていないかを、実走中でも安全に切り分けるための診断値。
type telemetryOperationsState struct {
	TextTEL     uint64 `json:"textTel"`
	BinaryTEL   uint64 `json:"binaryTel"`
	BinaryAudio uint64 `json:"binaryAudio"`
	Other       uint64 `json:"other"`
}

type vehicleHealthOperationsState struct {
	HP           float64 `json:"hp"`
	SpeedCap     float64 `json:"speedCap"`
	Mode         string  `json:"mode"`
	RecoveryMode string  `json:"recoveryMode"`
}

type downstreamOperationsState struct {
	PilotLeaseReserved bool `json:"pilotLeaseReserved"`
	NegotiatingPeers   int  `json:"negotiatingPeers"`
	ConnectedPilots    int  `json:"connectedPilots"`
	ConnectedObservers int  `json:"connectedObservers"`
	TelemetryOpen      int  `json:"telemetryChannelsOpen"`
	RaceOpen           int  `json:"raceChannelsOpen"`
	EventsOpen         int  `json:"eventsChannelsOpen"`
}

type pliRequestCounts struct {
	NewTrack      uint64 `json:"newTrack"`
	ViewerConnect uint64 `json:"viewerConnect"`
	Watchdog      uint64 `json:"watchdog"`
}

type recoveryOperationsState struct {
	PLIRequests   pliRequestCounts `json:"pliRequests"`
	RTPStalls     uint64           `json:"rtpStalls"`
	RetryAttempts uint64           `json:"retryAttempts"`
	LastErrorCode *string          `json:"lastErrorCode"`
}

func newRelay(name string, upstreamURL string, raceCarID string, allowObserverCommand bool,
	rtpStallTimeout time.Duration, upstreamStartTimeout time.Duration,
	healthRecoveryMode vehicleHealthRecoveryMode) (*relay, error) {
	api, err := newH264API()
	if err != nil {
		return nil, err
	}
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(h264Codec, "video", "momo")
	if err != nil {
		return nil, fmt.Errorf("create local H264 track: %w", err)
	}
	relay := &relay{
		name:                 name,
		upstreamURL:          upstreamURL,
		raceCarID:            raceCarID,
		allowObserverCommand: allowObserverCommand,
		videoTrack:           videoTrack,
		api:                  api,
		viewers:              make(map[uint64]*viewer),
		rtpStallTimeout:      rtpStallTimeout,
		upstreamStartTimeout: upstreamStartTimeout,
		pilotCommandTimeout:  defaultPilotCommandTimeout,
		vehicleHealth:        newVehicleHealth(time.Now()),
		vehicleEvents:        newVehicleEventStore(),
		eventDispatch:        make(chan string, vehicleEventQueueLimit),
	}
	relay.pitPresence = newPitPresenceState(raceCarID, vehicleHealthMaximum)
	relay.lifecycle.Store(int32(sourceWaiting))
	relay.videoHealth.Store(int32(videoNotStarted))
	relay.upstreamPeerState.Store("new")
	relay.lastErrorCode.Store("")
	relay.vehicleHealth.setRecoveryMode(healthRecoveryMode)
	return relay, nil
}

func newH264API() (*webrtc.API, error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    h264Codec.MimeType,
			ClockRate:   h264Codec.ClockRate,
			SDPFmtpLine: h264Codec.SDPFmtpLine,
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, fmt.Errorf("register H264 codec: %w", err)
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine)), nil
}

func (r *relay) start(ctx context.Context) {
	go r.watchPilotCommands(ctx)
	go r.runVehicleEventDispatcher(ctx)
	go func() {
		for {
			r.lifecycle.Store(int32(sourceConnecting))
			r.videoHealth.Store(int32(videoNotStarted))
			r.connectionAttempts.Add(1)
			err := r.connectUpstream(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				if sourceLifecycle(r.lifecycle.Load()) != sourceRecovering {
					r.lifecycle.Store(int32(sourceRetryWait))
				}
				log.Printf("upstream disconnected: %v; retrying in 3 seconds", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}()
}

func (r *relay) watchPilotCommands(ctx context.Context) {
	interval := r.pilotCommandTimeout / 2
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			r.viewersMu.RLock()
			pilotID := r.pilotID
			pilot := r.viewers[pilotID]
			r.viewersMu.RUnlock()
			if pilot == nil || pilot.command.Load() == nil {
				continue
			}
			lastCommand := pilot.lastCommandUnixNano.Load()
			if lastCommand == 0 || now.Sub(time.Unix(0, lastCommand)) >= r.pilotCommandTimeout {
				r.sendNeutralToUpstream("pilot command timeout")
			}
		}
	}
}

func (r *relay) connectUpstream(ctx context.Context) error {
	log.Printf("source %q: connecting upstream Momo: %s", r.name, r.upstreamURL)
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, r.upstreamURL, nil)
	if err != nil {
		r.setLastErrorCode("upstream_signaling_failed")
		return fmt.Errorf("connect upstream signaling: %w", err)
	}
	defer ws.Close()

	pc, err := r.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		r.setLastErrorCode("upstream_peer_failed")
		return fmt.Errorf("create upstream peer connection: %w", err)
	}
	defer pc.Close()
	generation := r.upstreamGeneration.Add(1)
	r.lastVideoFrameUnixNano.Store(0)
	r.lastRTPTimestamp.Store(0)
	r.upstreamSSRC.Store(0)
	r.upstreamPeerState.Store("new")

	var writeMu sync.Mutex
	sendSignal := func(message signalMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteJSON(message)
	}
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		candidateJSON := candidate.ToJSON()
		if err := sendSignal(signalMessage{Type: "candidate", ICE: &candidateJSON}); err != nil {
			log.Printf("send upstream ICE candidate: %v", err)
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if r.upstreamGeneration.Load() != generation {
			return
		}
		r.upstreamPeerState.Store(state.String())
		if state == webrtc.PeerConnectionStateConnected {
			r.lifecycle.Store(int32(sourceConnected))
		}
		if state == webrtc.PeerConnectionStateFailed {
			r.setLastErrorCode("upstream_peer_failed")
			r.lifecycle.Store(int32(sourceRetryWait))
			_ = ws.Close()
		}
		log.Printf("source %q: upstream peer connection state: %s", r.name, state.String())
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if r.upstreamGeneration.Load() != generation {
			return
		}
		if track.Kind() != webrtc.RTPCodecTypeVideo {
			return
		}
		if !strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeH264) {
			log.Printf("ignore unsupported upstream video codec: %s", track.Codec().MimeType)
			return
		}
		r.upstreamSSRC.Store(uint32(track.SSRC()))
		log.Printf("source %q: receiving upstream H264 track: SSRC=%d codec=%s", r.name, track.SSRC(), track.Codec().SDPFmtpLine)
		// Momo の再起動後は既存 Viewer の接続が維持されるため、
		// Viewer 接続時だけの PLI では復号器が差分フレームを受け続ける。
		// 新しい上流トラックを受けた時点でも IDR を要求する。
		r.requestKeyframe("new_track")
		go func() {
			for _, delay := range []time.Duration{time.Second, 3 * time.Second} {
				time.Sleep(delay)
				r.requestKeyframe("new_track")
			}
		}()
		for {
			packet, _, err := track.ReadRTP()
			if err != nil {
				log.Printf("upstream H264 RTP ended: %v", err)
				return
			}
			if r.upstreamGeneration.Load() == generation {
				previousTimestamp := r.lastRTPTimestamp.Swap(packet.Timestamp)
				if r.lastVideoFrameUnixNano.Load() == 0 ||
					previousTimestamp != packet.Timestamp {
					r.lastVideoFrameUnixNano.Store(time.Now().UnixNano())
					r.videoHealth.Store(int32(videoReceiving))
					r.setLastErrorCode("")
				}
			}
			if packet.Marker {
				r.frameRate.recordIngress(time.Now())
			}
			if !r.rewriteRTPHeader(generation, &packet.Header) {
				return
			}
			if err := r.videoTrack.WriteRTP(packet); err != nil {
				if !errors.Is(err, io.ErrClosedPipe) {
					r.setLastErrorCode("relay_write_failed")
					log.Printf("fan out upstream RTP: %v", err)
				}
			} else if packet.Marker {
				r.frameRate.recordRelayWrite(time.Now())
			}
		}
	})

	upstreamDC, err := pc.CreateDataChannel(upstreamLabel, nil)
	if err != nil {
		r.setLastErrorCode("upstream_data_channel_failed")
		return fmt.Errorf("create upstream data channel: %w", err)
	}
	upstreamDC.OnOpen(func() {
		if r.upstreamGeneration.Load() != generation {
			return
		}
		r.upstreamMu.Lock()
		r.upstreamPC = pc
		r.upstreamDC = upstreamDC
		r.upstreamMu.Unlock()
		log.Printf("source %q: upstream DataChannel %q opened", r.name, upstreamLabel)
	})
	upstreamDC.OnMessage(func(message webrtc.DataChannelMessage) {
		r.handleUpstreamTelemetry(message, generation)
	})
	upstreamDC.OnClose(func() {
		r.clearUpstream(pc)
		log.Printf("source %q: upstream DataChannel %q closed", r.name, upstreamLabel)
	})

	_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	})
	if err != nil {
		r.setLastErrorCode("upstream_peer_failed")
		return fmt.Errorf("add upstream recvonly video transceiver: %w", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		r.setLastErrorCode("upstream_peer_failed")
		return fmt.Errorf("create upstream offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		r.setLastErrorCode("upstream_peer_failed")
		return fmt.Errorf("set upstream local description: %w", err)
	}
	if err := sendSignal(signalMessage{Type: "offer", SDP: offer.SDP}); err != nil {
		r.setLastErrorCode("upstream_signaling_failed")
		return fmt.Errorf("send upstream offer: %w", err)
	}
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go r.watchUpstreamRTP(ctx, generation, pc, ws, watchdogDone)

	var pendingCandidates []webrtc.ICECandidateInit
	remoteDescriptionSet := false
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			r.clearUpstream(pc)
			if sourceLifecycle(r.lifecycle.Load()) != sourceRecovering {
				r.setLastErrorCode("upstream_signaling_failed")
			}
			return fmt.Errorf("read upstream signaling: %w", err)
		}
		var message signalMessage
		if err := json.Unmarshal(data, &message); err != nil {
			log.Printf("ignore malformed upstream signaling: %v", err)
			continue
		}
		switch message.Type {
		case "answer":
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: message.SDP}); err != nil {
				return fmt.Errorf("set upstream answer: %w", err)
			}
			remoteDescriptionSet = true
			for _, candidate := range pendingCandidates {
				if err := pc.AddICECandidate(candidate); err != nil {
					log.Printf("apply pending upstream ICE candidate: %v", err)
				}
			}
			pendingCandidates = nil
		case "candidate":
			if message.ICE == nil {
				continue
			}
			if !remoteDescriptionSet {
				pendingCandidates = append(pendingCandidates, *message.ICE)
				continue
			}
			if err := pc.AddICECandidate(*message.ICE); err != nil {
				log.Printf("apply upstream ICE candidate: %v", err)
			}
		case "close", "bye":
			r.clearUpstream(pc)
			r.setLastErrorCode("upstream_signaling_closed")
			return errors.New("upstream closed signaling session")
		}
	}
}

// TrackLocalStaticRTP は出力 SSRC だけを Viewer ごとに置き換え、上流の
// sequence number と timestamp はそのまま送る。Momo を再起動するとこれらが
// 先頭へ戻るため、接続済み Viewer の jitter buffer は再接続後の RTP を古い
// パケットとして捨てる。source 世代をまたぐ時だけ offset を更新し、下流で
// sequence number と timestamp が連続するようにする。
func (r *relay) rewriteRTPHeader(generation uint64, header *rtp.Header) bool {
	r.rtpRewriteMu.Lock()
	defer r.rtpRewriteMu.Unlock()

	if r.upstreamGeneration.Load() != generation {
		return false
	}

	if !r.rtpRewriteInitialized {
		r.rtpRewriteInitialized = true
		r.rtpRewriteGeneration = generation
		r.lastInputTimestamp = header.Timestamp
		r.lastTimestampStep = defaultVideoTimestampStep
	} else if r.rtpRewriteGeneration != generation {
		r.rtpSequenceOffset = r.lastOutputSequence + 1 - header.SequenceNumber
		r.rtpTimestampOffset = r.lastOutputTimestamp + r.lastTimestampStep - header.Timestamp
		r.rtpRewriteGeneration = generation
		r.lastInputTimestamp = header.Timestamp
	} else if header.Timestamp != r.lastInputTimestamp {
		r.lastTimestampStep = header.Timestamp - r.lastInputTimestamp
		r.lastInputTimestamp = header.Timestamp
	}

	header.SequenceNumber += r.rtpSequenceOffset
	header.Timestamp += r.rtpTimestampOffset
	r.lastOutputSequence = header.SequenceNumber
	r.lastOutputTimestamp = header.Timestamp
	return true
}

// WebRTC の PeerConnection は ICE が connected のままでも、RTP の受信だけが
// 無期限に止まることがある。停止後も古い RTP の再送が届く場合があるため、
// パケット到着ではなく RTP timestamp が進んだ最終時刻を監視する。止まった
// source の WebSocket と PeerConnection を閉じ、既存の再接続ループへ制御を戻す。
func (r *relay) watchUpstreamRTP(ctx context.Context, generation uint64,
	pc *webrtc.PeerConnection, ws *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	startedAt := time.Now()
	var keyframeRequestedAt time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case now := <-ticker.C:
			if r.upstreamGeneration.Load() != generation {
				return
			}

			lastVideoFrame := r.lastVideoFrameUnixNano.Load()
			stallTimeout := r.upstreamStartTimeout
			stalledFor := now.Sub(startedAt)
			if lastVideoFrame != 0 {
				stallTimeout = r.rtpStallTimeout
				stalledFor = now.Sub(time.Unix(0, lastVideoFrame))
			}
			if stalledFor < stallTimeout {
				keyframeRequestedAt = time.Time{}
				continue
			}

			if keyframeRequestedAt.IsZero() {
				keyframeRequestedAt = now
				r.videoHealth.Store(int32(videoStalled))
				r.rtpStalls.Add(1)
				log.Printf("source %q: no upstream video frames for %s; request keyframe before reconnect",
					r.name, stalledFor.Round(time.Millisecond))
				r.requestKeyframe("watchdog")
				continue
			}
			if now.Sub(keyframeRequestedAt) < keyframeRecoveryGrace {
				continue
			}

			log.Printf("source %q: upstream video frames remained stalled for %s; reconnecting source",
				r.name, stalledFor.Round(time.Millisecond))
			r.setLastErrorCode("upstream_rtp_stalled")
			r.lifecycle.Store(int32(sourceRecovering))
			r.clearUpstream(pc)
			_ = pc.Close()
			_ = ws.Close()
			return
		}
	}
}

func (r *relay) clearUpstream(pc *webrtc.PeerConnection) {
	r.upstreamMu.Lock()
	defer r.upstreamMu.Unlock()
	if r.upstreamPC == pc {
		r.upstreamPC = nil
		r.upstreamDC = nil
	}
}

func (r *relay) setLastErrorCode(code string) {
	r.lastErrorCode.Store(code)
}

func (r *relay) statusSnapshot(now time.Time) sourceOperationsState {
	lifecycle := sourceLifecycle(r.lifecycle.Load())
	videoHealth := sourceVideoHealth(r.videoHealth.Load())
	peerState, _ := r.upstreamPeerState.Load().(string)
	lastError, _ := r.lastErrorCode.Load().(string)
	lastFrame := r.lastVideoFrameUnixNano.Load()
	var lastRtpAgeMs *int64
	if lastFrame != 0 {
		age := now.Sub(time.Unix(0, lastFrame)).Milliseconds()
		if age < 0 {
			age = 0
		}
		lastRtpAgeMs = &age
	}
	ingressFPS, relayWriteFPS := r.frameRate.snapshot(now)
	health := r.vehicleHealth.snapshot(now)

	r.upstreamMu.RLock()
	serialOpen := r.upstreamDC != nil
	r.upstreamMu.RUnlock()

	attempts := r.connectionAttempts.Load()
	retries := uint64(0)
	if attempts > 0 {
		retries = attempts - 1
	}
	var lastErrorCode *string
	if lastError != "" {
		lastErrorCode = &lastError
	}

	return sourceOperationsState{
		ID:          r.name,
		RaceCarID:   r.raceCarID,
		State:       displaySourceState(lifecycle, videoHealth),
		Lifecycle:   lifecycle.String(),
		VideoHealth: videoHealth.String(),
		VehicleHealth: vehicleHealthOperationsState{
			HP:           health.HP,
			SpeedCap:     health.SpeedCap,
			Mode:         health.Mode,
			RecoveryMode: string(r.vehicleHealth.recoveryModeSnapshot()),
		},
		Upstream: upstreamOperationsState{
			PeerState:               peerState,
			SerialOpen:              serialOpen,
			LastRtpAgeMs:            lastRtpAgeMs,
			IngressAccessUnitFPS:    ingressFPS,
			RelayWriteAccessUnitFPS: relayWriteFPS,
			Generation:              r.upstreamGeneration.Load(),
			StallTimeoutMs:          r.rtpStallTimeout.Milliseconds(),
			StartTimeoutMs:          r.upstreamStartTimeout.Milliseconds(),
		},
		Telemetry: telemetryOperationsState{
			TextTEL:     r.telemetryTextTEL.Load(),
			BinaryTEL:   r.telemetryBinaryTEL.Load(),
			BinaryAudio: r.telemetryBinaryAudio.Load(),
			Other:       r.telemetryOther.Load(),
		},
		Downstream: r.downstreamStatusSnapshot(),
		Recovery: recoveryOperationsState{
			PLIRequests: pliRequestCounts{
				NewTrack:      r.pliNewTrack.Load(),
				ViewerConnect: r.pliViewerConnect.Load(),
				Watchdog:      r.pliWatchdog.Load(),
			},
			RTPStalls:     r.rtpStalls.Load(),
			RetryAttempts: retries,
			LastErrorCode: lastErrorCode,
		},
	}
}

func (r *relay) downstreamStatusSnapshot() downstreamOperationsState {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	state := downstreamOperationsState{PilotLeaseReserved: r.pilotID != 0}
	for _, client := range r.viewers {
		if viewerConnectionState(client.state.Load()) == viewerConnected {
			if client.role == "pilot" {
				state.ConnectedPilots++
			} else {
				state.ConnectedObservers++
			}
		} else {
			state.NegotiatingPeers++
		}
		if client.telemetry.Load() != nil {
			state.TelemetryOpen++
		}
		if client.race.Load() != nil {
			state.RaceOpen++
		}
		if client.events.Load() != nil {
			state.EventsOpen++
		}
	}
	return state
}

func displaySourceState(lifecycle sourceLifecycle, videoHealth sourceVideoHealth) string {
	switch lifecycle {
	case sourceRecovering:
		return "RECOVERING"
	case sourceRetryWait:
		return "DISCONNECTED"
	case sourceWaiting:
		return "WAITING"
	case sourceConnected:
		switch videoHealth {
		case videoStalled:
			return "STALE"
		case videoReceiving:
			return "STREAMING"
		default:
			return "CONNECTING"
		}
	default:
		return "CONNECTING"
	}
}

func (server *relayServer) operationsStatusSnapshot(now time.Time) operationsStatus {
	sources := make([]sourceOperationsState, 0, len(server.sourceOrder))
	for _, sourceID := range server.sourceOrder {
		source, ok := server.sources[sourceID]
		if !ok {
			continue
		}
		sources = append(sources, source.statusSnapshot(now))
	}
	return operationsStatus{Version: 1, ServerTime: now.UTC(), Sources: sources}
}

func (server *relayServer) serveOperationsStatus(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(server.operationsStatusSnapshot(time.Now()))
}

func (server *relayServer) serveRaceState(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	for _, sourceID := range server.sourceOrder {
		source, ok := server.sources[sourceID]
		if !ok {
			continue
		}
		if state := strings.TrimSpace(source.currentRaceState()); state != "" {
			state = strings.TrimPrefix(state, "RACE:")
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(state))
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (server *relayServer) pilotDevicesSnapshot(now time.Time) pilotDevicesStatus {
	devices := make([]pilotDeviceStatus, 0, len(server.sourceOrder))
	for _, sourceID := range server.sourceOrder {
		source, ok := server.sources[sourceID]
		if !ok {
			continue
		}
		status := source.statusSnapshot(now)
		pilotInUse := status.Downstream.PilotLeaseReserved || status.Downstream.ConnectedPilots > 0
		videoFPS := status.Upstream.RelayWriteAccessUnitFPS
		if videoFPS == 0 {
			videoFPS = status.Upstream.IngressAccessUnitFPS
		}
		devices = append(devices, pilotDeviceStatus{
			Device:       status.ID,
			CarID:        status.RaceCarID,
			Availability: pilotDeviceAvailability(status.State, pilotInUse),
			State:        status.State,
			VideoFPS:     videoFPS,
			PilotInUse:   pilotInUse,
		})
	}
	return pilotDevicesStatus{Version: 1, ServerTime: now.UTC(), Devices: devices}
}

func pilotDeviceAvailability(state string, pilotInUse bool) string {
	if pilotInUse {
		return "in_use"
	}
	if state == "STREAMING" {
		return "ready"
	}
	if state == "WAITING" || state == "CONNECTING" {
		return "connecting"
	}
	return "unavailable"
}

func (server *relayServer) servePilotDevices(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(server.pilotDevicesSnapshot(time.Now()))
}

func operationsPageHandler(operationsHTML []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(operationsHTML)
	}
}

// 新しい Viewer は relay に蓄積されていない差分フレームから受信を始める。
// 上流 Momo に IDR を要求しないと、次の自然発生キーフレームまで映像を
// 復号できず、黒画面のままになる。
func (r *relay) requestKeyframe(reason string) {
	r.upstreamMu.RLock()
	pc := r.upstreamPC
	ssrc := r.upstreamSSRC.Load()
	r.upstreamMu.RUnlock()
	if pc == nil || ssrc == 0 {
		return
	}
	switch reason {
	case "new_track":
		r.pliNewTrack.Add(1)
	case "viewer_connect":
		r.pliViewerConnect.Add(1)
	case "watchdog":
		r.pliWatchdog.Add(1)
	}
	if err := pc.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{SenderSSRC: 1, MediaSSRC: ssrc},
	}); err != nil {
		log.Printf("source %q: request upstream keyframe: %v", r.name, err)
	} else {
		log.Printf("source %q: requested upstream keyframe for SSRC=%d", r.name, ssrc)
	}
}

func (r *relay) broadcastTelemetry(message webrtc.DataChannelMessage) {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	for _, client := range r.viewers {
		if client.role == "pilot" && message.IsString && client.telemetryWS != nil {
			enqueueLatestTelemetry(client.telemetryWS, string(message.Data))
			client.telemetryMessages.Add(1)
			client.telemetryBytes.Add(uint64(len(message.Data)))
			if shouldLogTelemetryDelivery(client, time.Now()) {
				log.Printf("source %q: telemetry delivery viewer=%d role=%s remote=%s transport=websocket messages=%d bytes=%d errors=%d",
					r.name, client.id, client.role, client.remoteAddr,
					client.telemetryMessages.Load(), client.telemetryBytes.Load(), client.telemetrySendErrors.Load())
			}
			continue
		}
		if channel := client.telemetry.Load(); channel != nil {
			if telemetryDataChannelSaturated(channel.BufferedAmount()) {
				client.telemetryDropped.Add(1)
				if client.role == "pilot" && shouldLogTelemetryDelivery(client, time.Now()) {
					log.Printf("source %q: telemetry delivery viewer=%d role=%s remote=%s transport=datachannel action=drop buffered=%d dropped=%d errors=%d",
						r.name, client.id, client.role, client.remoteAddr, channel.BufferedAmount(),
						client.telemetryDropped.Load(), client.telemetrySendErrors.Load())
				}
				continue
			}
			if err := sendDataChannel(channel, message); err != nil {
				client.telemetrySendErrors.Add(1)
				log.Printf("send telemetry to viewer %d: %v", client.id, err)
			} else {
				client.telemetryMessages.Add(1)
				client.telemetryBytes.Add(uint64(len(message.Data)))
				if client.role == "pilot" && shouldLogTelemetryDelivery(client, time.Now()) {
					log.Printf("source %q: telemetry delivery viewer=%d role=%s remote=%s messages=%d bytes=%d state=%s buffered=%d errors=%d",
						r.name, client.id, client.role, client.remoteAddr,
						client.telemetryMessages.Load(), client.telemetryBytes.Load(),
						channel.ReadyState().String(), channel.BufferedAmount(), client.telemetrySendErrors.Load())
				}
			}
		}
	}
}

func telemetryDataChannelSaturated(buffered uint64) bool {
	return buffered >= telemetryDataHighWatermark
}

func enqueueLatestTelemetry(queue chan string, payload string) {
	select {
	case queue <- payload:
		return
	default:
	}
	select {
	case <-queue:
	default:
	}
	select {
	case queue <- payload:
	default:
	}
}

func shouldLogTelemetryDelivery(client *viewer, now time.Time) bool {
	if client == nil {
		return false
	}
	current := now.UnixNano()
	for {
		previous := client.lastTelemetryLog.Load()
		if previous != 0 && current-previous < telemetryDeliveryLogInterval.Nanoseconds() {
			return false
		}
		if client.lastTelemetryLog.CompareAndSwap(previous, current) {
			return true
		}
	}
}

func normalizeTelemetryMessage(message webrtc.DataChannelMessage) (webrtc.DataChannelMessage, string, bool, bool) {
	if message.IsString && strings.HasPrefix(string(message.Data), "TEL:") {
		return message, string(message.Data), true, false
	}
	if !message.IsString && bytes.HasPrefix(message.Data, []byte("TEL:")) {
		// Momo の serial DataChannel は、同じ TEL 行でも機体ごとに binary
		// として届くことがある。Viewer と NDJSON 契約は Text TEL を前提と
		// するため、内容が TEL: である場合だけ Relay 境界で正規化する。
		message.IsString = true
		return message, string(message.Data), true, true
	}
	return message, "", false, false
}

func (r *relay) handleUpstreamTelemetry(message webrtc.DataChannelMessage, generation uint64) {
	normalized, raw, isTEL, wasBinaryTEL := normalizeTelemetryMessage(message)
	if isTEL {
		if wasBinaryTEL {
			r.telemetryBinaryTEL.Add(1)
		} else {
			r.telemetryTextTEL.Add(1)
		}
		if r.recorder != nil && r.driveLoggingEnabled.Load() {
			r.recorder.RecordTelemetry(r.name, r.raceCarID, generation, raw)
		}
		message = normalized
		health, publish, event := r.vehicleHealth.ingestTelemetry(raw, r.raceCarID, time.Now())
		if event != nil {
			r.publishVehicleEvent(*event)
		} else if isLegacyImpactEvent(raw) {
			log.Printf("source %q: ignore diagnostic V1 impact event: legacy_event_unsupported", r.name)
		}
		if publish {
			r.broadcastVehicleHealth(health)
			if r.pitPresence != nil {
				if pit, changed := r.pitPresence.observeHealth(health); changed {
					r.broadcastPitPresence(pit)
				}
			}
		}
	} else if bytes.HasPrefix(message.Data, []byte("AUD:")) {
		r.telemetryBinaryAudio.Add(1)
	} else {
		r.telemetryOther.Add(1)
	}
	r.broadcastTelemetry(message)
}

func (r *relay) broadcastVehicleHealth(health vehicleHealthSnapshot) {
	r.broadcastTelemetry(webrtc.DataChannelMessage{
		Data:     []byte(formatVehicleHealthTelemetry(health)),
		IsString: true,
	})
}

// Race Control の状態は操縦テレメトリーと分離した reliable DataChannel で配る。
// 順位やフラグは最新値を確実に渡す必要があり、低遅延・非信頼の telemetry channel
// に混在させると再送されず、Viewer が古い状態のまま残るためである。
func (r *relay) publishRaceState(message string, phase string) {
	r.raceStateMu.Lock()
	r.raceState = message
	r.raceStateMu.Unlock()
	r.broadcastRaceState(message)
	if health, reset := r.vehicleHealth.observeRacePhase(phase, time.Now()); reset {
		r.broadcastVehicleHealth(health)
	}
}

func (r *relay) currentRaceState() string {
	r.raceStateMu.RLock()
	defer r.raceStateMu.RUnlock()
	return r.raceState
}

func (r *relay) broadcastRaceState(message string) {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	for _, client := range r.viewers {
		if channel := client.race.Load(); channel != nil {
			r.sendRaceState(client, channel, message)
		}
	}
}

func (r *relay) publishVehicleEvent(event vehicleImpactEvent) {
	if r.vehicleEvents == nil || !r.vehicleEvents.add(event) {
		return
	}
	message, err := marshalVehicleEvent(event)
	if err != nil {
		log.Printf("source %q: encode vehicle event %q: %v", r.name, event.EventID, err)
		return
	}
	r.enqueueVehicleEventMessage(message)
}

func (r *relay) resetVehicleEvents(raceRunID string) {
	if r.vehicleEvents == nil || !r.vehicleEvents.reset(raceRunID) {
		return
	}
	message, err := marshalVehicleEvent(r.vehicleEvents.snapshot())
	if err != nil {
		log.Printf("source %q: encode empty vehicle event snapshot: %v", r.name, err)
		return
	}
	r.enqueueVehicleEventMessage(message)
}

func (r *relay) enqueueVehicleEventMessage(message string) {
	if r.eventDispatch == nil {
		return
	}
	select {
	case r.eventDispatch <- message:
	default:
		log.Printf("source %q: drop vehicle event delivery because the bounded queue is full", r.name)
	}
}

func (r *relay) runVehicleEventDispatcher(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case message := <-r.eventDispatch:
			r.broadcastVehicleEventMessage(message)
		}
	}
}

func (r *relay) broadcastVehicleEventMessage(message string) {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	for _, client := range r.viewers {
		if client.role == "pilot" && client.eventsWS != nil {
			if !enqueueVehicleEvent(client.eventsWS, message) {
				log.Printf("source %q: drop WebSocket vehicle event for viewer %d because the bounded queue is full", r.name, client.id)
			}
			continue
		}
		if channel := client.events.Load(); channel != nil {
			r.sendVehicleEventMessage(client, channel, message)
		}
	}
}

func enqueueVehicleEvent(queue chan string, message string) bool {
	select {
	case queue <- message:
		return true
	default:
		return false
	}
}

func (r *relay) sendVehicleEventMessage(client *viewer, channel *webrtc.DataChannel, message string) bool {
	client.eventsSendMu.Lock()
	defer client.eventsSendMu.Unlock()
	if client.events.Load() != channel {
		return false
	}
	if err := channel.SendText(message); err != nil {
		client.events.CompareAndSwap(channel, nil)
		log.Printf("send vehicle event to viewer %d: %v", client.id, err)
		_ = channel.Close()
		return false
	}
	return true
}

func (r *relay) openVehicleEventsChannel(client *viewer, channel *webrtc.DataChannel) {
	client.eventsSendMu.Lock()
	defer client.eventsSendMu.Unlock()
	client.events.Store(channel)
	message, err := marshalVehicleEvent(r.vehicleEvents.snapshot())
	if err != nil {
		log.Printf("source %q: encode vehicle event snapshot: %v", r.name, err)
		return
	}
	if client.role == "pilot" && client.eventsWS != nil {
		if !enqueueVehicleEvent(client.eventsWS, message) {
			log.Printf("source %q: drop WebSocket vehicle event snapshot for viewer %d because the bounded queue is full", r.name, client.id)
		}
		return
	}
	if err := channel.SendText(message); err != nil {
		client.events.CompareAndSwap(channel, nil)
		log.Printf("send vehicle event snapshot to viewer %d: %v", client.id, err)
		_ = channel.Close()
	}
}

func (r *relay) sendRaceState(client *viewer, channel *webrtc.DataChannel, message string) bool {
	client.raceSendMu.Lock()
	defer client.raceSendMu.Unlock()
	if client.race.Load() != channel {
		return false
	}
	if err := channel.SendText(message); err != nil {
		client.race.CompareAndSwap(channel, nil)
		log.Printf("send race state to viewer %d: %v", client.id, err)
		_ = channel.Close()
		return false
	}
	return true
}

func (r *relay) sendCurrentRaceState(client *viewer, channel *webrtc.DataChannel) {
	message := r.currentRaceState()
	if message == "" {
		return
	}
	r.sendRaceState(client, channel, message)
}

func (r *relay) refreshRaceState(client *viewer, channel *webrtc.DataChannel) {
	go func() {
		ticker := time.NewTicker(raceSnapshotRefreshInterval)
		defer ticker.Stop()
		for range ticker.C {
			if client.race.Load() != channel {
				return
			}
			r.sendCurrentRaceState(client, channel)
		}
	}()
}

func (r *relay) broadcastCommand(message webrtc.DataChannelMessage) {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	for _, client := range r.viewers {
		if channel := client.command.Load(); channel != nil {
			if err := sendDataChannel(channel, message); err != nil {
				log.Printf("send command audit to viewer %d: %v", client.id, err)
			}
		}
	}
}

func sendDataChannel(channel *webrtc.DataChannel, message webrtc.DataChannelMessage) error {
	if message.IsString {
		return channel.SendText(string(message.Data))
	}
	return channel.Send(message.Data)
}

func (r *relay) handleCommand(client *viewer, message webrtc.DataChannelMessage) {
	if client.role != "pilot" && !r.allowObserverCommand {
		log.Printf("drop command from observer viewer %d", client.id)
		return
	}
	r.upstreamMu.RLock()
	upstream := r.upstreamDC
	r.upstreamMu.RUnlock()
	if upstream == nil {
		if shouldLogCommandDrop(client, time.Now()) {
			log.Printf("drop command from viewer %d: upstream DataChannel is unavailable", client.id)
		}
		return
	}
	forwarded := message
	if message.IsString {
		forwarded.Data = []byte(r.vehicleHealth.limitCommand(string(message.Data), time.Now()))
	}
	if err := sendDataChannel(upstream, forwarded); err != nil {
		log.Printf("forward command from viewer %d to Momo: %v", client.id, err)
		return
	}
	client.lastCommandUnixNano.Store(time.Now().UnixNano())
	// コマンドは全員に同じ DataChannel で返す。クライアント側は受信時にのみ
	// 表示するため、この監査メッセージが Momo に再送されることはない。
	r.broadcastCommand(forwarded)
}

func shouldLogCommandDrop(client *viewer, now time.Time) bool {
	if client == nil {
		return true
	}
	current := now.UnixNano()
	for {
		previous := client.lastCommandDropLog.Load()
		if previous != 0 && current-previous < commandDropLogInterval.Nanoseconds() {
			return false
		}
		if client.lastCommandDropLog.CompareAndSwap(previous, current) {
			return true
		}
	}
}

func (r *relay) handleDriveState(client *viewer, message webrtc.DataChannelMessage) {
	if client.role != "pilot" || !r.isCurrentPilot(client.id) {
		log.Printf("drop drive state from viewer %d (%s)", client.id, client.role)
		return
	}
	if !message.IsString {
		log.Printf("drop non-text drive state from viewer %d", client.id)
		return
	}

	switch strings.TrimSpace(string(message.Data)) {
	case "DRIVE:1":
		r.setDriveLogging(client.id, true, "viewer drive on")
	case "DRIVE:0":
		r.setDriveLogging(client.id, false, "viewer drive off")
	default:
		log.Printf("drop invalid drive state from viewer %d", client.id)
	}
}

func (r *relay) isCurrentPilot(id uint64) bool {
	r.viewersMu.RLock()
	defer r.viewersMu.RUnlock()
	return r.pilotID == id
}

func (r *relay) setDriveLogging(pilotID uint64, enabled bool, reason string) {
	if enabled {
		ownerID := r.driveOwnerID.Load()
		if ownerID != 0 && ownerID != pilotID {
			log.Printf("drop drive on from viewer %d: current owner is %d", pilotID, ownerID)
			return
		}
		r.driveOwnerID.Store(pilotID)
		if r.driveLoggingEnabled.Swap(true) {
			return
		}
	} else {
		ownerID := r.driveOwnerID.Load()
		if ownerID != 0 && ownerID != pilotID {
			return
		}
		if ownerID == pilotID {
			r.driveOwnerID.CompareAndSwap(pilotID, 0)
		}
		if !r.driveLoggingEnabled.Swap(false) {
			return
		}
	}
	if r.recorder != nil {
		r.recorder.RecordDriveState(r.name, r.raceCarID, pilotID, enabled, reason)
	}
}

func (r *relay) sendNeutralToUpstream(reason string) {
	r.upstreamMu.RLock()
	upstream := r.upstreamDC
	r.upstreamMu.RUnlock()
	if upstream == nil {
		return
	}
	if err := upstream.SendText("S:1500,T:1500"); err != nil {
		log.Printf("source %q: send neutral after %s: %v", r.name, reason, err)
	}
}

func (r *relay) addViewer(client *viewer) {
	r.viewersMu.Lock()
	r.viewers[client.id] = client
	r.viewersMu.Unlock()
}

func (r *relay) reservePilot(id uint64) bool {
	r.viewersMu.Lock()
	defer r.viewersMu.Unlock()
	if r.pilotID != 0 {
		return false
	}
	r.pilotID = id
	return true
}

func (r *relay) removeViewer(id uint64) {
	wasPilot := false
	r.viewersMu.Lock()
	delete(r.viewers, id)
	if r.pilotID == id {
		r.pilotID = 0
		wasPilot = true
	}
	r.viewersMu.Unlock()
	if wasPilot {
		r.setDriveLogging(id, false, "pilot disconnected")
		r.sendNeutralToUpstream("pilot disconnect")
	}
}

// Ayame の room は source ごとに 1 つだけ割り当てる。ここでは映像の下流配信だけを
// 担当する。外部操縦は deadman / neutral failsafe が未実装のため、別段階で追加する。
func (r *relay) startAyamePilot(ctx context.Context, signalingURL string, roomID string, clientID string, key string) {
	go func() {
		for {
			err := r.connectAyamePilot(ctx, signalingURL, roomID, clientID, key)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("source %q: Ayame pilot disconnected: %v; retrying in 3 seconds", r.name, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}()
}

func (r *relay) connectAyamePilot(ctx context.Context, signalingURL string, roomID string, clientID string, key string) error {
	log.Printf("source %q: connecting Ayame external pilot room %q", r.name, roomID)
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, signalingURL, nil)
	if err != nil {
		return fmt.Errorf("connect Ayame signaling: %w", err)
	}
	defer ws.Close()

	var writeMu sync.Mutex
	sendSignal := func(message signalMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteJSON(message)
	}
	if err := sendSignal(signalMessage{Type: "register", RoomID: roomID, ClientID: clientID, Key: key}); err != nil {
		return fmt.Errorf("register Ayame room: %w", err)
	}

	var client *viewer
	var pc *webrtc.PeerConnection
	var ayameICEServers []webrtc.ICEServer
	remoteDescriptionSet := false
	var pendingCandidates []webrtc.ICECandidateInit
	cleanup := func() {
		if pc != nil {
			_ = pc.Close()
		}
		if client != nil {
			r.removeViewer(client.id)
		}
	}
	defer cleanup()

	createPeer := func(iceServers []webrtc.ICEServer) error {
		if pc != nil {
			return nil
		}
		client = &viewer{id: r.nextID.Add(1), role: "pilot", remoteAddr: "ayame"}
		client.state.Store(int32(viewerNegotiating))
		if !r.reservePilot(client.id) {
			return errors.New("a local or external pilot is already connected")
		}
		var createErr error
		pc, createErr = r.api.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
		if createErr != nil {
			r.removeViewer(client.id)
			return fmt.Errorf("create Ayame peer connection: %w", createErr)
		}
		pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
			if candidate == nil {
				return
			}
			candidateJSON := candidate.ToJSON()
			if err := sendSignal(signalMessage{Type: "candidate", ICE: &candidateJSON}); err != nil {
				log.Printf("source %q: send Ayame ICE candidate: %v", r.name, err)
			}
		})
		pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
			log.Printf("source %q: Ayame pilot peer connection state: %s", r.name, state.String())
			if state == webrtc.PeerConnectionStateConnected {
				client.state.Store(int32(viewerConnected))
				r.addViewer(client)
				r.requestKeyframe("viewer_connect")
			}
		})
		pc.OnDataChannel(func(channel *webrtc.DataChannel) {
			switch channel.Label() {
			case commandLabel:
				channel.OnOpen(func() {
					client.command.Store(channel)
					log.Printf("source %q: Ayame pilot command channel opened", r.name)
				})
				channel.OnMessage(func(message webrtc.DataChannelMessage) {
					r.handleCommand(client, message)
				})
				channel.OnClose(func() {
					client.command.CompareAndSwap(channel, nil)
					r.setDriveLogging(client.id, false, "Ayame command channel closed")
					r.sendNeutralToUpstream("Ayame command channel closed")
				})
			case driveLabel:
				channel.OnOpen(func() {
					client.drive.Store(channel)
					log.Printf("source %q: Ayame pilot drive channel opened", r.name)
				})
				channel.OnMessage(func(message webrtc.DataChannelMessage) {
					r.handleDriveState(client, message)
				})
				channel.OnClose(func() {
					client.drive.CompareAndSwap(channel, nil)
					r.setDriveLogging(client.id, false, "Ayame drive channel closed")
				})
			case telemetryLabel:
				channel.OnOpen(func() {
					client.telemetry.Store(channel)
					r.sendCurrentGameplayState(client, channel)
				})
				channel.OnClose(func() { client.telemetry.CompareAndSwap(channel, nil) })
			case raceLabel:
				channel.OnOpen(func() {
					client.race.Store(channel)
					r.sendCurrentRaceState(client, channel)
					r.refreshRaceState(client, channel)
				})
				channel.OnClose(func() { client.race.CompareAndSwap(channel, nil) })
			case eventsLabel:
				channel.OnOpen(func() {
					r.openVehicleEventsChannel(client, channel)
					log.Printf("source %q: Ayame pilot events channel opened", r.name)
				})
				channel.OnClose(func() { client.events.CompareAndSwap(channel, nil) })
			default:
				log.Printf("source %q: Ayame pilot opened unsupported DataChannel %q", r.name, channel.Label())
			}
		})
		if _, createErr = pc.AddTrack(r.videoTrack); createErr != nil {
			_ = pc.Close()
			r.removeViewer(client.id)
			pc = nil
			return fmt.Errorf("add Ayame video track: %w", createErr)
		}
		return nil
	}

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		var message signalMessage
		if err := json.Unmarshal(data, &message); err != nil {
			return fmt.Errorf("decode Ayame signaling: %w", err)
		}
		switch message.Type {
		case "accept":
			ayameICEServers = message.ICEServers
			if message.IsExistUser {
				if err := createPeer(ayameICEServers); err != nil {
					return err
				}
				offer, err := pc.CreateOffer(nil)
				if err != nil {
					return fmt.Errorf("create Ayame offer: %w", err)
				}
				if err := pc.SetLocalDescription(offer); err != nil {
					return fmt.Errorf("set Ayame offer: %w", err)
				}
				if err := sendSignal(signalMessage{Type: "offer", SDP: offer.SDP}); err != nil {
					return fmt.Errorf("send Ayame offer: %w", err)
				}
			}
		case "offer":
			if err := createPeer(ayameICEServers); err != nil {
				return err
			}
			if remoteDescriptionSet {
				return errors.New("Ayame renegotiation is not supported")
			}
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: message.SDP}); err != nil {
				return fmt.Errorf("set Ayame offer: %w", err)
			}
			remoteDescriptionSet = true
			for _, candidate := range pendingCandidates {
				if err := pc.AddICECandidate(candidate); err != nil {
					log.Printf("source %q: apply pending Ayame ICE candidate: %v", r.name, err)
				}
			}
			pendingCandidates = nil
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				return fmt.Errorf("create Ayame answer: %w", err)
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				return fmt.Errorf("set Ayame answer: %w", err)
			}
			if err := sendSignal(signalMessage{Type: "answer", SDP: answer.SDP}); err != nil {
				return fmt.Errorf("send Ayame answer: %w", err)
			}
		case "answer":
			if pc == nil {
				return errors.New("received Ayame answer before offer")
			}
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: message.SDP}); err != nil {
				return fmt.Errorf("set Ayame answer: %w", err)
			}
			remoteDescriptionSet = true
			for _, candidate := range pendingCandidates {
				if err := pc.AddICECandidate(candidate); err != nil {
					log.Printf("source %q: apply pending Ayame ICE candidate: %v", r.name, err)
				}
			}
			pendingCandidates = nil
		case "candidate":
			if message.ICE == nil {
				continue
			}
			if pc == nil || !remoteDescriptionSet {
				pendingCandidates = append(pendingCandidates, *message.ICE)
				continue
			}
			if err := pc.AddICECandidate(*message.ICE); err != nil {
				log.Printf("source %q: apply Ayame ICE candidate: %v", r.name, err)
			}
		case "ping":
			if err := sendSignal(signalMessage{Type: "pong"}); err != nil {
				return fmt.Errorf("send Ayame pong: %w", err)
			}
		case "bye", "reject":
			return fmt.Errorf("Ayame %s: %s", message.Type, firstNonEmpty(message.Reason, message.Error))
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "unknown"
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(_ *http.Request) bool { return true },
}

func (server *relayServer) serveViewerWS(w http.ResponseWriter, req *http.Request) {
	device := req.URL.Query().Get("device")
	if device == "" && len(server.sources) == 1 {
		for device = range server.sources {
		}
	}
	source, ok := server.sources[device]
	if !ok {
		if device == "" {
			http.Error(w, "device is required when multiple Momo sources are configured", http.StatusBadRequest)
			return
		}
		http.Error(w, "unknown device: "+device, http.StatusNotFound)
		return
	}
	source.serveViewerWS(w, req)
}

func (r *relay) serveViewerWS(w http.ResponseWriter, req *http.Request) {
	role := req.URL.Query().Get("role")
	if role != "pilot" {
		role = "observer"
	}
	client := &viewer{id: r.nextID.Add(1), role: role, remoteAddr: req.RemoteAddr}
	client.state.Store(int32(viewerNegotiating))
	if role == "pilot" && !r.reservePilot(client.id) {
		http.Error(w, "a pilot viewer is already connected", http.StatusConflict)
		return
	}
	defer r.removeViewer(client.id)
	ws, err := wsUpgrader.Upgrade(w, req, nil)
	if err != nil {
		log.Printf("upgrade viewer signaling: %v", err)
		return
	}
	defer ws.Close()

	pc, err := r.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		_ = ws.WriteJSON(signalMessage{Type: "error", Error: err.Error()})
		return
	}
	defer pc.Close()

	client.pc = pc
	var writeMu sync.Mutex
	sendSignal := func(message signalMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteJSON(message)
	}
	telemetryDone := make(chan struct{})
	defer close(telemetryDone)
	if role == "pilot" {
		client.telemetryWS = make(chan string, 1)
		client.eventsWS = make(chan string, 64)
		go func() {
			for {
				select {
				case payload := <-client.eventsWS:
					if err := sendSignal(signalMessage{Type: "vehicle-event", Data: payload}); err != nil {
						log.Printf("source %q: send WebSocket vehicle event to viewer %d: %v", r.name, client.id, err)
						return
					}
					continue
				default:
				}
				select {
				case payload := <-client.eventsWS:
					if err := sendSignal(signalMessage{Type: "vehicle-event", Data: payload}); err != nil {
						log.Printf("source %q: send WebSocket vehicle event to viewer %d: %v", r.name, client.id, err)
						return
					}
				case payload := <-client.telemetryWS:
					if err := sendSignal(signalMessage{Type: "telemetry", Data: payload}); err != nil {
						log.Printf("source %q: send WebSocket telemetry to viewer %d: %v", r.name, client.id, err)
						return
					}
				case <-telemetryDone:
					return
				}
			}
		}()
	}

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		candidateJSON := candidate.ToJSON()
		if err := sendSignal(signalMessage{Type: "candidate", ICE: &candidateJSON}); err != nil {
			log.Printf("send viewer %d ICE candidate: %v", client.id, err)
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("viewer %d (%s) peer connection state: %s", client.id, client.role, state.String())
		if state == webrtc.PeerConnectionStateConnected {
			client.state.Store(int32(viewerConnected))
			r.requestKeyframe("viewer_connect")
			// 接続直後の IDR が欠けると、H.264 の復号器は次の IDR まで
			// 映像を出せない。LAN 内でも ICE/DTLS の確立直後はこの状態に
			// なり得るため、短時間だけ PLI を再送する。
			go func() {
				for _, delay := range []time.Duration{time.Second, 3 * time.Second} {
					time.Sleep(delay)
					r.requestKeyframe("viewer_connect")
				}
			}()
		}
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			r.removeViewer(client.id)
		}
	})
	pc.OnDataChannel(func(channel *webrtc.DataChannel) {
		switch channel.Label() {
		case telemetryLabel:
			channel.OnOpen(func() {
				client.telemetry.Store(channel)
				log.Printf("source %q: viewer %d (%s remote=%s) telemetry channel opened", r.name, client.id, client.role, client.remoteAddr)
				r.sendCurrentGameplayState(client, channel)
			})
			channel.OnClose(func() {
				client.telemetry.CompareAndSwap(channel, nil)
				log.Printf("source %q: viewer %d (%s remote=%s) telemetry channel closed", r.name, client.id, client.role, client.remoteAddr)
			})
		case commandLabel:
			channel.OnOpen(func() {
				client.command.Store(channel)
				log.Printf("viewer %d command channel opened", client.id)
			})
			channel.OnMessage(func(message webrtc.DataChannelMessage) {
				r.handleCommand(client, message)
			})
			channel.OnClose(func() {
				client.command.CompareAndSwap(channel, nil)
				r.setDriveLogging(client.id, false, "command channel closed")
			})
		case driveLabel:
			channel.OnOpen(func() {
				client.drive.Store(channel)
				log.Printf("viewer %d drive channel opened", client.id)
			})
			channel.OnMessage(func(message webrtc.DataChannelMessage) {
				r.handleDriveState(client, message)
			})
			channel.OnClose(func() {
				client.drive.CompareAndSwap(channel, nil)
				r.setDriveLogging(client.id, false, "drive channel closed")
			})
		case raceLabel:
			channel.OnOpen(func() {
				client.race.Store(channel)
				log.Printf("viewer %d race channel opened", client.id)
				r.sendCurrentRaceState(client, channel)
				r.refreshRaceState(client, channel)
			})
			channel.OnClose(func() { client.race.CompareAndSwap(channel, nil) })
		case eventsLabel:
			channel.OnOpen(func() {
				r.openVehicleEventsChannel(client, channel)
				log.Printf("viewer %d events channel opened", client.id)
			})
			channel.OnClose(func() { client.events.CompareAndSwap(channel, nil) })
		default:
			log.Printf("viewer %d opened unsupported DataChannel %q", client.id, channel.Label())
		}
	})

	_, err = pc.AddTrack(r.videoTrack)
	if err != nil {
		_ = sendSignal(signalMessage{Type: "error", Error: err.Error()})
		return
	}

	remoteDescriptionSet := false
	var pendingCandidates []webrtc.ICECandidateInit
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			r.removeViewer(client.id)
			return
		}
		var message signalMessage
		if err := json.Unmarshal(data, &message); err != nil {
			_ = sendSignal(signalMessage{Type: "error", Error: "invalid signaling JSON"})
			continue
		}
		switch message.Type {
		case "offer":
			if remoteDescriptionSet {
				_ = sendSignal(signalMessage{Type: "error", Error: "renegotiation is not supported"})
				continue
			}
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: message.SDP}); err != nil {
				_ = sendSignal(signalMessage{Type: "error", Error: err.Error()})
				continue
			}
			remoteDescriptionSet = true
			for _, candidate := range pendingCandidates {
				if err := pc.AddICECandidate(candidate); err != nil {
					log.Printf("apply pending viewer %d ICE candidate: %v", client.id, err)
				}
			}
			pendingCandidates = nil
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				_ = sendSignal(signalMessage{Type: "error", Error: err.Error()})
				continue
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				_ = sendSignal(signalMessage{Type: "error", Error: err.Error()})
				continue
			}
			r.addViewer(client)
			if err := sendSignal(signalMessage{Type: "answer", SDP: answer.SDP}); err != nil {
				return
			}
		case "candidate":
			if message.ICE == nil {
				continue
			}
			if !remoteDescriptionSet {
				pendingCandidates = append(pendingCandidates, *message.ICE)
				continue
			}
			if err := pc.AddICECandidate(*message.ICE); err != nil {
				log.Printf("apply viewer %d ICE candidate: %v", client.id, err)
			}
		case "close", "bye":
			r.removeViewer(client.id)
			return
		}
	}
}

func parseSource(value string) (string, string, error) {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("source must be DEVICE=WS_URL: %q", value)
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}

func raceMessageForCar(state []byte, carID string) (string, error) {
	if strings.TrimSpace(carID) == "" {
		return "", errors.New("race car ID is empty")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(state, &payload); err != nil {
		return "", fmt.Errorf("decode race state: %w", err)
	}
	carIDJSON, err := json.Marshal(carID)
	if err != nil {
		return "", fmt.Errorf("encode race car ID: %w", err)
	}
	payload["viewerCarId"] = carIDJSON
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode race state: %w", err)
	}
	return "RACE:" + string(encoded), nil
}

func (server *relayServer) startRaceControl(ctx context.Context, raceURL string, viewerToken string) {
	if strings.TrimSpace(raceURL) == "" {
		return
	}
	go func() {
		for {
			if err := server.connectRaceControl(ctx, raceURL, viewerToken); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("Race Control disconnected: %v; retrying in 3 seconds", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}()
}

func (server *relayServer) connectRaceControl(ctx context.Context, raceURL string, viewerToken string) error {
	headers := http.Header{}
	if strings.TrimSpace(viewerToken) != "" {
		headers.Set("Authorization", "Bearer "+strings.TrimSpace(viewerToken))
	}
	log.Printf("connecting Race Control: %s", raceURL)
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, raceURL, headers)
	if err != nil {
		return fmt.Errorf("connect Race Control WebSocket: %w", err)
	}
	defer ws.Close()
	defer server.markRaceControlDisconnected()

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return fmt.Errorf("read Race Control WebSocket: %w", err)
		}
		var envelope raceStateEnvelope
		if err := json.Unmarshal(data, &envelope); err != nil {
			log.Printf("ignore malformed Race Control message: %v", err)
			continue
		}
		if envelope.Type != "race_state" || envelope.Version != 2 {
			log.Printf("ignore unsupported Race Control message: type=%q version=%d", envelope.Type, envelope.Version)
			continue
		}
		server.observeRaceContext(envelope, time.Now())
		if server.recorder != nil {
			server.recorder.RecordRaceState(string(data), telemetryRaceContext{
				RaceID:    envelope.RaceID,
				RaceRunID: envelope.RaceRunID,
				Phase:     envelope.Phase,
				Flag:      envelope.Flag,
				Sequence:  envelope.Sequence,
				Present:   true,
			})
		}
		for _, source := range server.sources {
			message, err := raceMessageForCar(data, source.raceCarID)
			if err != nil {
				log.Printf("source %q: ignore Race Control state: %v", source.name, err)
				continue
			}
			source.publishRaceState(message, envelope.Phase)
		}
	}
}

func main() {
	var upstream string
	var listen string
	var allowObserverCommand bool
	var rtpStallTimeout time.Duration
	var upstreamStartTimeout time.Duration
	var sources sourceFlag
	var raceCars sourceFlag
	var operationsAllowCIDRs sourceFlag
	var garageAllowCIDRs sourceFlag
	var gameplayAllowCIDRs sourceFlag
	var raceURL string
	var raceViewerToken string
	var ayameSignalingURL string
	var ayameClientIDPrefix string
	var ayameSignalingKey string
	var ayamePilotRooms sourceFlag
	var telemetryLogDir string
	var healthRecoveryModeValue string
	flag.StringVar(&upstream, "upstream", "", "Momo P2P WebSocket URL, for example ws://192.168.11.3:8080/ws")
	flag.Var(&sources, "source", "Momo source as DEVICE=WS_URL; can be repeated")
	flag.Var(&raceCars, "race-car", "Race Control car mapping as DEVICE=CAR_ID; can be repeated")
	flag.Var(&operationsAllowCIDRs, "operations-allow-cidr", "CIDR allowed to read /operations.html and /api/v1/status; can be repeated (default: loopback only)")
	flag.Var(&garageAllowCIDRs, "garage-allow-cidr", "CIDR allowed to read /garage.html and /api/v1/pilot-devices; can be repeated (default: loopback only)")
	flag.Var(&gameplayAllowCIDRs, "gameplay-allow-cidr", "CIDR allowed to call gameplay APIs; can be repeated (default: loopback only)")
	flag.StringVar(&listen, "listen", ":8090", "HTTP and WebSocket listen address")
	flag.StringVar(&raceURL, "race-url", strings.TrimSpace(os.Getenv("MOMO_RACE_CONTROL_WS_URL")), "Race Control WebSocket URL for race_state v2 distribution")
	flag.StringVar(&raceViewerToken, "race-viewer-token", strings.TrimSpace(os.Getenv("MOMO_RACE_CONTROL_VIEWER_TOKEN")), "Race Control Viewer Bearer token")
	flag.StringVar(&ayameSignalingURL, "ayame-signaling-url", "", "Ayame signaling WebSocket URL for external pilot distribution")
	flag.StringVar(&ayameClientIDPrefix, "ayame-client-id-prefix", "momo-relay", "Ayame client ID prefix; source name is appended")
	flag.StringVar(&ayameSignalingKey, "ayame-signaling-key", "", "Ayame signaling key for external pilot distribution")
	flag.Var(&ayamePilotRooms, "ayame-pilot-room", "Ayame external pilot room as DEVICE=ROOM_ID; can be repeated")
	flag.StringVar(&telemetryLogDir, "telemetry-log-dir", "", "directory for Relay-local interleaved telemetry NDJSON logs (disabled when empty)")
	flag.StringVar(&healthRecoveryModeValue, "health-recovery-mode", strings.TrimSpace(os.Getenv("MOMO_RELAY_HEALTH_RECOVERY_MODE")), "vehicle HP recovery mode: legacy, pit-marker, hybrid, or disabled")
	flag.BoolVar(&allowObserverCommand, "allow-observer-command", false, "allow observer viewers to send commands to Momo")
	flag.DurationVar(&rtpStallTimeout, "rtp-stall-timeout", defaultRTPStallTimeout, "reconnect a source when received RTP stops for this duration")
	flag.DurationVar(&upstreamStartTimeout, "upstream-start-timeout", defaultUpstreamStartTimeout, "reconnect a source when no RTP arrives after connection")
	flag.Parse()
	if rtpStallTimeout <= 0 || upstreamStartTimeout <= 0 {
		log.Fatal("-rtp-stall-timeout and -upstream-start-timeout must be positive")
	}
	if strings.TrimSpace(healthRecoveryModeValue) == "" {
		healthRecoveryModeValue = string(vehicleHealthRecoveryDefault)
	}
	healthRecoveryMode, err := parseVehicleHealthRecoveryMode(healthRecoveryModeValue)
	if err != nil {
		log.Fatal(err)
	}
	gameplayToken := strings.TrimSpace(os.Getenv("MOMO_RELAY_GAMEPLAY_TOKEN"))
	if healthRecoveryMode.allowsPitRecovery() {
		if gameplayToken == "" {
			log.Fatalf("MOMO_RELAY_GAMEPLAY_TOKEN is required when -health-recovery-mode=%s", healthRecoveryMode)
		}
		if strings.TrimSpace(raceURL) == "" {
			log.Fatalf("-race-url is required when -health-recovery-mode=%s", healthRecoveryMode)
		}
	}
	if upstream != "" {
		sources = append(sources, "default="+upstream)
	}
	if len(sources) == 0 {
		log.Fatal("-upstream or at least one -source is required")
	}
	operationsPolicy, err := parseOperationsAccessPolicy(operationsAllowCIDRs)
	if err != nil {
		log.Fatal(err)
	}
	garagePolicy, err := parseOperationsAccessPolicy(garageAllowCIDRs)
	if err != nil {
		log.Fatal(err)
	}
	gameplayPolicy, err := parseOperationsAccessPolicy(gameplayAllowCIDRs)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	raceCarBySource := make(map[string]string, len(raceCars))
	raceCarSources := make(map[string]string, len(raceCars))
	for _, raceCarValue := range raceCars {
		name, carID, err := parseSource(raceCarValue)
		if err != nil {
			log.Fatalf("invalid -race-car: %v", err)
		}
		if _, exists := raceCarBySource[name]; exists {
			log.Fatalf("duplicate Race Control source mapping: %q", name)
		}
		if existingSource, exists := raceCarSources[carID]; exists {
			log.Fatalf("duplicate Race Control car mapping %q for sources %q and %q", carID, existingSource, name)
		}
		raceCarBySource[name] = carID
		raceCarSources[carID] = name
	}
	ayameRoomBySource := make(map[string]string, len(ayamePilotRooms))
	for _, ayameRoomValue := range ayamePilotRooms {
		name, roomID, err := parseSource(ayameRoomValue)
		if err != nil {
			log.Fatalf("invalid -ayame-pilot-room: %v", err)
		}
		if _, exists := ayameRoomBySource[name]; exists {
			log.Fatalf("duplicate Ayame source mapping: %q", name)
		}
		ayameRoomBySource[name] = roomID
	}
	if len(ayameRoomBySource) > 0 && ayameSignalingURL == "" {
		log.Fatal("-ayame-signaling-url is required when -ayame-pilot-room is set")
	}
	var recorder *telemetryRecorder
	if strings.TrimSpace(telemetryLogDir) != "" {
		recorder, err = newTelemetryRecorder(strings.TrimSpace(telemetryLogDir))
		if err != nil {
			log.Fatal(err)
		}
		defer func() {
			if err := recorder.Close(); err != nil {
				log.Printf("close telemetry recorder: %v", err)
			}
			stats := recorder.Stats()
			log.Printf("telemetry recorder stopped: path=%s telemetry=%d raceState=%d queueDrops=%d writeErrors=%d",
				recorder.Path(), stats.TelemetryRecords, stats.RaceStateRecords, stats.QueueDrops, stats.WriteErrors)
		}()
		log.Printf("telemetry recorder started: path=%s", recorder.Path())
	}
	serverRelay := &relayServer{
		sources:     make(map[string]*relay, len(sources)),
		sourceOrder: make([]string, 0, len(sources)),
		recorder:    recorder,
		pitEvents:   make(map[string]pitPresenceReceipt),
	}
	for _, sourceValue := range sources {
		name, sourceURL, err := parseSource(sourceValue)
		if err != nil {
			log.Fatal(err)
		}
		if _, exists := serverRelay.sources[name]; exists {
			log.Fatalf("duplicate source name: %q", name)
		}
		raceCarID := raceCarBySource[name]
		if raceURL != "" && raceCarID == "" {
			log.Fatalf("Race Control is enabled but source %q has no -race-car mapping", name)
		}
		relay, err := newRelay(name, sourceURL, raceCarID, allowObserverCommand,
			rtpStallTimeout, upstreamStartTimeout, healthRecoveryMode)
		if err != nil {
			log.Fatal(err)
		}
		relay.recorder = recorder
		serverRelay.sources[name] = relay
		serverRelay.sourceOrder = append(serverRelay.sourceOrder, name)
		relay.start(ctx)
		if roomID := ayameRoomBySource[name]; roomID != "" {
			clientID := strings.TrimSuffix(ayameClientIDPrefix, "-") + "-" + name
			relay.startAyamePilot(ctx, ayameSignalingURL, roomID, clientID, ayameSignalingKey)
		}
	}
	for name := range ayameRoomBySource {
		if _, exists := serverRelay.sources[name]; !exists {
			log.Fatalf("Ayame source %q is not configured by -source", name)
		}
	}
	serverRelay.startRaceControl(ctx, raceURL, raceViewerToken)

	webRoot, err := fs.Sub(webAssets, "web")
	if err != nil {
		log.Fatal(err)
	}
	operationsHTML, err := fs.ReadFile(webRoot, "operations.html")
	if err != nil {
		log.Fatal(err)
	}
	garageHTML, err := fs.ReadFile(webRoot, "garage.html")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/status", operationsPolicy.wrap(serverRelay.serveOperationsStatus))
	mux.HandleFunc("/api/v1/race-state", serverRelay.serveRaceState)
	mux.HandleFunc("/operations.html", operationsPolicy.wrap(operationsPageHandler(operationsHTML)))
	mux.HandleFunc("/api/v1/pilot-devices", garagePolicy.wrap(serverRelay.servePilotDevices))
	mux.HandleFunc("/api/v1/gameplay/pit-recovery-ticks",
		gameplayPolicy.wrap(bearerTokenHandler(gameplayToken, serverRelay.servePitRecoveryTick)))
	mux.HandleFunc("/api/v1/gameplay/pit-presence-events",
		gameplayPolicy.wrap(bearerTokenHandler(gameplayToken, serverRelay.servePitPresenceEvent)))
	mux.HandleFunc("/garage.html", garagePolicy.wrap(operationsPageHandler(garageHTML)))
	fileServer := http.FileServer(http.FS(webRoot))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fileServer.ServeHTTP(w, req)
	}))
	mux.HandleFunc("/pilot", func(w http.ResponseWriter, req *http.Request) {
		target := "/pilot.html"
		if req.URL.RawQuery != "" {
			target += "?" + req.URL.RawQuery
		}
		http.Redirect(w, req, target, http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/ws", serverRelay.serveViewerWS)
	server := &http.Server{Addr: listen, Handler: mux}
	log.Printf("Momo relay is listening on http://%s/ for sources: %s", listen, strings.Join(sources, ", "))
	log.Fatal(server.ListenAndServe())
}
