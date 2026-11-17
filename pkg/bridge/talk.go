package bridge

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"

	"github.com/Fever-r/BombeCam/internal/winproc"
)

// camAudioInfo describes the camera's own microphone stream as it arrives.
// Talkback mirrors it (sample rate, RTP clock step), on the principle that the
// camera's speaker path expects the same dialect its microphone speaks.
type camAudioInfo struct {
	sampleRate int
	channels   int
	clock      uint32
	pt         uint8
	tsStep     uint32
	packets    int
	markers    int
	framesSeen int
	lastTS     uint32
	lastFrames int
	logged     bool
	firstAt    time.Time
}

// observeCameraAudio records one downlink audio packet (payload = ADTS frames).
func (v *Viewer) observeCameraAudio(payload []byte, ts uint32, marker bool, clock uint32, pt uint8) {
	v.camAudioMu.Lock()
	defer v.camAudioMu.Unlock()
	a := &v.camAudio
	a.clock, a.pt = clock, pt
	frames := splitADTS(payload)
	if a.sampleRate == 0 {
		if cfg, ok := aacConfigFromADTS(payload); ok {
			a.sampleRate, a.channels = cfg.SampleRate, cfg.Channels
		}
	}
	if a.packets > 0 && a.lastFrames > 0 {
		if d := ts - a.lastTS; d > 0 && d < 1<<20 && d%uint32(a.lastFrames) == 0 && a.tsStep == 0 {
			a.tsStep = d / uint32(a.lastFrames)
		}
	}
	if a.packets == 0 {
		a.firstAt = time.Now()
	}
	a.packets++
	if marker {
		a.markers++
	}
	a.framesSeen += len(frames)
	a.lastTS, a.lastFrames = ts, len(frames)
	if !a.logged && a.packets >= 25 {
		a.logged = true
		fmt.Printf("[%s] [AUDIO] camera mic: AAC %d Hz, %d ch, RTP pt=%d clock=%d, ts step/frame=%d, frames/packet≈%.1f, marker on %d/%d packets, one packet every %d ms so far\n",
			v.model, a.sampleRate, a.channels, a.pt, a.clock, a.tsStep, float64(a.framesSeen)/float64(a.packets), a.markers, a.packets,
			time.Since(a.firstAt).Milliseconds()/int64(a.packets-1))
	}
}

// talkFormat returns the AAC sample rate and RTP timestamp step to use for
// talkback, mirroring the camera's microphone stream when it has been seen.
func (v *Viewer) talkFormat() (rate int, step uint32) {
	v.camAudioMu.Lock()
	a := v.camAudio
	v.camAudioMu.Unlock()
	rate = a.sampleRate
	if rate == 0 {
		rate = 16000
	}
	step = a.tsStep
	if step == 0 {
		clock := a.clock
		if clock == 0 {
			clock = 16000
		}
		step = uint32(1024 * int(clock) / rate)
	}
	return rate, step
}

// SetTalkFormat chooses how the next talk session is sent to the camera.
func (v *Viewer) SetTalkFormat(id string) error {
	if _, ok := TalkFormatByID(id); !ok {
		return fmt.Errorf("unknown talk format %q", id)
	}
	v.talkMu.Lock()
	v.talkFormatID = id
	v.talkMu.Unlock()
	return nil
}

// talkPlan resolves a format against the camera's own audio: the encoder
// rate, the RTP clock step per packet, the packet's real duration, and the
// rate the 16 kHz input is relabelled to (0: not needed).
//
// The WS03 sends one AAC frame every 120 ms and steps its RTP clock 960 per
// frame at 8 kHz, although an AAC frame decodes to 1024 samples (128 ms at
// 8 kHz): its audio really runs at about 8533 Hz. Its speaker plays our
// frames the same way, one per 120 ms. Sending 128 ms of voice per frame
// would make the camera run out of sound and fall further behind every frame,
// so each frame carries 120 ms of voice (the input is relabelled
// and resampled so 1024 samples cover 120 ms, which also keeps the pitch
// right) and frames go out every 120 ms.
func (v *Viewer) talkPlan(f TalkFormat) (rate int, step uint32, dur time.Duration, relabel int) {
	rate = f.rate
	clock := f.clock
	if f.mirror {
		v.camAudioMu.Lock()
		cam := v.camAudio
		v.camAudioMu.Unlock()
		rate, step = v.talkFormat()
		clock = cam.clock
		if clock == 0 {
			clock = uint32(rate)
		}
	}
	if rate == 0 {
		rate = 16000
	}
	if clock == 0 {
		clock = uint32(rate)
	}
	if step == 0 {
		step = uint32(uint64(f.samples) * uint64(clock) / uint64(rate))
	}
	dur = time.Duration(float64(time.Second) * float64(f.samples) / float64(rate))
	if f.mirror {
		real := time.Duration(float64(time.Second) * float64(step) / float64(clock))
		if d := real - dur; d > dur/100 || -d > dur/100 {
			// frames must cover `real` of voice: relabel 16 kHz input so that
			// after resampling to `rate`, f.samples samples span `real`
			relabel = int(math.Round(16000 * float64(rate) * real.Seconds() / float64(f.samples)))
			dur = real
		}
	}
	return rate, step, dur, relabel
}

// talkSession encodes browser PCM into the chosen talk format (with one
// long-running FFmpeg for AAC and Opus, in Go for G.711) and paces the
// packets onto the call's audio track in real time.
type talkSession struct {
	format TalkFormat
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	frames chan talkPacket
	stop   chan struct{}
	once   sync.Once
}

// TalkAudioRunning reports whether a talkback encoder is active.
func (v *Viewer) TalkAudioRunning() bool {
	v.talkMu.Lock()
	defer v.talkMu.Unlock()
	return v.talkSess != nil
}

// tailBuffer keeps the last few hundred bytes of FFmpeg's error output.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 600 {
		t.buf = t.buf[len(t.buf)-600:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// StartTalkAudio starts the talkback encoder (16 kHz s16le mono PCM in) in
// the chosen talk format.
func (v *Viewer) StartTalkAudio(ffmpegPath string) error {
	return v.startTalkAudio(ffmpegPath, false)
}

// StartActiveTalkAudio keeps an upload racing Stop from recreating an encoder.
func (v *Viewer) StartActiveTalkAudio(ffmpegPath string) error {
	return v.startTalkAudio(ffmpegPath, true)
}

func (v *Viewer) startTalkAudio(ffmpegPath string, requireActive bool) error {
	v.talkMu.Lock()
	defer v.talkMu.Unlock()
	if requireActive && !v.talkActive {
		return fmt.Errorf("talkback session is stopped")
	}
	if v.talkSess != nil {
		return nil
	}
	id := v.talkFormatID
	if id == "" {
		id = DefaultTalkFormatID
	}
	f, _ := TalkFormatByID(id)
	if f.NeedsFFmpeg() && ffmpegPath == "" {
		return fmt.Errorf("talkback in this format needs FFmpeg to encode your voice for the camera; install it (winget install Gyan.FFmpeg) and restart BombeCam")
	}
	rate, step, dur, relabel := v.talkPlan(f)
	s := &talkSession{format: f, frames: make(chan talkPacket, 256), stop: make(chan struct{})}

	if f.NeedsFFmpeg() {
		cmd := exec.Command(ffmpegPath, talkFFmpegArgs(f, rate, relabel)...)
		winproc.Hide(cmd)
		stderr := &tailBuffer{}
		cmd.Stderr = stderr
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start ffmpeg for talkback: %w", err)
		}
		s.cmd, s.stdin = cmd, stdin
		if f.codec == "opus" {
			go readOpusPackets(stdout, s, f)
		} else {
			go readADTSPackets(stdout, s, f)
		}
		go func() {
			err := cmd.Wait()
			select {
			case <-s.stop:
			default:
				if err != nil {
					msg := stderr.String()
					if msg == "" {
						msg = err.Error()
					}
					fmt.Printf("[%s] [TALK] encoder for %s exited: %s\n", v.model, f.ID, msg)
				}
				// An exited encoder cannot remain an apparently running session.
				s.once.Do(func() { close(s.stop); _ = s.stdin.Close() })
				v.talkMu.Lock()
				if v.talkSess == s {
					v.talkSess = nil
					v.talkActive = false
				}
				v.talkMu.Unlock()
			}
		}()
	} else {
		s.stdin = &g711Encoder{alaw: f.codec == "pcma", out: s.frames}
	}
	v.talkSess = s
	if v.audioTrack != nil {
		bound, ssrc, npt, codec := v.audioTrack.binding()
		pt := int(npt)
		if f.pt >= 0 {
			pt = f.pt
		}
		fmt.Printf("[%s] [TALK] sending %s: RTP pt %d (negotiated pt %d %s), ssrc %d, step %d per %d ms, track connected: %v\n",
			v.model, f.Label, pt, npt, codec, ssrc, step, dur.Milliseconds(), bound)
	} else {
		fmt.Printf("[%s] [TALK] sending %s: step %d per %d ms\n", v.model, f.Label, step, dur.Milliseconds())
	}

	// Pacer: one packet per packet-duration, on the call's audio track.
	go func() {
		next := time.Now()
		first := true
		for {
			select {
			case <-s.stop:
				return
			case <-v.doneChan:
				return
			case p := <-s.frames:
				if skipped := skipTalkBacklog(s.frames, &p, dur); skipped > 0 {
					fmt.Printf("[%s] [TALK] skipped %d ms of voice that arrived late (the upload from the browser paused), so Talk stays live\n",
						v.model, (time.Duration(skipped) * dur).Milliseconds())
					first = true
				}
				now := time.Now()
				if wait := next.Sub(now); wait > 0 {
					timer := time.NewTimer(wait)
					select {
					case <-timer.C:
					case <-s.stop:
						timer.Stop()
						return
					case <-v.doneChan:
						timer.Stop()
						return
					}
				} else if -wait > 300*time.Millisecond {
					next = now // after a pause, start a new talkspurt
					first = true
				}
				select {
				case <-s.stop:
					return
				case <-v.doneChan:
					return
				default:
				}
				pdur := dur
				if p.samples != 0 && p.samples != f.samples {
					pdur = time.Duration(float64(time.Second) * float64(p.samples) / float64(rate))
				}
				if first {
					v.skipTalkClock(time.Now(), step, dur)
				}
				if err := v.writeTalkPacket(f, p.payload, step, first || f.markerEach); err != nil && first {
					fmt.Printf("[%s] [TALK] audio track write failed: %v\n", v.model, err)
				}
				first = false
				next = next.Add(pdur)
				v.TouchAudioUp()
			}
		}
	}()

	// Progress in the log while talking: packets sent, how many the
	// connection accepted, and any RTCP the camera sends about our audio.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		lastF, lastR, lastD := v.talkFramesSent.Load(), v.audioRTCP.Load(), v.talkDelivered.Load()
		for {
			select {
			case <-s.stop:
				return
			case <-v.doneChan:
				return
			case <-t.C:
				answered, ret, f, b, r := v.TalkDiagnostics()
				d := v.talkDelivered.Load()
				fmt.Printf("[%s] [TALK] last 5 s: %d audio packets sent, %d accepted by the connection (total %d, %d bytes); camera RTCP about our audio: %d new (total %d); camera answered talk request: %v (ret %d)\n",
					v.model, f-lastF, d-lastD, f, b, r-lastR, r, answered, ret)
				lastF, lastR, lastD = f, r, d
			}
		}
	}()
	return nil
}

// Talk plays voice in real time, so voice queued for sending is delay the
// camera's speaker never catches up on. When the browser's upload pauses and
// then delivers the backlog at once, more than talkMaxBacklog queues up; the
// oldest is skipped, down to talkKeepBacklog. (Without this, every pause
// stayed as extra delay until Talk was stopped.)
const (
	talkMaxBacklog  = 400 * time.Millisecond
	talkKeepBacklog = 120 * time.Millisecond
)

// skipTalkBacklog replaces *p with newer packets while too much voice is
// queued behind it, and returns how many packets were skipped.
func skipTalkBacklog(frames chan talkPacket, p *talkPacket, dur time.Duration) int {
	if dur <= 0 || time.Duration(len(frames))*dur <= talkMaxBacklog {
		return 0
	}
	skipped := 0
	for time.Duration(len(frames))*dur > talkKeepBacklog {
		select {
		case *p = <-frames:
			skipped++
		default:
			return skipped
		}
	}
	return skipped
}

// readADTSPackets splits FFmpeg's ADTS output into frames and packs them.
func readADTSPackets(stdout io.Reader, s *talkSession, f TalkFormat) {
	br := bufio.NewReaderSize(stdout, 16*1024)
	hdr := make([]byte, 7)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			return
		}
		if hdr[0] != 0xFF || hdr[1]&0xF0 != 0xF0 {
			continue // resync
		}
		flen := (int(hdr[3]&0x03) << 11) | (int(hdr[4]) << 3) | (int(hdr[5]&0xE0) >> 5)
		if flen < 7 || flen > 8191 {
			continue
		}
		frame := make([]byte, flen)
		copy(frame, hdr)
		if _, err := io.ReadFull(br, frame[7:]); err != nil {
			return
		}
		select {
		case s.frames <- talkPacket{payload: packAAC(frame, f.packing), samples: f.samples}:
		default: // drop if the pacer is far behind
		}
	}
}

// readOpusPackets takes the Opus packets out of FFmpeg's Ogg output.
func readOpusPackets(stdout io.Reader, s *talkSession, f TalkFormat) {
	o := &oggPacketReader{r: bufio.NewReaderSize(stdout, 16*1024)}
	for {
		p, err := o.Next()
		if err != nil {
			return
		}
		if isOpusHeader(p) || len(p) == 0 {
			continue
		}
		select {
		case s.frames <- talkPacket{payload: p, samples: f.samples}:
		default:
		}
	}
}

// skipTalkClock moves the talk RTP clock over a pause, so a new talkspurt's
// timestamps follow real time (as RFC 3550 has it) instead of continuing
// where the last talkspurt ended.
func (v *Viewer) skipTalkClock(now time.Time, step uint32, dur time.Duration) {
	v.rtpMu.Lock()
	last := v.talkLastPkt
	v.rtpMu.Unlock()
	if last.IsZero() || dur <= 0 {
		return
	}
	gap := now.Sub(last) - dur
	if gap <= 0 {
		return
	}
	if ticks := uint64(gap) * uint64(step) / uint64(dur); ticks > 0 && ticks < 1<<31 {
		v.NextTimestamp(uint32(ticks))
	}
}

func (v *Viewer) writeTalkPacket(f TalkFormat, payload []byte, step uint32, marker bool) error {
	v.rtpMu.Lock()
	v.talkLastPkt = time.Now()
	v.rtpMu.Unlock()
	v.talkFramesSent.Add(1)
	v.talkBytesSent.Add(int64(len(payload)))
	if v.talkWriteHook != nil {
		return v.talkWriteHook(payload, step, marker)
	}
	if v.audioTrack == nil {
		return fmt.Errorf("audio track not initialized")
	}
	n, err := v.audioTrack.writeWithPT(rtp.Header{
		SequenceNumber: v.NextSeq(),
		Timestamp:      v.NextTimestamp(step),
		Marker:         marker,
	}, payload, f.pt)
	if err == nil && n > 0 {
		v.talkDelivered.Add(1)
	}
	return err
}

// WriteTalkPCM feeds 16 kHz s16le mono PCM to the talkback encoder.
func (v *Viewer) WriteTalkPCM(pcm []byte) error {
	v.talkMu.Lock()
	s := v.talkSess
	v.talkMu.Unlock()
	if s == nil {
		return fmt.Errorf("talkback encoder not running")
	}
	v.TouchAudioUp()
	_, err := s.stdin.Write(pcm)
	return err
}

// StopTalkAudio stops the talkback encoder, if running.
func (v *Viewer) StopTalkAudio() {
	v.talkMu.Lock()
	s := v.talkSess
	v.talkSess = nil
	v.talkMu.Unlock()
	if s == nil {
		return
	}
	s.once.Do(func() {
		close(s.stop)
		_ = s.stdin.Close()
		if s.cmd != nil {
			go func() {
				time.Sleep(time.Second)
				if s.cmd.Process != nil {
					_ = s.cmd.Process.Kill()
				}
			}()
		}
		fmt.Printf("[%s] [TALK] encoder stopped\n", v.model)
	})
}
