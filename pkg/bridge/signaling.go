package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// protocol constants (from WebSocketApiKt)
const (
	CmdSdpOffer     = "service.SdpOffer"
	CmdIceCandidate = "service.IceCandidate"
	CmdSwitch       = "service.Switch"
	EvtSdpAnswer1   = "response.SdpAnswer"
	EvtSdpAnswer2   = "event.SdpAnswer"
	EvtIce1         = "event.IceCandidate"
	EvtIce2         = "response.IceCandidate"

	cmdSdpOffer     = CmdSdpOffer
	cmdIceCandidate = CmdIceCandidate
	cmdSwitch       = CmdSwitch
	evtSdpAnswer1   = EvtSdpAnswer1
	evtSdpAnswer2   = EvtSdpAnswer2
	evtIce1         = EvtIce1
	evtIce2         = EvtIce2
)

type wsMsg struct {
	Method      string `json:"method"`
	MsgID       string `json:"msg_id"`
	Ver         string `json:"ver"`
	Origin      int    `json:"origin"`
	Time        int64  `json:"time"`
	UUID        string `json:"uuid"`
	DeviceModel string `json:"device_model"`
	Data        any    `json:"data"`
}

type Signaling struct {
	conn           *websocket.Conn
	uuid           string
	model          string
	seq            int
	mu             sync.Mutex
	OnMsg          func(method string, data any)
	queries        map[string]*attributeQuery
	cmdQueries     map[string]*cmdQuery
	observations   map[string]attributeObservation
	attributeEpoch map[string]uint64

	// Reconnection state. The vendor WebSocket carries every camera control
	// command, so a dropped socket is redialled unless Close() was called.
	wsURL   string
	header  http.Header
	closed  bool
	down    bool
	gotPong bool
	// kick wakes the reconnect loop early. A control sent while the socket
	// is down retries at once instead of waiting out the backoff.
	kick chan struct{}
	// readTimeout is readDeadline, shorter only in tests. Set once at
	// construction.
	readTimeout time.Duration
}

// Reconnect and dead-socket tuning. The stream session sends a keep-alive
// (service.Switch) every 20 s and the server answers it, so a healthy socket
// never goes readDeadline without a message.
const (
	readDeadline      = 75 * time.Second
	reconnectMaxDelay = 10 * time.Second
)

// ErrReconnecting is returned by Send while the socket is being redialled.
var ErrReconnecting = fmt.Errorf("controls are reconnecting to Osaio's server; try again in a few seconds")

func msgID(seq int) string {
	return fmt.Sprintf("ad-%015d-%d", rand.Int63n(1e15), seq)
}

func dialSignaling(wsURL string, h http.Header) (*websocket.Conn, error) {
	d := websocket.Dialer{HandshakeTimeout: 15 * time.Second, Proxy: http.ProxyFromEnvironment}
	conn, resp, err := d.Dial(wsURL, h)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		return nil, fmt.Errorf("ws dial (http %d): %v", code, err)
	}
	return conn, nil
}

func Connect(wsURL, apiToken, uid, phoneCode string) (*Signaling, error) {
	appID, err := AppID()
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("api_token", apiToken)
	h.Set("phone_code", phoneCode)
	h.Set("appid", appID)
	h.Set("uid", uid)
	h.Set("User-Agent", userAgent)
	return connectHeader(wsURL, h, readDeadline)
}

func connectHeader(wsURL string, h http.Header, readTimeout time.Duration) (*Signaling, error) {
	conn, err := dialSignaling(wsURL, h)
	if err != nil {
		return nil, err
	}
	s := &Signaling{conn: conn, wsURL: wsURL, header: h, kick: make(chan struct{}, 1), readTimeout: readTimeout}
	s.installPongHandler(conn)
	go s.readLoop(conn)
	go s.keepalive()
	fmt.Println("[ws] connected to", wsURL)
	return s, nil
}

func (s *Signaling) installPongHandler(conn *websocket.Conn) {
	conn.SetPongHandler(func(string) error {
		s.mu.Lock()
		s.gotPong = true
		s.mu.Unlock()
		return conn.SetReadDeadline(time.Now().Add(s.readTimeoutOrDefault()))
	})
}

func (s *Signaling) readTimeoutOrDefault() time.Duration {
	if s.readTimeout > 0 {
		return s.readTimeout
	}
	return readDeadline
}

// keepalive pings the server so idle connections are not dropped by NATs,
// and (once the server has proven it answers pings) detects dead sockets.
func (s *Signaling) keepalive() {
	t := time.NewTicker(25 * time.Second)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		conn, down := s.conn, s.down
		s.mu.Unlock()
		if conn == nil || down {
			continue
		}
		_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
	}
}

func (s *Signaling) readLoop(conn *websocket.Conn) {
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			s.mu.Lock()
			s.failQueriesLocked(conn, fmt.Errorf("camera readback connection lost: %w", err))
			closed := s.closed
			if !closed && s.conn == conn {
				s.down = true
			}
			s.mu.Unlock()
			if closed {
				return
			}
			fmt.Println("[ws] connection lost:", err, "- reconnecting")
			s.reconnect()
			return
		}
		// Any message proves the socket is alive. Once traffic has flowed,
		// silence longer than readDeadline means a dead socket (for example
		// the network dropped without a TCP reset), so reconnect instead of
		// waiting for the operating system's TCP timeout.
		_ = conn.SetReadDeadline(time.Now().Add(s.readTimeoutOrDefault()))
		var m wsMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		// Command replies are matched before anything else: the gateway's
		// OnMsg callback blocks and can switch the camera, which would leave
		// the reply unrecognised.
		s.mu.Lock()
		s.answerCmdLocked(conn, m)
		s.mu.Unlock()
		if m.Method != "event.Heartbeat" {
			summary := fmt.Sprintf("%+v", m.Data)
			if strings.Contains(m.Method, "Sdp") || len(summary) > 300 {
				summary = summary[:min(len(summary), 120)] + "..."
			}
			fmt.Printf("[ws recv] method=%s data=%s\n", m.Method, summary)
		}
		s.mu.Lock()
		s.deliverAttributesLocked(conn, m)
		cb := s.OnMsg
		s.mu.Unlock()
		if cb != nil {
			cb(m.Method, m.Data)
		}
	}
}

// currentHeader is the connect header with the app ID in use now, so an app
// ID changed on the web page reaches reconnects too. Without a usable app ID
// it fails and nothing is sent.
func (s *Signaling) currentHeader() (http.Header, error) {
	id, err := AppID()
	if err != nil {
		return nil, err
	}
	h := s.header.Clone()
	h.Set("appid", id)
	return h, nil
}

// reconnect redials the vendor WebSocket with backoff (1 s doubling to
// reconnectMaxDelay) until it succeeds or the Signaling is closed. A control
// sent meanwhile wakes it for an immediate attempt.
func (s *Signaling) reconnect() {
	delay := time.Second
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		h, err := s.currentHeader()
		var conn *websocket.Conn
		if err == nil {
			conn, err = dialSignaling(s.wsURL, h)
		}
		if err == nil {
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				_ = conn.Close()
				return
			}
			old := s.conn
			s.conn = conn
			s.down = false
			s.gotPong = false
			s.mu.Unlock()
			if old != nil {
				_ = old.Close()
			}
			s.installPongHandler(conn)
			fmt.Println("[ws] reconnected to", s.wsURL)
			go s.readLoop(conn)
			return
		}
		fmt.Printf("[ws] reconnect failed: %v (retrying in %v, sooner if a control is sent)\n", err, delay)
		s.waitRetry(delay)
		delay *= 2
		if delay > reconnectMaxDelay {
			delay = reconnectMaxDelay
		}
	}
}

// waitRetry sleeps for d, or less if a control was sent since the last
// attempt. It always waits at least a second so repeated clicks can't hammer
// the server.
func (s *Signaling) waitRetry(d time.Duration) {
	const minGap = time.Second
	kick := s.kickCh()
	first := d
	if first > minGap {
		first = minGap
	}
	time.Sleep(first)
	if d <= minGap {
		return
	}
	t := time.NewTimer(d - minGap)
	defer t.Stop()
	select {
	case <-t.C:
	case <-kick:
		fmt.Println("[ws] control requested; retrying now")
	}
}

func (s *Signaling) kickCh() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kick == nil {
		s.kick = make(chan struct{}, 1)
	}
	return s.kick
}

// SetOnMsg installs the incoming-message callback.
func (s *Signaling) SetOnMsg(cb func(method string, data any)) {
	s.mu.Lock()
	s.OnMsg = cb
	s.mu.Unlock()
}

// Connected reports whether the WebSocket is currently usable.
func (s *Signaling) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil && !s.closed && !s.down
}

func (s *Signaling) Send(method string, data any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if method == "atr.set" {
		if values, ok := data.(map[string]any); ok {
			if s.attributeEpoch == nil {
				s.attributeEpoch = make(map[string]uint64)
			}
			for key := range values {
				delete(s.observations, key)
				s.attributeEpoch[key]++
			}
		}
	}
	s.seq++
	return s.sendLocked(method, data, msgID(s.seq), time.Now().Add(10*time.Second))
}

// isAttributePoll reports the periodic atr.get for the IR/LED keys, the one
// message that repeats every few seconds; logging it would bury everything else.
func isAttributePoll(data any) bool {
	keys, ok := data.([]string)
	if !ok {
		return false
	}
	for _, k := range keys {
		if k == "SdCardInfo" || k == "RecordDays" {
			return false
		}
	}
	return true
}

func (s *Signaling) sendLocked(method string, data any, id string, deadline time.Time) error {
	if s.conn == nil {
		return fmt.Errorf("signaling not connected")
	}
	if s.closed {
		return fmt.Errorf("signaling closed")
	}
	if s.down {
		if s.kick == nil {
			s.kick = make(chan struct{}, 1)
		}
		select {
		case s.kick <- struct{}{}:
		default:
		}
		return ErrReconnecting
	}
	m := wsMsg{Method: method, MsgID: id, Ver: "1.0", Origin: 1,
		Time: time.Now().Unix(), UUID: s.uuid, DeviceModel: s.model, Data: data}
	raw, _ := json.Marshal(m)
	if method != "atr.get" || !isAttributePoll(data) {
		summary := fmt.Sprintf("%+v", data)
		if len(summary) > 200 {
			summary = summary[:200] + "..."
		}
		fmt.Printf("[ws send] method=%s id=%s data=%s\n", method, id, summary)
	}
	_ = s.conn.SetWriteDeadline(deadline)
	if err := s.conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		// A failed write means the socket is gone. Close it so the read loop
		// notices now and starts reconnecting.
		fmt.Printf("[ws] send failed: %v; reconnecting\n", err)
		s.down = true
		_ = s.conn.Close()
		return ErrReconnecting
	}
	return nil
}

func (s *Signaling) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.failQueriesLocked(nil, fmt.Errorf("signaling closed before camera readback"))
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}

func (s *Signaling) send(method string, data any) error {
	return s.Send(method, data)
}

type attributeResult struct {
	values map[string]any
	err    error
}

type attributeQuery struct {
	camera string
	conn   *websocket.Conn
	keys   []string
	epochs map[string]uint64
	result chan attributeResult
}

type attributeObservation struct {
	value any
	at    time.Time
	conn  *websocket.Conn
}

// FreshAttributes contains only correlated replies on the active connection.
func (s *Signaling) FreshAttributes(camera string, maxAge time.Duration) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]any)
	if s.closed || s.down || s.uuid != camera {
		return out
	}
	for key, o := range s.observations {
		if o.conn == s.conn && time.Since(o.at) <= maxAge {
			out[key] = o.value
		}
	}
	return out
}

func (s *Signaling) BindCamera(camera, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uuid != camera {
		s.observations = nil
		s.failQueriesLocked(nil, fmt.Errorf("camera connection changed"))
	}
	s.uuid, s.model = camera, model
}

// QueryAttributes accepts only the reply to this atr.get on this camera's
// current connection. An atr.set echo, old request, unsolicited report or reply
// without correlation cannot certify a command. Commands still send normally.
func (s *Signaling) QueryAttributes(ctx context.Context, camera string, keys ...string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if camera == "" || s.uuid != camera {
		s.mu.Unlock()
		return nil, fmt.Errorf("camera readback connection is not bound to %s", camera)
	}
	s.seq++
	id := msgID(s.seq)
	q := &attributeQuery{camera: camera, conn: s.conn, keys: keys, result: make(chan attributeResult, 1)}
	q.epochs = make(map[string]uint64)
	for _, key := range keys {
		q.epochs[key] = s.attributeEpoch[key]
	}
	if s.queries == nil {
		s.queries = make(map[string]*attributeQuery)
	}
	s.queries[id] = q
	deadline, _ := ctx.Deadline()
	err := s.sendLocked("atr.get", keys, id, deadline)
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.queries, id); s.mu.Unlock() }()
	if err != nil {
		return nil, err
	}
	select {
	case result := <-q.result:
		return result.values, result.err
	case <-ctx.Done():
		return nil, fmt.Errorf("camera readback unconfirmed: %w", ctx.Err())
	}
}

func (s *Signaling) failQueriesLocked(conn *websocket.Conn, err error) {
	for id, q := range s.queries {
		if conn == nil || q.conn == conn {
			select {
			case q.result <- attributeResult{err: err}:
			default:
			}
			delete(s.queries, id)
		}
	}
	for id, q := range s.cmdQueries {
		if conn == nil || q.conn == conn {
			select {
			case q.result <- cmdResult{err: err}:
			default:
			}
			delete(s.cmdQueries, id)
		}
	}
}

type cmdResult struct {
	data any
	err  error
}

type cmdQuery struct {
	camera string
	method string
	conn   *websocket.Conn
	result chan cmdResult
}

// QueryCommand sends method with data and returns the correlated reply's Data
// as the camera sent it. A reply counts only on this camera's current
// connection and with the message id of this request. Its method must be the
// sent one or what the vendor uses to answer it: a read asked with "atr.get"
// comes back as "atr.post", and a "service." method as "response."+its name
// (WebSocketApi.java:83,89). The message stays available to OnMsg.
func (s *Signaling) QueryCommand(ctx context.Context, camera, method string, data any) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if camera == "" || s.uuid != camera {
		s.mu.Unlock()
		return nil, fmt.Errorf("camera connection is not bound to %s", camera)
	}
	s.seq++
	id := msgID(s.seq)
	q := &cmdQuery{camera: camera, method: method, conn: s.conn, result: make(chan cmdResult, 1)}
	if s.cmdQueries == nil {
		s.cmdQueries = make(map[string]*cmdQuery)
	}
	s.cmdQueries[id] = q
	deadline, _ := ctx.Deadline()
	err := s.sendLocked(method, data, id, deadline)
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.cmdQueries, id); s.mu.Unlock() }()
	if err != nil {
		return nil, err
	}
	select {
	case result := <-q.result:
		return result.data, result.err
	case <-ctx.Done():
		return nil, fmt.Errorf("camera did not answer %s: %w", method, ctx.Err())
	}
}

// replyMethod is the method a reply to this request carries: a service command
// is answered as response.<name> (WebSocketApi.java:83,89 in the Osaio app).
func replyMethod(method string) string {
	if suffix, ok := strings.CutPrefix(method, "service."); ok {
		return "response." + suffix
	}
	return method
}

// repliesTo reports whether got answers a request sent as sent: the same method,
// the vendor's response form, or atr.post for an atr.get, which is how the
// camera answers a read.
func repliesTo(sent, got string) bool {
	if got == sent || got == replyMethod(sent) {
		return true
	}
	return sent == "atr.get" && got == "atr.post"
}

// answerCmdLocked completes the pending command query this message answers.
// It runs before OnMsg, which may block and switch the camera.
func (s *Signaling) answerCmdLocked(conn *websocket.Conn, m wsMsg) {
	q := s.cmdQueries[m.MsgID]
	if q == nil || q.conn != conn || conn != s.conn || m.UUID != q.camera {
		return
	}
	if !repliesTo(q.method, m.Method) {
		return
	}
	select {
	case q.result <- cmdResult{data: m.Data}:
	default:
	}
	if m.Data == nil {
		fmt.Printf("[ws] %s answered nil for %+v\n", m.Method, q.method)
	}
	delete(s.cmdQueries, m.MsgID)
}

func (s *Signaling) deliverAttributesLocked(conn *websocket.Conn, m wsMsg) {
	q := s.queries[m.MsgID]
	if q == nil || q.conn != conn || conn != s.conn || m.UUID != q.camera || m.Method != "atr.get" {
		return
	}
	values, ok := m.Data.(map[string]any)
	if !ok {
		return
	}
	if nested, ok := values["data"].(map[string]any); ok {
		values = nested
	}
	out := make(map[string]any)
	for _, key := range q.keys {
		if v, ok := values[key]; ok && q.epochs[key] == s.attributeEpoch[key] {
			out[key] = v
		}
	}
	if len(out) == 0 {
		return
	}
	if s.observations == nil {
		s.observations = make(map[string]attributeObservation)
	}
	for key, value := range out {
		n, valid := ParseCameraAttributeNumber(value)
		if key != "IrLedMode" && n > 1 {
			valid = false
		}
		if valid {
			s.observations[key] = attributeObservation{value: n, at: time.Now(), conn: conn}
		}
	}
	select {
	case q.result <- attributeResult{values: out}:
	default:
	}
	delete(s.queries, m.MsgID)
}
