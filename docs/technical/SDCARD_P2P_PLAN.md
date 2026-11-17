# SD card over the control channel — implementation plan

Status: **phase 1 confirmed against a real WS03 on 2026-11-17**. Phase 2
(playback bytes) waits for a real reply.

## Confirmed on hardware

Card `6b13a142f1d0b6d42411293f892babfe` (WS03_A10S14A5, "Ingresso"):

```json
GET /sdcard            {"raw":{"Status":0,"TotalSizeK":31251968,"UsedSizeK":30285312},
                        "status":"0","total_kb":31251968,"used_kb":30285312}
GET /sdcard/recordings {"cur_days":"14,15,16,17","cur_month":"11","prev_month":"10",
                        "raw":{"curMonDays":"14,15,16,17","curMonth":"11","preMonDays":"","preMonth":"10"}}
```

Both keys the plan assumed are answered by this firmware: `RecordDays` needs no
fallback to `service.GetLocalVideoTime`. `preMonDays` came back empty, consistent
with a card holding only the current month.

### The reply carries no uuid

The WS03 answers `atr.get` with `uuid: ""`. Matching on it dropped every reply and
made all SD requests time out with `context canceled` while the camera was
answering correctly. Correlation is now the message id (already unique and bound to
one connection when sent) plus the connection it arrives on; a non-empty uuid that
names another camera is still rejected. `TestSDCardQueryAcceptsEmptyReplyUUID`
pins this.

Still unconfirmed: what `Status: 0` means beyond "not an error", and whether a
second card generation reports the same key names.

## What phase 1 added

- `Signaling.QueryCommand(ctx, camera, method, data)` (`pkg/bridge/signaling.go`) — a
  correlated request/reply on the existing socket, its own `cmdQueries` map, and a
  matcher that binds message id, socket, camera and method (only when the reply names
  a camera; this firmware sends none). `failQueriesLocked` now
  releases command waiters too, so a dropped socket cannot hang a call.
- `SignalingControlChannel.GetSDCardInfo` / `GetRecordDays`
  (`pkg/bridge/signaling_channel.go`) — extra methods on the concrete type, not on
  `ControlChannel`.
- Gateway `GET /api/v1/cameras/{id}/sdcard` and `/sdcard/recordings` return the mapped
  reply; 501 `sdcard_unavailable` when the channel has no such capability, 502 when the
  camera does not answer.
- The Streams & ports card shows card usage and the days that have recordings above the
  cloud list.
- Tests: `pkg/bridge/sdcard_signaling_test.go` covers a correlated reply, a wrong id, a
  foreign camera, a wrong method and a socket drop; `cmd/bombecam-gateway/sdcard_test.go`
  covers 501 and the field mapping.

## Corrected finding

An earlier version of this document said the SD card needed the ThingClips P2P
protocol implemented from scratch. That was wrong, and the evidence that
corrected it is below.

The Osaio app has **two** independent paths to a camera:

- **ThingClips P2P** (`com.thingclips.smart.camera.*`) — implemented in
  `libthingcamera.so`, which is **not in `base.apk`** (it is a split APK:
  `lib/**` has zero entries). Using it would mean reverse-engineering native code.
- **YRCX cloud WebSocket** (`com.yrcx.yripc.websocket`) — all message names are
  plain Java strings. **This is the channel BombeCam already has open**
  (`pkg/bridge/signaling.go`).

The SD card is readable on the second one.

## Evidence (all from base.apk, sha256 e015dc96…, JADX 1.5.1)

| Purpose | Message | Payload | Reply | Evidence |
|---|---|---|---|---|
| SD card state | `atr.get` | `["SdCardInfo"]` (key names as the payload) | `SdCardInfo{Status, TotalSizeK, UsedSizeK}` | key `SdCardInfo` in `yripc/middleservice/helper/YRIPCHelper.java:102`; inner keys `xplayer/helper/PlayerHelper.java:222`; readers `xplayer/.../AnimalModelKt.java:17,25,33` |
| Record days of a month | `atr.get` | `["RecordDays"]` | `curMonth`, `curMonDays`, `preMonth`, `preMonDays` (comma-separated day lists) | `xplayer/websocket/WebSocketHandler.java:365`; parse in `PlayerHelper.K0()` |
| Recordings of a day | `service.GetLocalVideoTime` | `{Start, Duration, Order, Limited}` | `response.GetLocalVideoTimeResp` → `data:{Ret, ChunkStart[], ChunkLength[], HasMore}` | `yripc/websocket/WebSocketHandlerKt.java:16,40`; payload `xplayer/.../YRPlayerComponentViewModel.java:618-623`; parse `:641-646` |
| Playback seek | `service.Seek` | session-based | `response.SeekResp` | `WebSocketHandlerKt.java:25,52` |
| Stream switch | `service.Switch` | `{SessionId, Action, Quality, Timestamp, call_id}` | `response.SwitchResp` | `WebSocketHandlerKt.java:31,58` — already used by the gateway |
| Format card | `service.SDCardFormat` | set on method list | — | `YRIPCHelper.java:32` |

Method names are `"service."+cmd` and `"response."+cmd` (`yripc/websocket/WebSocketApi.java:83,89`).

## What BombeCam has (verified)

- `pkg/bridge/signaling.go` — one open WebSocket per camera: `Send(method, data)`,
  receive loop, reconnect, keepalive. Correlated replies exist only for `atr.get`
  (`QueryAttributes` + `attributeQuery`, keyed by message id, `signaling.go:382-460`,
  matcher at `474-511`).
- `pkg/bridge/signaling_channel.go` — `ControlChannel`: `atr.get`/`atr.set` for IR,
  LED, light, motion, sound. `GetAttributes` is a concrete method, not on the
  interface — the precedent for adding a capability without widening it.
- `cmd/bombecam-gateway/stream_manager.go:1421` — `sig.SetOnMsg` per camera.
- `cmd/bombecam-gateway/api_handlers.go` — `/sdcard` and `/sdcard/recordings` answer
  501 today.

## Phase 1 — status and record list (no new socket)

1. `pkg/bridge`: add a correlated query that sends `atr.get` with
   `{"cmd":["SdCardInfo"]}` / `{"cmd":["RecordDays"]}` and returns the reply body.
   Model it on `QueryAttributes`, but its own matcher: the reply to `atr.get` with
   a `cmd` payload does not look like the attribute reply (map of values).
   Keep `Send`'s `atr.set` epoch invalidation out of this path.
2. Surface it as `GetSDCardInfo` / `GetRecordDays` on the concrete channel
   (type assertion, like `GetAttributes`), **not** on `ControlChannel`: widening the
   interface would force `mqtt_channel.go`, `tests/e2e/harness.go`,
   `mockControlChannel` and the readback assertions to grow a transport they cannot
   support.
3. Gateway: `GET /api/v1/cameras/{id}/sdcard` returns the card state;
   `GET /sdcard/recordings?month=YYYY-MM` returns the days. Keep the honest error
   when the camera does not answer.
4. Tests: fake WebSocket in `pkg/bridge/signaling_test.go` style (`newWSTestServer`,
   `drain`, `waitFor`); gateway test replaces the 501 assertion in
   `sdcard_test.go`.

## Phase 2 — play one recording (after a real reply)

Needs the recording list from Phase 1 to know the shape, then `service.Switch`/
`Seek` on the existing session, and the bytes from the media channel. Do not
design this before Phase 1 shows real data.

## Constraints

- Never invent a message name; each one cites the file:line above.
- Verify on the HA box with the real camera after each step.
- The 501 stays until Phase 1 answers with real data.

## Correction after the first live attempt

The first version sent `atr.get` with `{"cmd": ["SdCardInfo"]}`. The camera
answered `data=<nil>`: that envelope belongs to the ThingClips channel, not to
YRCX. `WebSocketApi.getAtr` passes the `cmd` object through untouched, and
`WebSocketHandler.getAllSettingAttribute()` passes a plain list of key names with
`SdCardInfo` among them. The payload is the list itself.

Evidence: `websocket/WebSocketApi.java:116` (passes `cmd` to `sendData`),
`websocket/WebSocketHandler.java` (`getAllSettingAttribute`), observed reply
`[ws recv] method=atr.get data=<nil>` from the WS03.
