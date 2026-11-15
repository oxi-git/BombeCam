package bridge

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// sdCardServer answers atr.get "cmd" queries through answer, which may edit
// the echoed message and returns the reply body (nil to stay silent).
func sdCardServer(t *testing.T, answer func(m *wsMsg, cmd string) any) *wsTestServer {
	t.Helper()
	return newWSTestServer(t, func(c *websocket.Conn) {
		for {
			var m wsMsg
			if c.ReadJSON(&m) != nil {
				return
			}
			if m.Method != "atr.get" {
				continue
			}
			// The app sends the key names themselves as the payload.
			keys, _ := m.Data.([]any)
			if len(keys) == 0 {
				continue
			}
			cmd, _ := keys[0].(string)
			body := answer(&m, cmd)
			if body == nil {
				continue
			}
			m.Data = body
			if c.WriteJSON(m) != nil {
				return
			}
		}
	})
}

func sdCardSignaling(t *testing.T, ts *wsTestServer) *Signaling {
	t.Helper()
	sig, err := connectHeader(ts.url(), http.Header{}, readDeadline)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sig.Close() })
	sig.BindCamera("fixture", "WS03")
	return sig
}

func TestSDCardInfoResolvesOnCorrelatedReply(t *testing.T) {
	state := map[string]any{"Status": 1.0, "TotalSizeK": 62000000.0, "UsedSizeK": 512.0}
	ts := sdCardServer(t, func(m *wsMsg, cmd string) any {
		if cmd != "SdCardInfo" {
			t.Errorf("cmd = %q, want SdCardInfo", cmd)
		}
		return map[string]any{"SdCardInfo": state}
	})
	cc := NewSignalingControlChannel(sdCardSignaling(t, ts))
	body, err := cc.GetSDCardInfo(context.Background(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := body["SdCardInfo"].(map[string]any)
	for k, want := range state {
		if info[k] != want {
			t.Fatalf("%s = %v, want %v (reply %v)", k, info[k], want, body)
		}
	}
}

func TestRecordDaysResolvesOnCorrelatedReply(t *testing.T) {
	ts := sdCardServer(t, func(m *wsMsg, cmd string) any {
		if cmd != "RecordDays" {
			t.Errorf("cmd = %q, want RecordDays", cmd)
		}
		return map[string]any{"curMonth": "2026-11", "curMonDays": "3,4,10", "preMonth": "2026-10", "preMonDays": "1,2"}
	})
	cc := NewSignalingControlChannel(sdCardSignaling(t, ts))
	body, err := cc.GetRecordDays(context.Background(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if body["curMonDays"] != "3,4,10" || body["preMonDays"] != "1,2" {
		t.Fatalf("record days = %v", body)
	}
}

// The camera answers an atr.get read with atr.post, as it does on the real
// WS03 firmware.
func TestSDCardInfoResolvesOnAtrPostReply(t *testing.T) {
	ts := newWSTestServer(t, func(c *websocket.Conn) {
		for {
			var m wsMsg
			if c.ReadJSON(&m) != nil {
				return
			}
			if m.Method != "atr.get" {
				continue
			}
			m.Method = "atr.post"
			m.Data = map[string]any{"SdCardInfo": map[string]any{
				"Status": 0.0, "TotalSizeK": 31251968.0, "UsedSizeK": 11638464.0,
			}}
			if c.WriteJSON(m) != nil {
				return
			}
		}
	})
	cc := NewSignalingControlChannel(sdCardSignaling(t, ts))
	body, err := cc.GetSDCardInfo(context.Background(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := body["SdCardInfo"].(map[string]any)
	if info["TotalSizeK"] != 31251968.0 || info["UsedSizeK"] != 11638464.0 {
		t.Fatalf("reply = %v", body)
	}
}

// A reply with another message id, for another camera or under another method
// must not resolve the query.
func TestSDCardQueryRejectsUncorrelatedReply(t *testing.T) {
	for _, mismatch := range []string{"wrong-id", "other-camera", "wrong-method"} {
		t.Run(mismatch, func(t *testing.T) {
			ts := sdCardServer(t, func(m *wsMsg, cmd string) any {
				switch mismatch {
				case "wrong-id":
					m.MsgID = "old"
				case "other-camera":
					m.UUID = "other"
				case "wrong-method":
					m.Method = "atr.set"
				}
				return map[string]any{"SdCardInfo": map[string]any{"Status": 1.0}}
			})
			sig := sdCardSignaling(t, ts)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if body, err := sig.QueryCommand(ctx, "fixture", "atr.get", map[string]any{"cmd": []string{"SdCardInfo"}}); err == nil {
				t.Fatalf("uncorrelated reply returned %v", body)
			}
		})
	}
}

// A lost socket fails the pending call instead of leaving it to hang.
func TestSDCardQueryFailsWhenSocketDrops(t *testing.T) {
	received := make(chan struct{}, 1)
	ts := newWSTestServer(t, func(c *websocket.Conn) {
		var m wsMsg
		if c.ReadJSON(&m) != nil {
			return
		}
		received <- struct{}{}
		_ = c.Close()
		drain(c)
	})
	cc := NewSignalingControlChannel(sdCardSignaling(t, ts))
	done := make(chan error, 1)
	go func() { _, err := cc.GetSDCardInfo(context.Background(), "fixture"); done <- err }()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("query not sent")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("lost connection returned success")
		}
	case <-time.After(2 * time.Second): // shorter than QueryCommand's 5 s
		t.Fatal("query did not terminate on socket loss")
	}
}
