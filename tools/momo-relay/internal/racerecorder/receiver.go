package racerecorder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

const telemetryLabel = "momo-telemetry"

var recorderH264Codec = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeH264,
	ClockRate:   90000,
	SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
}

var recorderOpusCodec = webrtc.RTPCodecCapability{
	MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	SDPFmtpLine: "minptime=10;useinbandfec=1",
}

type signalMessage struct {
	Type  string                   `json:"type"`
	Data  string                   `json:"data,omitempty"`
	SDP   string                   `json:"sdp,omitempty"`
	ICE   *webrtc.ICECandidateInit `json:"ice,omitempty"`
	Error string                   `json:"error,omitempty"`
}

type sourceManifest struct {
	SourceID  string     `json:"sourceId"`
	VehicleID string     `json:"vehicleId"`
	CarID     string     `json:"carId"`
	State     string     `json:"state"`
	Error     string     `json:"error,omitempty"`
	Video     videoStats `json:"video"`
	Audio     audioStats `json:"audio"`
}

type sourceCapture struct {
	source    Source
	endpoint  string
	directory string
	startedAt time.Time
	video     *h264Writer
	audio     *audioWriter
	ready     chan struct{}
	readyOnce sync.Once
	workerMu  sync.Mutex
	workers   sync.WaitGroup
	closing   bool
}

func newSourceCapture(source Source, relayWebSocketURL string, runDirectory string, startedAt time.Time, segmentDuration time.Duration) (*sourceCapture, error) {
	endpoint, err := recorderSourceURL(relayWebSocketURL, source.SourceID)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(runDirectory, "sources", source.SourceID)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create source directory %s: %w", source.SourceID, err)
	}
	audio, err := newAudioWriter(directory, startedAt)
	if err != nil {
		return nil, err
	}
	return &sourceCapture{
		source: source, endpoint: endpoint, directory: directory, startedAt: startedAt,
		video: newH264Writer(directory, startedAt, segmentDuration), audio: audio, ready: make(chan struct{}),
	}, nil
}

func recorderSourceURL(baseURL string, sourceID string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", fmt.Errorf("Relay WebSocket URL must be credential-free ws:// or wss://")
	}
	query := parsed.Query()
	query.Set("role", "observer")
	query.Set("client", "recorder")
	query.Set("device", sourceID)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func (capture *sourceCapture) Ready() <-chan struct{} {
	return capture.ready
}

func (capture *sourceCapture) Run(ctx context.Context) error {
	api, err := newRecorderAPI()
	if err != nil {
		return err
	}
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, capture.endpoint, nil)
	if err != nil {
		return fmt.Errorf("connect Relay source %s: %w", capture.source.SourceID, err)
	}
	defer ws.Close()
	peer, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return fmt.Errorf("create Recorder peer for %s: %w", capture.source.SourceID, err)
	}
	defer func() {
		capture.workerMu.Lock()
		capture.closing = true
		capture.workerMu.Unlock()
		_ = peer.Close()
		capture.workers.Wait()
	}()

	var writeMu sync.Mutex
	send := func(message signalMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteJSON(message)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = peer.Close()
			_ = ws.Close()
		case <-done:
		}
	}()

	peer.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		value := candidate.ToJSON()
		_ = send(signalMessage{Type: "candidate", ICE: &value})
	})
	fatal := make(chan error, 2)
	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed {
			select {
			case fatal <- fmt.Errorf("Relay peer failed for source %s", capture.source.SourceID):
			default:
			}
		}
	})
	peer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeVideo || !strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeH264) {
			return
		}
		if !capture.beginWorker() {
			return
		}
		go func() {
			defer capture.workers.Done()
			if err := capture.readVideoTrack(ctx, track); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
				select {
				case fatal <- err:
				default:
				}
			}
		}()
	})
	telemetry, err := peer.CreateDataChannel(telemetryLabel, nil)
	if err != nil {
		return fmt.Errorf("create M5 audio DataChannel for %s: %w", capture.source.SourceID, err)
	}
	telemetry.OnMessage(func(message webrtc.DataChannelMessage) {
		if !capture.beginWorker() {
			return
		}
		defer capture.workers.Done()
		if bytes.HasPrefix(message.Data, []byte("AUD:")) {
			if err := capture.audio.Write(message.Data, time.Now()); err != nil {
				select {
				case fatal <- fmt.Errorf("record M5 audio for source %s: %w", capture.source.SourceID, err):
				default:
				}
			}
		}
	})
	if _, err := peer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		return fmt.Errorf("add Recorder video transceiver for %s: %w", capture.source.SourceID, err)
	}
	offer, err := peer.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create Recorder offer for %s: %w", capture.source.SourceID, err)
	}
	if err := peer.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set Recorder offer for %s: %w", capture.source.SourceID, err)
	}
	if err := send(signalMessage{Type: "offer", SDP: offer.SDP}); err != nil {
		return fmt.Errorf("send Recorder offer for %s: %w", capture.source.SourceID, err)
	}

	signalErrors := make(chan error, 1)
	go func() { signalErrors <- readRecorderSignals(peer, ws) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-fatal:
		return err
	case err := <-signalErrors:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Relay signaling ended for source %s: %w", capture.source.SourceID, err)
	}
}

func (capture *sourceCapture) beginWorker() bool {
	capture.workerMu.Lock()
	defer capture.workerMu.Unlock()
	if capture.closing {
		return false
	}
	capture.workers.Add(1)
	return true
}

func (capture *sourceCapture) readVideoTrack(ctx context.Context, track *webrtc.TrackRemote) error {
	builder := samplebuilder.New(512, &codecs.H264Packet{}, recorderH264Codec.ClockRate)
	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		builder.Push(packet)
		for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
			capture.video.RecordPacketLoss(sample.PrevDroppedPackets)
			if err := capture.video.WriteSample(sample.Data, time.Now()); err != nil {
				return err
			}
			select {
			case <-capture.video.Ready():
				capture.readyOnce.Do(func() { close(capture.ready) })
			default:
			}
		}
	}
}

func (capture *sourceCapture) Close(state string, runError string, at time.Time) (sourceManifest, error) {
	video, videoErr := capture.video.Close(at)
	audio, audioErr := capture.audio.Close()
	manifest := sourceManifest{
		SourceID: capture.source.SourceID, VehicleID: capture.source.VehicleID, CarID: capture.source.CarID,
		State: state, Error: runError, Video: video, Audio: audio,
	}
	if videoErr != nil {
		return manifest, videoErr
	}
	return manifest, audioErr
}

func newRecorderAPI() (*webrtc.API, error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: recorderH264Codec, PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, fmt.Errorf("register Recorder H.264 codec: %w", err)
	}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: recorderOpusCodec, PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("register Recorder Opus codec: %w", err)
	}
	interceptors := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptors); err != nil {
		return nil, fmt.Errorf("register Recorder interceptors: %w", err)
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(interceptors)), nil
}

func readRecorderSignals(peer *webrtc.PeerConnection, ws *websocket.Conn) error {
	remoteSet := false
	var pending []webrtc.ICECandidateInit
	for {
		_, payload, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		var message signalMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			return fmt.Errorf("decode Relay signal: %w", err)
		}
		switch message.Type {
		case "answer":
			if remoteSet {
				return fmt.Errorf("Relay renegotiation is not supported")
			}
			if err := peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: message.SDP}); err != nil {
				return fmt.Errorf("set Relay answer: %w", err)
			}
			remoteSet = true
			for _, candidate := range pending {
				if err := peer.AddICECandidate(candidate); err != nil {
					return fmt.Errorf("apply pending Relay candidate: %w", err)
				}
			}
			pending = nil
		case "candidate":
			if message.ICE == nil {
				continue
			}
			if !remoteSet {
				pending = append(pending, *message.ICE)
				continue
			}
			if err := peer.AddICECandidate(*message.ICE); err != nil {
				return fmt.Errorf("apply Relay candidate: %w", err)
			}
		case "error":
			return fmt.Errorf("Relay error: %s", message.Error)
		}
	}
}
