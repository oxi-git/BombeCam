# BombeCam refactor — decision log

Baseline: clean tree, one commit `ff7b59b`. 4 pre-existing test failures are environmental
(2 × pkg/bridge need FFmpeg; 2 × tests/e2e need root). I treat those as out of scope and use
`go build ./...` + `go test ./...` for the packages I touch as my regression gate, except the
4 known-environmental ones.

Feature chosen by the user: **local recording** (MediaMTX record + a Recordings page). Not
feasible to validate against hardware here; I implement it so it is covered by unit tests for
config generation and the helper logic, and document the residual risk.

## Constraints that bound this session's refactor

The codebase is a real, working gateway for vendor cameras. Many findings are structural and
can only be validated against a camera or with root/FFmpeg. Splitting `pkg/bridge` into
subpackages, extracting Session interfaces, or re-architecting MediaMTX wiring would change
the untested-but-working camera path with no way to prove it here. So this session:

- DO: remove surplus/dead code that the unit tests still pass without.
- DO: collapse duplicated logic that is provably identical and not a workaround.
- DO: comments cleanup (remove comments that only restate the implementation) and a few test
  cleanups where a test obviously cannot fail.
- DON'T: split packages or change the live camera/media/control protocol path.

Everything not done here but worth doing is logged below so a future session with hardware can
finish it.

## What I fixed now

(1) Local recording (done + committed a2c12a1)
- Problem: BombeCam has no way to keep video. The only `record`/`playback` references are
  MediaMTX auth permissions; the generated config has no `record` block and no recordings
  directory; the web page has no Recordings tab.
- Fix (done + tested):
  - `internal/mediamtx/config.go`: added `Config.RecordDir`. When set, `GenerateConfig` emits a
    per-path `record` block (recordPath `%path/%Y-%m-%d_%H-%M-%S`, format fmp4, 1s segments)
    under `paths.all_others`. When empty (default), the output stays byte-identical to
    deploy/mediamtx.yml — confirmed by `TestDeployConfigMatchesGenerated`.
  - New test `TestGenerateConfig_Recording` (config_test.go) covers both cases.
  - `main.go`: new `-recordings-dir` flag (env `BOMBECAM_RECORDINGS_DIR`), wired into
    `mtxCfg.RecordDir`, and `setRecordingsRoot(*recordingsDirFlag)` installed.
  - `recordings.go` (new): keeps the recording root, lists recorded segments
    (`listRecordings`), and serves `GET /api/v1/recordings` (list) and
    `GET /api/v1/recordings/download?path=...` (one segment, path-traversal-safe `inside`).
  - `api_handlers.go`: registered the two recordings routes in `SetupAPIMux`.
  - `recordings_test.go` (new): exercises list, download and the path-escape refusal.
  - `web/index.html` + `web/app.js`: a Recordings card on the Streams & ports tab lists
    recorded segments (per stream) with a working Download link; it loads when the tab opens
    and on Refresh. Applies the API's "recording off" explanation when the flag isn't set.
  - `README.md`: documented `-recordings-dir` / `BOMBECAM_RECORDINGS_DIR`.
- Known limitation (documented here and in the code): recording only captures while a stream
  is published (i.e. a live view is active). Continuous recording needs keep-alive publishers,
  which is a separate design decision (see "Follow-ups").

(2) Safe cleanup (done + committed 8a1c072)
- Removed a dead MediaMTX stop defer in main.go (else branch never assigns mtxSupervisor; the
  supervisor is stopped in the normal shutdown path at main.go:760).
- Tests for the recording feature (recordings_test.go, TestGenerateConfig_Recording) verified
  with go build ./... and go test on the touched packages; only the 2 pre-existing
  FFmpeg-required pkg/bridge tests fail, identical to baseline.

## Follow-ups the user chose — honest status

The user chose: (A) safe comment/test cleanup, (B) modularize pkg/bridge, (C) 24/7 recording.

- (A) Done for the code touched: dead-code removal and a test for the feature. A full
  comment-cleanup sweep across all 57.8k LOC is a large, separate pass; I kept removals
  confined to changes I could verify (build + tests green).
- (B) pkg/bridge modularization (MODULARIZATION_PLAN steps 6-9) is NOT done in this session.
  It is a structural refactor of the working camera/media/control path. It cannot be validated
  here: no camera, no FFmpeg, no root (two pkg/bridge tests need FFmpeg and fail before/after
  on this machine). Doing it blind risks silently breaking the one thing BombeCam does.
  Requires hardware validation. The full audit findings are in this log / the subagent reports.
- (C) 24/7 recording (keep all publishers up so cameras record continuously) is NOT done.
  It is a behavior change to the stream lifecycle (on-demand today). It cannot be validated
  without a camera. Implementable after (B) and hardware.

So: I delivered the recording feature, the safe cleanup, and a complete audit + decision log.
The two deeper items (pkg/bridge split, 24/7 all-day recording) are deliberately deferred
because they change untestable-but-working behavior and need a camera to verify.

## Problems found (full audit) — deferred, worth fixing with hardware

### pkg/bridge — package owns cloud HTTP + WS + control + media + MQTT all together
- cloud.go is a grab-bag: signing, login, device list, WebRTC session setup, region, timezone.
- Duplicated ADTS framing across adts.go / talk.go / talk_formats.go / rtsp_publisher.go / media.go
- `Viewer` (media.go) is a WebRTC endpoint + vendor client + publisher + owns the Signaling socket
  + can arm talk directly; media depends on concrete `*SignalingControlChannel` via type assert (media.go:1259).
- FU (H.264 fragments) path is re-implemented in media.go:497-527 and nal_assembler.go:26-57.
- Signaling protocol names exist twice (CmdX and cmdX).
- PTZ preemption/cancel logic duplicated in signaling_channel.go and mqtt_channel.go.
- MQTT transport is experimental but non-trivial duplicate surface (mosquitto_bridge, shadow_shim, mqtt_channel).

Recommendation: Step 7/9 of MODULARIZATION_PLAN.md (Session interface, split pkg/bridge) — needs hardware.

Postpone all of the above; the audit listed ~120 more specific findings under these headings
(hidden links, needless links, surplus, special cases) which are captured in this session's
subagent reports. They are deliberately not all fixed now to avoid destabilising working code
without hardware.

## Feature requirements captured for follow-up
- Continuous all-day recording requires keeping every camera stream published; today streams are
  on-demand. Decide whether to add an "always record" mode that keeps publishers up.
## Home Assistant alignment — done
- Verified (earlier) that all four HA endpoints and every parsed field matched the
  `ha-bombecam` integration; its 19 pytest tests passed in Docker.
- Added automated contract tests on BOTH sides so a rename can't silently break the pairing:
  - BombeCam (Go): TestHomeAssistantContractPinsAPI (payload fields HA reads),
    TestShadowContractPinsAPI (state.reported attributes), TestHomeAssistantContractRecording
    (recording_available / latest_recording_url present and the download URL is exact).
  - ha-bombecam (Python): tests/test_contract.py exercises the real BombeCamClient against the
    exact payloads BombeCam produces (no client mock). Full HA suite now 22 tests.
- Per-camera recordings surfaced in HA (design B): the gateway's
  /api/v1/integrations/homeassistant now reports recording_available + latest_recording_url per
  camera, and ha-bombecam added a per-camera `binary_sensor.xxx_recording` exposing
  latest_recording_url. All green.
- Deploy from the fork: docs/technical/HA_DEPLOY_FORK_IMAGE.md has build + compose steps
  (including enabling MediaMTX recording in the Docker deploy, which does NOT use the gateway's
  generated config because BOMBECAM_NO_MEDIAMTX_SUPERVISOR=true).
