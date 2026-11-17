package bridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/Fever-r/BombeCam/internal/winproc"
	"github.com/Fever-r/BombeCam/pkg/netstack"
)

type Viewer struct {
	foreignSeen map[string]bool // other viewers' sessions already logged (under mu)
	// sameLoginAt is when a viewer signed in with BombeCam's own Osaio login
	// (the Osaio app, usually) last showed activity on this camera (under mu).
	// The camera then gives BombeCam no video until that view closes.
	sameLoginAt time.Time

	sig            *Signaling
	pc             *webrtc.PeerConnection
	vc             *VideoCall
	callID         string
	uuid           string
	model          string
	ffmpeg         string
	rtspURL        string
	rtpPort        int
	ff             *exec.Cmd
	ffStdin        io.WriteCloser
	ffAudio        io.WriteCloser
	audioListener  net.Listener
	pendingICE     []webrtc.ICECandidateInit
	closed         bool
	doneChan       chan struct{}
	closeOnce      sync.Once
	mu             sync.Mutex
	lastPacketTime time.Time
	gotFirstPacket bool
	gotFirstAudio  bool
	hasAudio       bool

	// Lifecycle callbacks
	OnMediaActive     func()
	OnCandidateHostIP func(ip string)
	// OnMediaPath reports how the media travels once connected: "lan"
	// (directly on the local network), "internet" (directly, across NAT) or
	// "relay" (through the vendor's TURN server), plus the remote address.
	OnMediaPath func(kind, remote string)

	// WebRTC Two-Way Audio Talkback
	audioTrack    *talkTrack
	talkActive    bool
	talkFormatID  string // chosen talk format (under talkMu); "" = default
	talkMu        sync.Mutex
	talkCommandMu sync.Mutex // serialize start/stop and their response waiter
	talkWaitCh    chan int
	lastAudioDown time.Time
	lastAudioUp   time.Time
	audioMu       sync.Mutex
	audioHdrByte2 byte

	// Talkback diagnostics: did the camera answer service.Talk, how much
	// audio went out, and did the camera send RTCP about our audio stream
	// (proof that our packets reach it).
	talkAnswered   bool
	talkRet        int
	talkFramesSent atomic.Int64
	talkBytesSent  atomic.Int64
	talkDelivered  atomic.Int64 // packets the connection accepted for sending
	audioRTCP      atomic.Int64

	// Local control channel (e.g. MQTTControlChannel)
	ctrlChan ControlChannel

	// Verified media activation
	mediaActiveTriggered bool
	allowedSubnets       []*net.IPNet

	rtpMu        sync.Mutex
	rtpSeq       uint16
	rtpTimestamp uint32
	talkLastPkt  time.Time // when the last talk packet went out (under rtpMu)

	// Publishing: "ffmpeg" pipes into an FFmpeg subprocess; "native" publishes
	// RTSP directly from Go (no external dependency).
	publisher   string
	sink        *nativeSink
	lastFFStart time.Time

	// Camera microphone format (mirrored by talkback) and the talkback encoder.
	camAudioMu sync.Mutex
	camAudio   camAudioInfo
	talkSess   *talkSession
	// talkWriteHook replaces the RTP write in tests.
	talkWriteHook func(frame []byte, step uint32, marker bool) error
}

// Publisher modes for SetPublisher.
const (
	PublisherFFmpeg = "ffmpeg"
	PublisherNative = "native"
)

// SetPublisher selects how camera media reaches MediaMTX. Call before Start.
func (v *Viewer) SetPublisher(mode string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if mode == PublisherNative {
		v.publisher = PublisherNative
	} else {
		v.publisher = PublisherFFmpeg
	}
}

// nativeSinkIfEnabled returns the native publisher, creating it on first use.
func (v *Viewer) nativeSinkIfEnabled() *nativeSink {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.publisher != PublisherNative || v.closed {
		return nil
	}
	if v.sink == nil {
		v.sink = newNativeSink(v.rtspURL, v.model, v.hasAudio, v.ffmpeg)
	}
	return v.sink
}

// VideoInfo reports the published stream's size and frame rate, once known
// (built-in publisher only).
func (v *Viewer) VideoInfo() (VideoInfo, bool) {
	v.mu.Lock()
	sink := v.sink
	v.mu.Unlock()
	if sink == nil {
		return VideoInfo{}, false
	}
	return sink.VideoInfo()
}

// rtpPayloadBounds returns the payload offsets of an RTP packet, honouring
// CSRCs, header extensions and padding. ok=false means no usable payload.
func rtpPayloadBounds(buf []byte, n int) (start, end int, ok bool) {
	if n < 12 {
		return 0, 0, false
	}
	hlen := 12 + 4*int(buf[0]&0x0f)
	if (buf[0]>>4)&1 == 1 && n >= hlen+4 {
		extLen := int(binary.BigEndian.Uint16(buf[hlen+2 : hlen+4]))
		hlen += 4 + 4*extLen
	}
	end = n
	if buf[0]&0x20 != 0 { // padding: last byte is the pad length
		pad := int(buf[n-1])
		if pad >= end-hlen {
			return 0, 0, false
		}
		end -= pad
	}
	if end <= hlen {
		return 0, 0, false
	}
	return hlen, end, true
}

func iceServers(vc *VideoCall) []webrtc.ICEServer {
	var out []webrtc.ICEServer
	for _, i := range vc.UserICEs {
		s := webrtc.ICEServer{URLs: []string{i.URL}}
		if i.Username != "" {
			s.Username = i.Username
			s.Credential = i.Password
		}
		out = append(out, s)
	}
	return out
}

func NewViewer(sig *Signaling, vc *VideoCall, uuid, model, callID, ffmpeg, rtspURL string, rtpPort int) (*Viewer, error) {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH265, ClockRate: 90000, RTCPFeedback: []webrtc.RTCPFeedback{
			{Type: "goog-remb"}, {Type: "transport-cc"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"},
		}},
		PayloadType: 127,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "audio/AAC", ClockRate: 16000, Channels: 1},
		PayloadType:        97,
	}, webrtc.RTPCodecTypeAudio)

	se := webrtc.SettingEngine{LoggerFactory: newPionLoggerFactory(model)}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers(vc)})
	if err != nil {
		return nil, err
	}
	// Create local outgoing audio track for talkback (16kHz mono AAC)
	audioTrack, err := newTalkTrack()
	if err != nil {
		return nil, fmt.Errorf("create audio track: %w", err)
	}

	// Add audio transceiver with Sendrecv direction (MUST be mid 0)
	audioTr, err := pc.AddTransceiverFromTrack(audioTrack, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendrecv,
	})
	if err != nil {
		return nil, fmt.Errorf("add audio transceiver: %w", err)
	}

	// Add video transceiver with Recvonly direction (MUST be mid 1)
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		return nil, fmt.Errorf("add video transceiver: %w", err)
	}

	hasAudio := downlinkAudioEnabled(uuid)
	v := &Viewer{
		sig:            sig,
		pc:             pc,
		vc:             vc,
		callID:         callID,
		uuid:           uuid,
		model:          model,
		ffmpeg:         ffmpeg,
		rtspURL:        rtspURL,
		rtpPort:        rtpPort,
		doneChan:       make(chan struct{}),
		lastPacketTime: time.Now(),
		hasAudio:       hasAudio,
		audioTrack:     audioTrack,
		lastAudioDown:  time.Now(),
		lastAudioUp:    time.Now(),
		audioHdrByte2:  0x6C,
		rtpSeq:         uint16(rand.Intn(65535)),
		rtpTimestamp:   uint32(rand.Intn(1000000)),
	}
	sig.BindCamera(uuid, model)

	// Idle talk auto-timeout watchdog (auto-stops talkback if no audio streamed for 5.0s per test_t2_talk_timeout.py)
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-v.doneChan:
				return
			case <-ticker.C:
				if v.IsTalkActive() && v.IdleDuration() >= 5*time.Second {
					fmt.Printf("[%s] [TALK_WATCHDOG] Talk session idle > 5s without audio; auto-stopping\n", v.model)
					_, _ = v.SetTalk(false)
				}
			}
		}
	}()

	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) {
		fmt.Printf("[%s] ice %s\n", model, st)
		if st == webrtc.ICEConnectionStateFailed || st == webrtc.ICEConnectionStateClosed {
			v.Close()
		}
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		fmt.Printf("[%s] pc %s\n", model, st)
		if st == webrtc.PeerConnectionStateConnected {
			v.mu.Lock()
			v.lastPacketTime = time.Now()
			v.mu.Unlock()
			go v.reportMediaPath()
		}
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			v.Close()
		}
	})

	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand == nil {
			return
		}
		ci := cand.ToJSON()
		v.sig.send(cmdIceCandidate, map[string]any{
			"SessionId":           v.vc.SessionID,
			"WebrtcCandidate":     ci.Candidate,
			"WebrtcSdpMid":        "0",
			"WebrtcSdpMLineIndex": 0,
			"call_id":             v.callID,
		})
	})

	pc.OnTrack(func(tr *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		mime := strings.ToLower(tr.Codec().MimeType)
		fmt.Printf("[%s] track %s pt=%d ssrc=%d\n", model, tr.Codec().MimeType, tr.PayloadType(), tr.SSRC())
		// The camera's sender reports carry its own clock for the track
		// (diagnostics: how our audio and video timelines line up).
		if sink := v.nativeSinkIfEnabled(); sink != nil && receiver != nil {
			kind := 0
			if strings.Contains(mime, "audio") || strings.Contains(mime, "aac") {
				kind = 1
			}
			go readSenderReports(receiver, uint32(tr.SSRC()), tr.Codec().ClockRate, kind, sink)
		}
		if strings.Contains(mime, "audio") || strings.Contains(mime, "aac") {
			if !v.hasAudio {
				fmt.Printf("[%s] audio track ignored (explicit video-only configuration)\n", model)
				return
			}
			fmt.Printf("[%s] audio track received\n", model)
			sink := v.nativeSinkIfEnabled()
			go func() {
				buf := make([]byte, 2048)
				for {
					v.mu.Lock()
					closed := v.closed
					v.mu.Unlock()
					if closed {
						return
					}

					n, _, err := tr.Read(buf)
					if err != nil {
						return
					}
					hlen, end, ok := rtpPayloadBounds(buf, n)
					if !ok {
						continue
					}
					n = end

					v.observeCameraAudio(buf[hlen:n], binary.BigEndian.Uint32(buf[4:8]), buf[1]&0x80 != 0, tr.Codec().ClockRate, uint8(tr.PayloadType()))
					v.mu.Lock()
					if !v.gotFirstAudio && n >= hlen+7 {
						v.gotFirstAudio = true
						v.audioHdrByte2 = buf[hlen+2]
					}
					v.lastPacketTime = time.Now()
					v.gotFirstPacket = true
					v.lastAudioDown = time.Now()
					audioWriter := v.ffAudio
					v.mu.Unlock()
					isTalk := v.IsTalkActive()

					// Verify active candidate pair is typ host in allowed subnets before triggering OnMediaActive
					var onActive func()
					v.mu.Lock()
					needCheck := !v.mediaActiveTriggered && v.OnMediaActive != nil
					v.mu.Unlock()

					if needCheck && v.verifyActiveCandidatePair() {
						v.mu.Lock()
						if !v.mediaActiveTriggered && v.OnMediaActive != nil {
							v.mediaActiveTriggered = true
							onActive = v.OnMediaActive
						}
						v.mu.Unlock()
						if onActive != nil {
							go onActive()
						}
					}

					if isTalk {
						// Mic suppressed during active talk session to prevent acoustic feedback
						continue
					}

					if sink != nil {
						sink.OnAudioRTPClock(buf[hlen:n], binary.BigEndian.Uint32(buf[4:8]), tr.Codec().ClockRate)
						continue
					}

					if audioWriter != nil {
						v.audioMu.Lock()
						if v.ffAudio != nil {
							_, _ = v.ffAudio.Write(buf[hlen:n])
						}
						v.audioMu.Unlock()
					}
				}
			}()
			return
		}
		if !strings.Contains(mime, "h265") && !strings.Contains(mime, "hevc") && !strings.Contains(mime, "h264") {
			return
		}

		// Send immediate PLI and repeat every 2.5 seconds to ensure fast recovery and steady keyframes
		go func() {
			t := time.NewTicker(2500 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-v.doneChan:
					return
				case <-t.C:
					v.mu.Lock()
					closed := v.closed
					v.mu.Unlock()
					if closed {
						return
					}
					_ = pc.WriteRTCP([]rtcp.Packet{
						&rtcp.PictureLossIndication{MediaSSRC: uint32(tr.SSRC())},
					})
				}
			}
		}()

		sink := v.nativeSinkIfEnabled()
		if sink == nil {
			if err := v.startFFmpeg(); err != nil {
				fmt.Printf("[%s] ffmpeg start error: %v\n", model, err)
				v.Close()
				return
			}
		} else {
			fmt.Printf("[%s] publishing natively to %s (no FFmpeg)\n", model, v.rtspURL)
		}

		startCode := []byte{0x00, 0x00, 0x00, 0x01}
		buf := make([]byte, 2048)

		for {
			n, _, err := tr.Read(buf)
			if err != nil {
				fmt.Printf("[%s] track ended: %v\n", model, err)
				v.Close()
				return
			}
			hlen, end, ok := rtpPayloadBounds(buf, n)
			if !ok {
				continue
			}

			v.mu.Lock()
			v.lastPacketTime = time.Now()
			v.gotFirstPacket = true
			stdin := v.ffStdin
			v.mu.Unlock()

			// Verify active candidate pair is typ host in allowed subnets before triggering OnMediaActive
			var onActive func()
			v.mu.Lock()
			needCheck := !v.mediaActiveTriggered && v.OnMediaActive != nil
			v.mu.Unlock()

			if needCheck && v.verifyActiveCandidatePair() {
				v.mu.Lock()
				if !v.mediaActiveTriggered && v.OnMediaActive != nil {
					v.mediaActiveTriggered = true
					onActive = v.OnMediaActive
				}
				v.mu.Unlock()
				if onActive != nil {
					go onActive()
				}
			}

			p := buf[hlen:end]

			if sink != nil {
				sink.OnVideoPacket(p, binary.BigEndian.Uint32(buf[4:8]), buf[1]&0x80 != 0)
				continue
			}

			// FFmpeg exited (e.g. MediaMTX restarted): start a new one instead of
			// silently discarding video for the rest of the session.
			if stdin == nil {
				v.mu.Lock()
				restart := !v.closed && v.ff == nil && time.Since(v.lastFFStart) > 3*time.Second
				v.mu.Unlock()
				if restart {
					fmt.Printf("[%s] FFmpeg is not running; restarting it\n", model)
					if err := v.startFFmpeg(); err != nil {
						fmt.Printf("[%s] ffmpeg restart error: %v\n", model, err)
					}
					v.mu.Lock()
					stdin = v.ffStdin
					v.mu.Unlock()
				}
			}
			nalType := (p[0] >> 1) & 0x3f

			var pkt []byte
			if nalType == 49 && len(p) >= 3 {
				s := (p[2] >> 7) & 1
				fuType := p[2] & 0x3f
				if s == 1 {
					var nalHdr byte = 0x41 // non-IDR P-slice
					if fuType == 18 || fuType == 19 || fuType == 20 {
						nalHdr = 0x65 // IDR keyframe slice
					}
					firstByte := p[1]
					pkt = make([]byte, 4+2+len(p[3:]))
					copy(pkt[0:4], startCode)
					pkt[4] = nalHdr
					pkt[5] = firstByte
					copy(pkt[6:], p[3:])
				} else {
					pkt = make([]byte, len(p[3:]))
					copy(pkt, p[3:])
				}
			} else if len(p) > 0 {
				// Single NAL unit (e.g. SPS or PPS)
				pkt = make([]byte, 4+len(p))
				copy(pkt[0:4], startCode)
				copy(pkt[4:], p)
			}

			if len(pkt) > 0 && stdin != nil {
				_, _ = stdin.Write(pkt)
			}
		}
	})

	// RTCP the camera sends about our (talkback) audio stream arrives on the
	// audio sender: counting it shows whether our packets reach the camera.
	go func(sender *webrtc.RTPSender) {
		for {
			pkts, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			if n := v.audioRTCP.Add(int64(len(pkts))); n == int64(len(pkts)) {
				fmt.Printf("[%s] [TALK] camera is sending RTCP about our audio stream (first: %T)\n", v.model, pkts[0])
			}
		}
	}(audioTr.Sender())

	return v, nil
}

// buildFFmpegArgs constructs the FFmpeg CLI arguments for RTSP publication.
// When hasAudio is true, audio is downlinked via loopback TCP socket (audioURL)
// avoiding inherited file descriptor passing which fails on Windows.
func buildFFmpegArgs(hasAudio bool, audioURL, rtspURL string) []string {
	if hasAudio {
		return []string{
			"-hide_banner",
			"-loglevel", "warning",
			"-y",
			"-thread_queue_size", "10240",
			"-fflags", "+genpts",
			"-use_wallclock_as_timestamps", "1",
			"-f", "h264",
			"-i", "pipe:0",
			"-thread_queue_size", "10240",
			"-fflags", "+genpts",
			"-f", "aac",
			"-i", audioURL,
			// Two audio tracks: AAC for HLS/RTSP consumers, G.711 for browsers
			// (WebRTC cannot carry AAC; MediaMTX picks the track each reader supports).
			"-map", "0:v:0",
			"-map", "1:a:0",
			"-map", "1:a:0",
			"-c:v", "copy",
			"-af", "aresample=async=1",
			"-c:a:0", "aac",
			"-b:a:0", "32k",
			"-ar:a:0", "8000",
			"-ac:a:0", "1",
			"-c:a:1", "pcm_mulaw",
			"-ar:a:1", "8000",
			"-ac:a:1", "1",
			"-f", "rtsp",
			rtspURL,
		}
	}
	return []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-y",
		"-thread_queue_size", "10240",
		"-fflags", "+genpts",
		"-use_wallclock_as_timestamps", "1",
		"-f", "h264",
		"-i", "pipe:0",
		"-c:v", "copy",
		"-an",
		"-f", "rtsp",
		rtspURL,
	}
}

func (v *Viewer) startFFmpeg() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ff != nil || v.closed {
		return nil
	}
	v.lastFFStart = time.Now()

	var audioListener net.Listener
	var audioURL string

	if v.hasAudio {
		var err error
		audioListener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("audio loopback listener: %w", err)
		}
		port := audioListener.Addr().(*net.TCPAddr).Port
		audioURL = fmt.Sprintf("tcp://127.0.0.1:%d?timeout=5000000", port)
	}

	args := buildFFmpegArgs(v.hasAudio, audioURL, v.rtspURL)
	cmd := exec.Command(v.ffmpeg, args...)
	winproc.Hide(cmd)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	stdin, err := cmd.StdinPipe()
	if err != nil {
		if audioListener != nil {
			_ = audioListener.Close()
		}
		return fmt.Errorf("stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		if audioListener != nil {
			_ = audioListener.Close()
		}
		_ = stdin.Close()
		return fmt.Errorf("ffmpeg start: %w", err)
	}

	v.ff = cmd
	v.ffStdin = stdin
	v.audioListener = audioListener
	v.ffAudio = nil
	fmt.Printf("[%s] FFmpeg started (hasAudio=%v), publishing to %s\n", v.model, v.hasAudio, v.rtspURL)

	if v.hasAudio && audioListener != nil {
		go v.acceptAudioConnection(audioListener)
	}

	if v.hasAudio {
		// Silence watchdog: when physical camera suppresses mic in talk mode or downlink pauses,
		// feed silent ADTS AAC frames every 64ms so FFmpeg pipe:3 never starves or deadlocks.
		go func() {
			ticker := time.NewTicker(64 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-v.doneChan:
					return
				case <-ticker.C:
					v.mu.Lock()
					closed := v.closed
					w := v.ffAudio
					lastDown := v.lastAudioDown
					hdrByte2 := v.audioHdrByte2
					v.mu.Unlock()
					isTalk := v.IsTalkActive()

					if closed {
						return
					}
					if w == nil {
						continue
					}

					if isTalk || time.Since(lastDown) > 150*time.Millisecond {
						silenceFrame := []byte{0xFF, 0xF1, hdrByte2, 0x40, 0x01, 0x7F, 0xFC, 0x01, 0x18, 0x20, 0x07}
						v.audioMu.Lock()
						if v.ffAudio != nil {
							_, _ = v.ffAudio.Write(silenceFrame)
							v.mu.Lock()
							v.lastAudioDown = time.Now()
							v.mu.Unlock()
						}
						v.audioMu.Unlock()
					}
				}
			}
		}()
	}

	go func() {
		err := cmd.Wait()
		v.audioMu.Lock()
		v.mu.Lock()
		if v.audioListener != nil {
			_ = v.audioListener.Close()
			v.audioListener = nil
		}
		if v.ffAudio != nil {
			_ = v.ffAudio.Close()
			v.ffAudio = nil
		}
		v.ff = nil
		v.ffStdin = nil
		v.mu.Unlock()
		v.audioMu.Unlock()
		fmt.Printf("[%s] FFmpeg exited: %v\n", v.model, err)
	}()

	return nil
}

func (v *Viewer) acceptAudioConnection(l net.Listener) {
	// Set 10-second accept deadline to prevent resource leaks if FFmpeg fails to connect
	if tcpL, ok := l.(*net.TCPListener); ok {
		_ = tcpL.SetDeadline(time.Now().Add(10 * time.Second))
	}

	conn, err := l.Accept()
	if err != nil {
		v.mu.Lock()
		closed := v.closed
		v.mu.Unlock()
		if !closed {
			fmt.Printf("[%s] audio loopback accept failed: %v\n", v.model, err)
		}
		_ = l.Close()
		return
	}

	// Close listener immediately as only 1 connection is expected
	_ = l.Close()

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
	}

	v.audioMu.Lock()
	v.mu.Lock()
	if v.closed || v.ff == nil {
		v.mu.Unlock()
		v.audioMu.Unlock()
		_ = conn.Close()
		return
	}
	v.ffAudio = conn
	hdrByte2 := v.audioHdrByte2
	v.audioListener = nil
	v.mu.Unlock()

	// Prime FFmpeg AAC demuxer immediately with an initial ADTS silence frame
	silenceFrame := []byte{0xFF, 0xF1, hdrByte2, 0x40, 0x01, 0x7F, 0xFC, 0x01, 0x18, 0x20, 0x07}
	_, _ = conn.Write(silenceFrame)
	v.audioMu.Unlock()

	fmt.Printf("[%s] FFmpeg audio downlink loopback connected on %s\n", v.model, conn.LocalAddr().String())
}

func (v *Viewer) stopFFmpeg() {
	v.audioMu.Lock()
	v.mu.Lock()
	if v.audioListener != nil {
		_ = v.audioListener.Close()
		v.audioListener = nil
	}
	if v.ffStdin != nil {
		_ = v.ffStdin.Close()
		v.ffStdin = nil
	}
	if v.ffAudio != nil {
		_ = v.ffAudio.Close()
		v.ffAudio = nil
	}
	if v.ff != nil && v.ff.Process != nil {
		_ = v.ff.Process.Kill()
		v.ff = nil
	}
	v.mu.Unlock()
	v.audioMu.Unlock()
}

func (v *Viewer) Close() {
	v.closeOnce.Do(func() {
		v.mu.Lock()
		v.closed = true
		v.mu.Unlock()

		v.stopFFmpeg()
		v.StopTalkAudio()
		v.mu.Lock()
		sink := v.sink
		v.mu.Unlock()
		if sink != nil {
			sink.Close()
		}

		if v.pc != nil {
			_ = v.pc.Close()
		}
		if v.sig != nil {
			_ = v.sig.Close()
		}
		close(v.doneChan)
		fmt.Printf("[%s] viewer closed cleanly\n", v.model)
	})
}

func (v *Viewer) Done() <-chan struct{} {
	return v.doneChan
}

func (v *Viewer) HasStalled() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return true
	}
	if v.gotFirstPacket {
		return time.Since(v.lastPacketTime) > 15*time.Second
	}
	return time.Since(v.lastPacketTime) > 60*time.Second
}

func (v *Viewer) iceEnvelope() (urls, users, passes []string) {
	for _, ic := range v.vc.DeviceICEs {
		u := ic.URL
		if strings.HasPrefix(u, "turn:") && !strings.Contains(u, "transport=") {
			u += "?transport=udp"
		}
		urls = append(urls, u)
		users = append(users, ic.Username)
		passes = append(passes, ic.Password)
	}
	return
}

func (v *Viewer) Start() error {
	offer, err := v.pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := v.pc.SetLocalDescription(offer); err != nil {
		return err
	}
	ufrag, pwd, fp := extractCrypto(offer.SDP)
	if ufrag == "" || fp == "" {
		return fmt.Errorf("could not extract crypto")
	}

	// Extract Pion's allocated audio SSRC and CNAME for compact SDP synchronization
	pionSSRC, pionCNAME := extractAudioSSRCAndCNAME(offer.SDP)
	webrtcSdp, _, _ := encodeOffer(ufrag, pwd, fp, pionSSRC, pionCNAME)
	urls, users, passes := v.iceEnvelope()
	if err := v.sig.send(cmdSdpOffer, map[string]any{
		"SessionId": v.vc.SessionID, "IceUrl": urls, "IceUsername": users, "IcePassword": passes,
		"WebrtcSdp": webrtcSdp, "Action": 2, "Timestamp": 0, "TrickleICE": false, "CodecMode": 1,
		"EnableSpeaker": false, "EnableMic": false, "Quality": 1, "DtlsWaitTime": 5000, "call_id": v.callID,
	}); err != nil {
		return err
	}
	fmt.Printf("[%s] offer sent\n", v.model)
	return nil
}

// sessionLogin is the Osaio login (user ID) part of a SessionId such as
// "usf0...:e56:ODMw...".
func sessionLogin(sid string) string {
	login, _, _ := strings.Cut(sid, ":")
	return login
}

// foreignDetail describes what the camera told another viewer (the Osaio
// app), for the speaker investigation: the audio section of its answer and
// the return codes of talk replies. No ICE credentials or session IDs.
func foreignDetail(method string, data map[string]any) string {
	switch {
	case strings.Contains(method, "SdpAnswer"):
		if sdp, _ := data["WebrtcSdp"].(string); sdp != "" {
			return "camera audio section for that viewer: " + compactAudioSummary(sdp)
		}
	case strings.Contains(method, "Talk"):
		var parts []string
		for k, val := range data {
			if k == "SessionId" || k == "call_id" {
				continue
			}
			parts = append(parts, fmt.Sprintf("%s=%v", k, val))
		}
		sort.Strings(parts)
		return "talk reply for that viewer: " + strings.Join(parts, " ")
	}
	return ""
}

// noteForeignSession logs the first message of each other viewer's session
// and remembers when a viewer using BombeCam's own login was last active.
func (v *Viewer) noteForeignSession(sid, method string, data map[string]any) {
	sameLogin := v.vc != nil && sessionLogin(sid) != "" && sessionLogin(sid) == sessionLogin(v.vc.SessionID)
	v.mu.Lock()
	if v.foreignSeen == nil {
		v.foreignSeen = map[string]bool{}
	}
	seen := v.foreignSeen[sid]
	v.foreignSeen[sid] = true
	key := sid + "|" + method
	methodSeen := v.foreignSeen[key]
	v.foreignSeen[key] = true
	if d := foreignDetail(method, data); d != "" && (!methodSeen || strings.Contains(method, "Talk")) {
		defer fmt.Printf("[%s] another viewer: %s: %s\n", v.model, method, d)
	}
	if sameLogin {
		v.sameLoginAt = time.Now()
	}
	v.mu.Unlock()
	switch {
	case seen:
	case sameLogin:
		fmt.Printf("[%s] the Osaio app is watching this camera with the same Osaio login BombeCam uses; the camera may send BombeCam no video until that view closes. Give BombeCam its own Osaio login to avoid this.\n", v.model)
	default:
		fmt.Printf("[%s] another viewer (such as the Osaio app) is connecting to the camera; its messages (%s...) are ignored here\n", v.model, method)
	}
}

// SameLoginViewerActive reports whether a viewer using BombeCam's own Osaio
// login showed activity on this camera within the last d.
func (v *Viewer) SameLoginViewerActive(d time.Duration) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return !v.sameLoginAt.IsZero() && time.Since(v.sameLoginAt) < d
}

func (v *Viewer) OnMessage(method string, data any) {
	if data == nil {
		return
	}
	fmt.Printf("[%s] WS recv: %s\n", v.model, method)
	dataMap, ok := data.(map[string]any)
	if !ok {
		return
	}
	// The vendor socket also carries the camera's replies to other viewers,
	// such as the Osaio app (another SessionId). Acting on them would add the
	// camera's candidates for the app's connection to BombeCam's own, and
	// take the app's talk replies as BombeCam's.
	if sid, _ := dataMap["SessionId"].(string); sid != "" && v.vc != nil && sid != v.vc.SessionID {
		v.noteForeignSession(sid, method, dataMap)
		return
	}
	switch {
	case method == evtSdpAnswer1 || method == evtSdpAnswer2:
		sid, _ := dataMap["SessionId"].(string)
		ret, _ := dataMap["Ret"].(float64)
		sdp, _ := dataMap["WebrtcSdp"].(string)
		fmt.Printf("[%s] SdpAnswer: sid=%s (expected=%s) ret=%v sdpLen=%d state=%s\n",
			v.model, sid, v.vc.SessionID, ret, len(sdp), v.pc.SignalingState().String())
		if sid != "" && sid != v.vc.SessionID {
			fmt.Printf("[%s] SessionId mismatch!\n", v.model)
			return
		}
		if v.pc.SignalingState() != webrtc.SignalingStateHaveLocalOffer {
			fmt.Printf("[%s] State is not HaveLocalOffer (is %s)\n", v.model, v.pc.SignalingState().String())
			return
		}
		if ret != 0 {
			fmt.Printf("[%s] answer Ret=%v\n", v.model, ret)
			return
		}
		if sdp == "" {
			fmt.Printf("[%s] sdp is empty in data: %+v\n", v.model, data)
			return
		}
		real, err := decodeAnswer(sdp)
		if err != nil {
			fmt.Printf("[%s] decode answer err: %v\n", v.model, err)
			return
		}
		if err := v.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: real}); err != nil {
			fmt.Printf("[%s] setRemote err: %v\n", v.model, err)
			return
		}
		fmt.Printf("[%s] answer OK; camera audio section: %s\n", v.model, compactAudioSummary(sdp))
		for _, cand := range v.pendingICE {
			if err := v.pc.AddICECandidate(cand); err != nil {
				fmt.Printf("[%s] drained ice err: %v\n", v.model, err)
			}
		}
		v.pendingICE = nil
		v.sig.send(cmdSwitch, map[string]any{"SessionId": v.vc.SessionID, "Action": 0, "Quality": 2, "Timestamp": 0, "call_id": v.callID})
	case method == evtIce1 || method == evtIce2:
		cand, _ := dataMap["WebrtcCandidate"].(string)
		if cand == "" {
			return
		}

		parsedCand, err := netstack.ParseCandidate(cand)
		if err != nil {
			fmt.Printf("[%s] ignoring malformed candidate: %v (raw: %q)\n", v.model, err, cand)
			return
		}

		if v.OnCandidateHostIP != nil && strings.ToLower(parsedCand.Type) == "host" && strings.ToLower(parsedCand.Transport) == "udp" {
			if parsedIP := net.ParseIP(parsedCand.Address); parsedIP != nil && v.VerifyCandidateIP(parsedIP) {
				go v.OnCandidateHostIP(parsedIP.String())
			}
		}

		mid, _ := dataMap["WebrtcSdpMid"].(string)
		idx := uint16(0)
		if f, ok := dataMap["WebrtcSdpMLineIndex"].(float64); ok {
			idx = uint16(f)
		}
		cInit := webrtc.ICECandidateInit{Candidate: cand, SDPMid: &mid, SDPMLineIndex: &idx}
		if v.pc == nil || v.pc.RemoteDescription() == nil {
			v.pendingICE = append(v.pendingICE, cInit)
			return
		}
		if err := v.pc.AddICECandidate(cInit); err != nil {
			fmt.Printf("[%s] ice add err: %v\n", v.model, err)
		}
	case strings.Contains(method, "TalkResp") || strings.Contains(method, "Talk"):
		ret := 0
		if r, ok := dataMap["enableSpeakerRet"].(float64); ok {
			ret = int(r)
		} else if r, ok := dataMap["Ret"].(float64); ok {
			ret = int(r)
		} else if r, ok := dataMap["ret"].(float64); ok {
			ret = int(r)
		} else if sub, ok := dataMap["data"].(map[string]any); ok {
			if r, ok := sub["enableSpeakerRet"].(float64); ok {
				ret = int(r)
			} else if r, ok := sub["Ret"].(float64); ok {
				ret = int(r)
			} else if r, ok := sub["ret"].(float64); ok {
				ret = int(r)
			}
		}
		fmt.Printf("[%s] [TALK] camera answered: method=%s ret=%d data=%v\n", v.model, method, ret, dataMap)
		v.talkMu.Lock()
		if v.talkWaitCh != nil {
			select {
			case v.talkWaitCh <- ret:
			default:
			}
			v.talkWaitCh = nil
		}
		v.talkMu.Unlock()
	}
}

func (v *Viewer) NextSeq() uint16 {
	v.rtpMu.Lock()
	defer v.rtpMu.Unlock()
	v.rtpSeq++
	return v.rtpSeq
}

func (v *Viewer) NextTimestamp(delta uint32) uint32 {
	v.rtpMu.Lock()
	defer v.rtpMu.Unlock()
	v.rtpTimestamp += delta
	return v.rtpTimestamp
}

// SetControlChannel binds a ControlChannel (such as MQTTControlChannel) for local talkback arming.
func (v *Viewer) SetControlChannel(cc ControlChannel) {
	v.talkMu.Lock()
	defer v.talkMu.Unlock()
	v.ctrlChan = cc
}

// SetAllowedSubnets sets multiple allowed camera subnets for WebRTC candidate pair verification.
func (v *Viewer) SetAllowedSubnets(cidrs []string) error {
	subnets, err := netstack.ParseCIDRs(cidrs)
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.allowedSubnets = subnets
	v.mu.Unlock()
	return nil
}

// SetAllowedSubnet sets a single allowed camera subnet for WebRTC candidate pair verification.
func (v *Viewer) SetAllowedSubnet(cidr string) error {
	return v.SetAllowedSubnets([]string{cidr})
}

// GetActiveRemoteCandidate returns the active selected remote ICE candidate from the PeerConnection.
func (v *Viewer) GetActiveRemoteCandidate() (*webrtc.ICECandidate, error) {
	if v.pc == nil {
		return nil, fmt.Errorf("peer connection is nil")
	}
	for _, recv := range v.pc.GetReceivers() {
		if dtls := recv.Transport(); dtls != nil {
			if ice := dtls.ICETransport(); ice != nil {
				if pair, err := ice.GetSelectedCandidatePair(); err == nil && pair != nil {
					return pair.Remote, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("no active selected candidate pair")
}

func (v *Viewer) selectedPair() (*webrtc.ICECandidatePair, error) {
	if v.pc == nil {
		return nil, fmt.Errorf("peer connection is nil")
	}
	for _, recv := range v.pc.GetReceivers() {
		if dtls := recv.Transport(); dtls != nil {
			if ice := dtls.ICETransport(); ice != nil {
				if pair, err := ice.GetSelectedCandidatePair(); err == nil && pair != nil {
					return pair, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("no active selected candidate pair")
}

// MediaPathKind classifies a selected ICE pair: "relay" if either side is a
// TURN relay, "lan" if the camera's address is a host candidate on the local
// network, otherwise "internet".
func (v *Viewer) MediaPathKind(pair *webrtc.ICECandidatePair) string {
	if pair == nil || pair.Local == nil || pair.Remote == nil {
		return ""
	}
	if pair.Local.Typ == webrtc.ICECandidateTypeRelay || pair.Remote.Typ == webrtc.ICECandidateTypeRelay {
		return "relay"
	}
	if pair.Remote.Typ == webrtc.ICECandidateTypeHost {
		if ip := net.ParseIP(pair.Remote.Address); ip != nil && v.VerifyCandidateIP(ip) {
			return "lan"
		}
	}
	return "internet"
}

// reportMediaPath logs (and reports through OnMediaPath) how the media travels.
func (v *Viewer) reportMediaPath() {
	for i := 0; i < 20; i++ {
		if pair, err := v.selectedPair(); err == nil {
			if kind := v.MediaPathKind(pair); kind != "" {
				remote := fmt.Sprintf("%s:%d (%s)", pair.Remote.Address, pair.Remote.Port, pair.Remote.Typ)
				desc := map[string]string{
					"lan":      "direct on your local network",
					"internet": "direct over the internet (not on your LAN)",
					"relay":    "RELAYED through the vendor's TURN server (not local)",
				}[kind]
				fmt.Printf("[%s] media path: %s, camera at %s\n", v.model, desc, remote)
				if cb := v.OnMediaPath; cb != nil {
					cb(kind, remote)
				}
				return
			}
		}
		select {
		case <-v.doneChan:
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// VerifyCandidateIP checks if a given IP address is acceptable under the Viewer's subnet configuration.
func (v *Viewer) VerifyCandidateIP(ip net.IP) bool {
	if ip == nil || netstack.IsInvalidAddress(ip) {
		return false
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}

	v.mu.Lock()
	subnets := make([]*net.IPNet, len(v.allowedSubnets))
	copy(subnets, v.allowedSubnets)
	v.mu.Unlock()

	if len(subnets) > 0 {
		for _, sn := range subnets {
			if sn.Contains(v4) {
				return true
			}
		}
		return false
	}

	// Dynamic detection fallback: check against host network subnets or valid private LAN candidate
	filter, err := netstack.NewICECandidateFilter(nil, nil)
	if err == nil {
		return filter.IsAllowedIP(v4)
	}

	return netstack.IsRFC1918PrivateIP(v4)
}

// ValidateRemoteCandidate validates that an active remote candidate adheres to RFC 5245 bounds,
// uses UDP transport, is of type 'host', and belongs to permitted camera subnets.
func (v *Viewer) ValidateRemoteCandidate(cand *webrtc.ICECandidate) bool {
	if cand == nil {
		return false
	}
	if cand.Typ != webrtc.ICECandidateTypeHost {
		return false
	}
	if cand.Protocol != webrtc.ICEProtocolUDP {
		return false
	}
	if cand.Port < 1 || cand.Port > 65535 {
		return false
	}
	if cand.Component < 1 || cand.Component > 256 {
		return false
	}
	ip := net.ParseIP(cand.Address)
	if ip == nil {
		return false
	}
	return v.VerifyCandidateIP(ip)
}

// verifyActiveCandidatePair verifies that the active remote candidate is of type 'host'
// and has an IP belonging to the configured allowed subnets or dynamic host subnets.
func (v *Viewer) verifyActiveCandidatePair() bool {
	pair, err := v.selectedPair()
	if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil || pair.Local.Typ == webrtc.ICECandidateTypeRelay {
		return false
	}
	return v.ValidateRemoteCandidate(pair.Remote)
}

func (v *Viewer) SetTalk(enable bool) (int, error) {
	v.talkCommandMu.Lock()
	defer v.talkCommandMu.Unlock()
	v.talkMu.Lock()
	cc := v.ctrlChan
	if v.talkActive && enable {
		v.talkMu.Unlock()
		return 0, nil
	}
	if !enable {
		// Local audio stays stopped even if the remote stop cannot be sent.
		v.talkActive = false
	}
	v.talkMu.Unlock()
	if !enable {
		v.StopTalkAudio()
	}

	// 1. Arm via a local control plane (MQTT shadow) if bound. The cloud
	//    signaling channel is skipped here: step 2 sends the same service.Talk
	//    with the real call SessionId, and a second one with a made-up session
	//    id could be answered with an error that step 2 then mistook for its own.
	if _, isCloud := cc.(*SignalingControlChannel); cc != nil && !isCloud {
		sessionID := ""
		if v.vc != nil {
			sessionID = v.vc.SessionID
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
		defer cancel()
		if err := cc.ArmTalk(ctx, v.uuid, enable, sessionID); err != nil {
			return -1, fmt.Errorf("local talk command unconfirmed: %w", err)
		}
		v.talkMu.Lock()
		v.talkActive = enable
		v.lastAudioUp = time.Now()
		v.talkAnswered, v.talkRet = true, 0
		v.talkMu.Unlock()
		fmt.Printf("[%s] [TALK] local camera report confirmed enable=%v\n", v.model, enable)
		return 0, nil
	}

	// 2. Dispatch service.Talk via active WebSocket signaling session if present
	if v.sig != nil {
		if v.vc == nil || v.vc.SessionID == "" {
			return -1, fmt.Errorf("talkback media call is not ready")
		}
		// EnableMic is set together with EnableSpeaker: the vendor fields are
		// ambiguous (camera speaker/mic, or app speaker/mic), and talking needs
		// the camera's speaker and the app's mic. The live view keeps
		// receiving the camera's sound either way.
		payload := map[string]any{
			"SessionId":     v.vc.SessionID,
			"EnableSpeaker": enable,
			"EnableMic":     enable,
			"call_id":       v.callID,
		}

		waitCh := make(chan int, 1)
		v.talkMu.Lock()
		v.talkWaitCh = waitCh
		v.talkMu.Unlock()

		fmt.Printf("[%s] [TALK] dispatching service.Talk enable=%v call_id=%s\n", v.model, enable, v.callID)
		if err := v.sig.send("service.Talk", payload); err != nil {
			v.talkMu.Lock()
			v.talkWaitCh = nil
			v.talkMu.Unlock()
			fmt.Printf("[%s] [TALK] send service.Talk warning: %v\n", v.model, err)
			return -1, fmt.Errorf("send talk command: %w", err)
		} else {
			select {
			case ret := <-waitCh:
				v.talkMu.Lock()
				if ret == 0 {
					v.talkActive = enable
					v.lastAudioUp = time.Now()
				}
				if enable {
					v.talkAnswered, v.talkRet = true, ret
				}
				v.talkMu.Unlock()
				return ret, nil
			case <-time.After(1500 * time.Millisecond):
				v.talkMu.Lock()
				v.talkActive = enable
				v.lastAudioUp = time.Now()
				v.talkWaitCh = nil
				if enable {
					v.talkAnswered, v.talkRet = false, 0
				}
				v.talkMu.Unlock()
				fmt.Printf("[%s] [TALK] command sent without a camera acknowledgement\n", v.model)
				return 0, nil
			case <-v.doneChan:
				v.talkMu.Lock()
				v.talkWaitCh = nil
				v.talkMu.Unlock()
				return -1, fmt.Errorf("talkback media session closed")
			}
		}
	}

	return -1, fmt.Errorf("no control channel or signaling available")
}

// TalkDiagnostics reports whether the camera answered the last talk request,
// its return code, how much audio has been sent, and how many RTCP packets the
// camera has sent about that audio.
func (v *Viewer) TalkDiagnostics() (answered bool, ret int, frames, bytes, rtcpPkts int64) {
	v.talkMu.Lock()
	answered, ret = v.talkAnswered, v.talkRet
	v.talkMu.Unlock()
	return answered, ret, v.talkFramesSent.Load(), v.talkBytesSent.Load(), v.audioRTCP.Load()
}

func (v *Viewer) IsTalkActive() bool {
	v.talkMu.Lock()
	defer v.talkMu.Unlock()
	return v.talkActive
}

func (v *Viewer) TouchAudioUp() {
	v.talkMu.Lock()
	v.lastAudioUp = time.Now()
	v.talkMu.Unlock()
}

func (v *Viewer) IdleDuration() time.Duration {
	v.talkMu.Lock()
	defer v.talkMu.Unlock()
	return time.Since(v.lastAudioUp)
}

func (v *Viewer) WriteAudioFrame(frame []byte) error {
	if v.audioTrack == nil {
		return fmt.Errorf("audio track not initialized")
	}
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    97,
			SequenceNumber: v.NextSeq(),
			Timestamp:      v.NextTimestamp(1024),
			Marker:         true,
		},
		Payload: frame,
	}
	return v.audioTrack.WriteRTP(pkt)
}

// readSenderReports passes the camera's RTCP sender reports for one track to
// the publisher.
func readSenderReports(rc *webrtc.RTPReceiver, ssrc, clock uint32, kind int, sink *nativeSink) {
	for {
		pkts, _, err := rc.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			if sr, ok := p.(*rtcp.SenderReport); ok && (sr.SSRC == ssrc || ssrc == 0) {
				sink.OnSenderReport(kind, sr.NTPTime, sr.RTPTime, clock)
			}
		}
	}
}

// ViewerCopyReady reports whether the video-only viewer copy is live (see
// nativeSink.writeViewerCopyLocked); its path is the camera's plus
// ViewerPathSuffix.
func (v *Viewer) ViewerCopyReady() bool {
	v.mu.Lock()
	sink := v.sink
	v.mu.Unlock()
	return sink != nil && sink.ViewerCopyReady()
}
