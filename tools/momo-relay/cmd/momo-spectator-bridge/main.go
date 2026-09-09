// momo-spectator-bridge receives one LAN Observer stream and forwards H.264 to
// an SFU peer. Its parent owns cloud authorization and public data projection.
// stdout is a bounded local JSON stream; stdin EOF or lease expiry closes peers.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

var h264 = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
	SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}}}

type message struct {
	Type    string                   `json:"type"`
	Data    string                   `json:"data,omitempty"`
	SDP     string                   `json:"sdp,omitempty"`
	ICE     *webrtc.ICECandidateInit `json:"ice,omitempty"`
	Error   string                   `json:"error,omitempty"`
	Packets uint64                   `json:"packets,omitempty"`
	Dropped uint64                   `json:"dropped,omitempty"`
}
type timedPacket struct {
	packet *rtp.Packet
	at     time.Time
}

func relayEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "ws" && u.Scheme != "wss") || u.User != nil || u.Path != "/ws" {
		return "", errors.New("explicit Relay /ws URL required")
	}
	q := u.Query()
	if q.Get("device") == "" {
		return "", errors.New("device is required")
	}
	q.Set("role", "observer")
	q.Set("client", "spectator-publisher")
	u.RawQuery = q.Encode()
	return u.String(), nil
}
func main() {
	raw := flag.String("relay-url", "", "explicit Relay /ws?device=source URL")
	flag.Parse()
	endpoint, err := relayEndpoint(*raw)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err = run(ctx, cancel, endpoint); err != nil && !errors.Is(err, context.Canceled) {
		log.Print(err)
		os.Exit(1)
	}
}
func run(ctx context.Context, cancel context.CancelFunc, endpoint string) error {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: h264, PayloadType: 102}, webrtc.RTPCodecTypeVideo); err != nil {
		return err
	}
	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, registry); err != nil {
		return err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(registry))
	lan, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	defer lan.Close()
	cloud, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.cloudflare.com:3478"}}}})
	if err != nil {
		return err
	}
	defer cloud.Close()
	track, err := webrtc.NewTrackLocalStaticRTP(h264, "video", "spectator")
	if err != nil {
		return err
	}
	transceiver, err := cloud.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		return err
	}
	sender := transceiver.Sender()
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return fmt.Errorf("Relay connect: %w", err)
	}
	defer ws.Close()
	ws.SetReadLimit(256 * 1024)
	var sendMu sync.Mutex
	send := func(m message) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		_ = ws.SetWriteDeadline(time.Now().Add(3 * time.Second))
		return ws.WriteJSON(m)
	}
	output := make(chan message, 256)
	fatal := make(chan error, 1)
	fail := func(err error) {
		select {
		case fatal <- err:
		default:
		}
		cancel()
	}
	var drops, packets atomic.Uint64
	emit := func(m message) {
		select {
		case output <- m:
		default:
			drops.Add(1)
		}
	}
	go func() {
		writer := bufio.NewWriter(os.Stdout)
		encoder := json.NewEncoder(writer)
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-output:
				if err := encoder.Encode(m); err != nil {
					fail(err)
					return
				}
				if err := writer.Flush(); err != nil {
					fail(err)
					return
				}
			}
		}
	}()
	go func() { <-ctx.Done(); _ = ws.Close(); _ = lan.Close(); _ = cloud.Close() }()
	var sourceSSRC atomic.Uint32
	var lastPLI atomic.Int64
	pli := func() {
		now := time.Now().UnixMilli()
		prev := lastPLI.Load()
		ssrc := sourceSSRC.Load()
		if ssrc != 0 && now-prev >= 1000 && lastPLI.CompareAndSwap(prev, now) {
			_ = lan.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
		}
	}
	go func() {
		for {
			feedback, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			for _, p := range feedback {
				switch p.(type) {
				case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
					pli()
				}
			}
		}
	}()
	var cloudReady atomic.Bool
	var authorizedUntil atomic.Int64
	cloud.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		cloudReady.Store(s == webrtc.PeerConnectionStateConnected)
		emit(message{Type: "cloud-state", Data: s.String()})
		if s == webrtc.PeerConnectionStateConnected {
			pli()
		}
		if s == webrtc.PeerConnectionStateFailed {
			fail(errors.New("cloud peer failed"))
		}
	})
	lan.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		emit(message{Type: "relay-state", Data: s.String()})
		if s == webrtc.PeerConnectionStateFailed {
			fail(errors.New("Relay peer failed"))
		}
	})
	lan.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			candidate := c.ToJSON()
			if err := send(message{Type: "candidate", ICE: &candidate}); err != nil {
				fail(err)
			}
		}
	})
	var offerOnce sync.Once
	queue := make(chan timedPacket, 512)
	lan.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if !strings.EqualFold(remote.Codec().MimeType, webrtc.MimeTypeH264) {
			fail(errors.New("H.264 required"))
			return
		}
		sourceSSRC.Store(uint32(remote.SSRC()))
		offerOnce.Do(func() {
			go func() {
				offer, e := cloud.CreateOffer(nil)
				if e != nil {
					fail(e)
					return
				}
				gather := webrtc.GatheringCompletePromise(cloud)
				if e = cloud.SetLocalDescription(offer); e != nil {
					fail(e)
					return
				}
				select {
				case <-gather:
				case <-time.After(10 * time.Second):
					fail(errors.New("SFU ICE gathering timeout"))
					return
				case <-ctx.Done():
					return
				}
				emit(message{Type: "offer", SDP: cloud.LocalDescription().SDP})
			}()
		})
		go func() {
			for {
				p, _, e := remote.ReadRTP()
				if e != nil {
					if ctx.Err() == nil {
						fail(e)
					}
					return
				}
				packets.Add(1)
				// Local extensions (including transport sequence numbers) belong to this leg.
				p.Extension = false
				p.Extensions = nil
				p.CSRC = nil
				if len(p.Payload) > 8192 {
					fail(errors.New("RTP payload exceeds bridge bound"))
					return
				}
				if !cloudReady.Load() {
					continue
				}
				select {
				case queue <- timedPacket{p, time.Now()}:
				default:
					drops.Add(1)
					fail(errors.New("cloud video queue overflow; reconnect required"))
					return
				}
			}
		}()
	})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case item := <-queue:
				if !cloudReady.Load() {
					continue
				}
				if time.Since(item.at) > 100*time.Millisecond {
					drops.Add(1)
					fail(errors.New("cloud video queue stale; reconnect required"))
					return
				}
				if e := track.WriteRTP(item.packet); e != nil && ctx.Err() == nil {
					fail(e)
					return
				}
			}
		}
	}()
	// The parent is the sole authority for renewing the short cloud-send lease.
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 4096), 65536)
		for scanner.Scan() {
			var m message
			if json.Unmarshal(scanner.Bytes(), &m) != nil {
				fail(errors.New("invalid parent message"))
				return
			}
			switch m.Type {
			case "answer":
				authorizedUntil.Store(time.Now().Add(15 * time.Second).UnixMilli())
				if err := cloud.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.SDP}); err != nil {
					fail(err)
					return
				}
			case "lease":
				authorizedUntil.Store(time.Now().Add(15 * time.Second).UnixMilli())
			default:
				fail(errors.New("unknown parent message"))
				return
			}
		}
		fail(io.EOF)
	}()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				until := authorizedUntil.Load()
				if until > 0 && time.Now().UnixMilli() > until {
					fail(errors.New("publisher authorization expired"))
					return
				}
				emit(message{Type: "stats", Packets: packets.Load(), Dropped: drops.Load()})
			}
		}
	}()
	if _, err = lan.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		return err
	}
	offer, err := lan.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err = lan.SetLocalDescription(offer); err != nil {
		return err
	}
	if err = send(message{Type: "offer", SDP: offer.SDP}); err != nil {
		return err
	}
	pending := []webrtc.ICECandidateInit{}
	descriptionSet := false
	for {
		var m message
		if err = ws.ReadJSON(&m); err != nil {
			select {
			case e := <-fatal:
				return e
			default:
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		switch m.Type {
		case "answer":
			if err = lan.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.SDP}); err != nil {
				return err
			}
			descriptionSet = true
			for _, c := range pending {
				if err = lan.AddICECandidate(c); err != nil {
					return err
				}
			}
			pending = nil
		case "candidate":
			if m.ICE != nil {
				if descriptionSet {
					if err = lan.AddICECandidate(*m.ICE); err != nil {
						return err
					}
				} else {
					if len(pending) >= 64 {
						return errors.New("too many Relay candidates")
					}
					pending = append(pending, *m.ICE)
				}
			}
		case "telemetry", "command", "vehicle-event", "m5-audio":
			emit(m)
		case "error", "close":
			return errors.New("Relay signaling ended")
		}
	}
}
