# Changelog

## Unreleased

**Block cloud video**
- The allowlist now supports the EU Osaio region as well as the US one: EU cameras keep their control channel (`mqtts-eu.osaio.net`, `mqtts01-eu.osaio.net`) and the stream-setup handshake (`wss-eu.osaio.net`). Choose the region under **Connect router → Advanced → Osaio region**; US stays the default, and changing it reapplies the rules. A camera on the wrong region loses its app controls while blocked.
- `bombecam-net` and `bombecam-verify` take `-region us|eu` (or `BOMBECAM_REGION`); `bombecam-policy` takes `-region`; the Docker `isolation` profile reads `BOMBECAM_REGION`.

## 1.0.0 — first public release

**Gateway**
- Connects to cameras that use the Osaio app (sold by Yoton, GNCC, Wolfang, Surfola and others; developed and tested with the Yoton WS03, other models not yet verified) with an Osaio login and receives their video and sound directly over the local network (WebRTC).
- Publishes each camera through MediaMTX (downloaded and checksum-verified on first run) as RTSP, HLS and WebRTC without re-encoding. Stream addresses use a short camera name (`rtsp://<pc>:8554/front_door`) or the camera ID.
- Cameras from several Osaio logins; add, remove and rename cameras; stop and start all streams.
- Osaio logins form a pool, stored encrypted on this computer and used only to find and reach cameras. Every login is equal: there is no main login, and the first sign-in page is only a quick start that adds the first login. A login can be added, given a new password (checked with Osaio before it is saved, so a typo never interrupts a working login), or removed together with the cameras added through it.
- At start-up every saved login is restored the same way (its saved sign-in, else its saved password), and only the cameras that were added come back; other cameras a login can see stay out until you add them. A login whose password stops working only affects its own cameras. Profiles from earlier builds move their single login into the pool by themselves.
- Reconnects by itself after network drops, sleep, video-server restarts and expired sign-ins.

**Administrator sign-in**
- BombeCam has its own administrator, created on the PC at first start (or from `BOMBECAM_ADMIN_PASSWORD` for a machine used only from other devices). Other devices always sign in; the PC running BombeCam asks too if you choose so when creating it (changeable under Settings → Sign-in settings). Osaio logins no longer sign anyone in to BombeCam.
- Optional two-step sign-in with an authenticator app (QR code setup), with ten one-time recovery codes.
- **Delete all stored information** clears Osaio logins, cameras, router rules and settings but keeps the administrator's sign-in; the password changes only under Sign-in settings.
- Optional password hint, shown under "Forgot the password?" on the PC only. There is no password reset: after the recovery tips, the PC running BombeCam (and only it) offers **Shut down and delete saved data**, which starts BombeCam over from scratch. Saved data that can't be opened (lost key, damaged file) can be deleted from the first-start screen.
- Sign-in forms work with browser and password-manager autofill ("Remember password"); Osaio, router, server-key, app ID and stream password fields are marked so password managers (Bitwarden, 1Password, LastPass) leave them alone.

**Server key and app ID**
- The downloads ship with the latest known Osaio server key and Osaio app ID, so there is nothing to set up. Neither is in the source code: builds take them from `osaio-setup.txt` (written by `go run ./tools/setup`, which `build.cmd` runs the first time) or from `BOMBECAM_SERVER_KEY` and `BOMBECAM_APP_ID`, and keep them obscured in the binary rather than as plain text. A build without one of them asks for it on the web page, and sign-in waits until it is saved.
- The server key and app ID work the same way. **Settings → Server key** and **Settings → App ID** say where the value in use comes from (they never show it) and let you paste a new one if Osaio changes it. It takes effect at once, without a restart or a new BombeCam version, and sessions reconnect by themselves. **Use built-in key** and **Use built-in app ID** go back to the shipped one. See README → Server key and app ID.
- The key can also be pinned with `-server-key-file`, `BOMBECAM_SERVER_KEY` or `BOMBECAM_SERVER_KEY_FILE`, and the app ID with `-app-id-file`, `BOMBECAM_APP_ID` or `BOMBECAM_APP_ID_FILE`.

**Web page**
- Viewer with live view (zoom, sound, full screen) and controls: pan/tilt, push-to-talk, night vision, status light, spotlight, motion and sound detection. Shows whether video arrives directly over the LAN or through Osaio's relay.
- Pan/tilt and spotlight controls follow the camera's model, on the page and in Home Assistant. Recognized models: WS01–WS04, P1, P1 Pro, P5, P10, C1, C1 Pro, C2, GC2, GC3, K1, K1 Pro, GK1, GK1 Pro, GK2, GL1, GW1, GW30, GW40, T1, T1 Pro, GT1 and GT1 Pro; Osaio model codes with a hardware revision (such as `GC3_A3S11A3`) are recognized. README → Supported cameras lists them by brand.
- Cameras tab: every camera in one list with a search box, streaming status, stream-name editing, View and Remove. Osaio logins tab: each login with its state and camera count, Add its cameras, Update password and Remove. Settings (formerly Manage) keeps the server key and app ID, Stop cameras, Start with Windows, Shut down and Delete all stored information. Dialogs scroll instead of running off the screen.
- Streams & ports: every address with copy buttons, configurable ports, optional stream password, optional JPEG snapshots.
- Frigate and Home Assistant guide with ready-to-paste settings, and optional Home Assistant MQTT discovery for camera controls.

**Block cloud video**
- Per-camera switches and Block all cameras, applied on a GL.iNet (firmware 4.x) or OpenWrt 21.02+ router (fw3 or fw4) over SSH with a restricted key; the router password is used once and not stored.
- Allows Osaio control, stream setup, DNS and time sync under a 4 KB/s cap per camera and drops all other internet traffic from the camera, including IPv6.
- Connect router suggests the router the cameras are on (for example a travel router), not just this PC's default gateway, and remembers the router used last, also after Disconnect.
- Clears a camera's open connections when blocking starts (after confirmation), and reports an error if a connection outside the allowed list reappears.
- Linux router support with nftables (`bombecam-net`), a boot guard and a traffic-based verifier (`bombecam-verify`).

**Linux and Docker**
- Linux downloads for amd64, arm64 and 32-bit ARM (Raspberry Pi), and a published Docker image, `ghcr.io/fever-r/bombecam-gateway`, for the same platforms. The image holds the program from the Linux download with the server key and app ID built in; `deploy/docker-compose.yml` runs it with MediaMTX. Each image is built and started on all four platforms before it is published. To run an image you built yourself, set `BOMBECAM_GATEWAY_IMAGE`.

**Windows**
- Single `bombecam.exe` with a tray icon, optional Start with Windows, logs in `%LOCALAPPDATA%\bombecam\logs`.
- The tray icon (and starting `bombecam.exe` again) brings an open BombeCam tab to the front instead of opening another; a new tab opens only when none is showing.
- **Shut down** on the page also closes the tab when the browser allows it.

**Security**
- Web page and API on `127.0.0.1` by default; LAN access requires an operator sign-in, with lockout after repeated wrong passwords.
- AES-256-GCM encrypted profile (DPAPI-protected key on Windows).
- MediaMTX downloads are verified against pinned SHA-256 checksums.

**Documentation**
- Technical documentation in `docs/technical`: configuration, troubleshooting, architecture, maintainers and the modularization plan, plus `DISCLAIMER.md`. The configuration and troubleshooting pages and the disclaimer ship in every download.
