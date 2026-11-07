# BombeCam

BombeCam is a local gateway for cameras that use the **Osaio app**, sold by Yoton, GNCC, Wolfang, Surfola and others (see [Supported cameras](#supported-cameras)). It connects to your cameras with an Osaio login and serves them on your own network as standard **RTSP, HLS and WebRTC** streams, with a web page for live view and camera controls. Frigate, Home Assistant, VLC and other NVRs can use the streams directly.

It can also turn on **Block cloud video** for each camera: rules on your router that limit what the camera may send to the internet, while the Osaio app keeps its controls.

- Runs on Windows (a single `bombecam.exe` with a tray icon) and on Linux (native or Docker).
- Video comes straight from the camera over your network; nothing is re-encoded.
- Camera controls: pan/tilt, night vision, status light, spotlight, motion and sound detection, and push-to-talk.
- Ready-to-paste settings for Frigate and Home Assistant, plus optional Home Assistant MQTT discovery for the controls.
- Your Osaio login is stored encrypted on your computer. There is no BombeCam account or server.

BombeCam is an independent project. It is not affiliated with, endorsed by or supported by OSAIO, Yoton, GNCC, Wolfang or Surfola.

## Download

Ready-to-run downloads are on the [Releases page](https://github.com/Fever-r/BombeCam/releases/latest):

| Download | For |
|---|---|
| `BombeCam-<version>-windows-amd64.zip` | Windows 10/11, 64-bit. Extract it and run `bombecam.exe`. |
| `BombeCam-<version>-linux-amd64.tar.gz` | Linux PCs and servers (x86-64) |
| `BombeCam-<version>-linux-arm64.tar.gz` | 64-bit ARM Linux, such as a Raspberry Pi 3, 4 or 5 with a 64-bit OS |
| `BombeCam-<version>-linux-armv6.tar.gz` | 32-bit Raspberry Pi OS |
| `SHA256SUMS.txt` | The SHA-256 checksum of each download |

The **Source code** archives that GitHub adds to every release are for [building BombeCam yourself](#building-from-source).

## Contents

- [Download](#download)
- [Requirements](#requirements)
- [Supported cameras](#supported-cameras)
- [Quick start (Windows)](#quick-start-windows)
- [Linux and Docker](#linux-and-docker)
- [Server key and app ID](#server-key-and-app-id)
- [The web page](#the-web-page)
- [Using the streams (Frigate, Home Assistant, VLC)](#using-the-streams-frigate-home-assistant-vlc)
- [How BombeCam works](#how-bombecam-works)
- [Block cloud video](#block-cloud-video)
- [Security and privacy](#security-and-privacy)
- [Troubleshooting](#troubleshooting)
- [Command-line options](#command-line-options)
- [Building from source](#building-from-source)
- [Project layout](#project-layout)
- [License](#license)

## Requirements

- **Cameras:** cameras set up in the Osaio app, such as the Yoton WS03. See [Supported cameras](#supported-cameras).
- **A second Osaio login for BombeCam** (see Quick start, step 2).
- **Windows 10/11 (64-bit)**, or **Linux** (amd64, arm64 or armv6/armv7), on the same network as the cameras.
- **FFmpeg (optional):** adds camera sound to the browser's WebRTC view, powers push-to-talk and snapshots. Video works without it. On Windows: `winget install Gyan.FFmpeg`.
- **For Block cloud video (optional):** a GL.iNet router on firmware 4.x or OpenWrt 21.02 or newer that your cameras connect to, or a Linux machine that routes the cameras' traffic. The allowlist uses the Osaio servers of your account's region (US or EU); see [Block cloud video](#block-cloud-video).

## Supported cameras

BombeCam supports the **Osaio camera protocol**, used by cameras sold under **Yoton, GNCC, Wolfang and Surfola**. Check the app named in your camera's manual and the model code shown on BombeCam's **Cameras** tab: the brand name alone does not establish compatibility. Cameras using another app and Osaio smart plugs are outside the current scope.

BombeCam was developed and tested with the **Yoton WS03**. The table lists the model families currently recognized by its control mapping. **Other models and rebrands are unverified with BombeCam**; appearing in Osaio or in this table does not guarantee working video, audio or controls on every firmware version. Osaio's [camera guides](https://osaioteam.zendesk.com/hc/en-us/sections/13953109233295-Security-Cameras) describe the hardware; they are not BombeCam compatibility tests. Reports are welcome (see the end of this section).

BombeCam offers live video and sound, talk, night vision, the status light, and motion and sound detection. Availability depends on the camera's hardware, firmware and media formats. Pan/tilt and spotlight buttons in the viewer and Home Assistant (MQTT) are selected from the model code Osaio reports; the **Yes/No** entries below describe that mapping, rather than a fresh hardware readback.

| Model code | Sold as | Kind | Pan/tilt | Spotlight |
|---|---|---|---|---|
| WS03 | Yoton WS03 (tested), Wolfang WS03 (unverified) | Indoor, plug-in | Yes | No |
| P1, P1 Pro, P5 (GP5), P10 | GNCC pet and baby cameras | Indoor | Yes | No |
| WS01 | Yoton WS01, Wolfang WS01, Surfola WS01 | Indoor | No | No |
| C1, C1 Pro, C2, GC2, GC3 | GNCC | Indoor | No | No |
| K1, K1 Pro, GK1, GK1 Pro, GK2 | GNCC | Outdoor | Yes | Yes |
| GL1 | GNCC light bulb camera | Light socket (indoor/outdoor) | Yes | Yes |
| WS04, WS04 Pro | Yoton / Wolfang WS04 family | Outdoor; WS04 mains-powered, WS04 Pro battery/solar | Yes | Yes |
| GW30 | GNCC | Outdoor, solar and battery | Yes | Yes |
| GW40 | GNCC | Outdoor, 4G cellular, solar | Yes | Yes |
| GW1 | GNCC (W1) | Outdoor, battery | No | Yes |
| WS02 | Wolfang WS02 | Outdoor, 2K | No | No |
| T1, T1 Pro, GT1, GT1 Pro | GNCC | Outdoor | No | No |

Model codes are matched loosely, so a code with extra letters or a hardware revision (such as `GP5` or `GC3_A3S11A3`) gets the controls of the model it contains.

**Good to know**

- **Plug-in cameras suit 24/7 streaming best.** Battery and solar cameras (WS04 Pro, GW30, GW40, GW1) may sleep between events. Live view may take longer to start, and keeping a stream open can increase battery use. Osaio documents the [mains-powered WS04](https://osaioteam.zendesk.com/hc/en-us/articles/58875323017113-Getting-Started-with-Your-WS04-Outdoor-Camera-Wired) and [battery/solar WS04 Pro](https://osaioteam.zendesk.com/hc/en-us/articles/56623701455385-Getting-Started-with-Your-WS04-Pro-Low-Power-Outdoor-Camera) separately.
- **4G cameras** such as GW40 are recognized in the control mapping but remain unverified. They are outside the supported local Wi-Fi streaming and [Block cloud video](#block-cloud-video) setup. Their presence in an Osaio account does not establish that BombeCam can stream them through a relay.
- **Other apps, other products.** Only cameras that use the Osaio app work. The same brands also sell action cameras and other products that use different apps; those don't.
- **A model that isn't listed** may work if it uses the same Osaio protocol. It gets pan/tilt buttons and no spotlight by default; those buttons do not prove the camera has a motor or that streaming works.

If your model is missing or shows the wrong controls, or you've tried one of the models above, open an issue with its brand, model code from the **Cameras** tab, firmware version, BombeCam version, and which features you verified (video, sound, audible talkback, controls). Include relevant `gateway.log` lines after removing camera names, device IDs, email addresses, passwords and tokens.

## Quick start (Windows)

1. **Download and extract** `BombeCam-<version>-windows-amd64.zip` from the [Releases page](https://github.com/Fever-r/BombeCam/releases/latest) into a folder of your choice, and **run `bombecam.exe`**. Windows may show "Windows protected your PC" for a newly downloaded program: choose **More info → Run anyway**. Your browser opens `http://127.0.0.1:8654`. BombeCam runs as an icon by the clock: click it to open the page again; right-click it for Start with Windows, the log folder and Shut down. (`bombecam-gateway.exe` is the same program with a console window.)
2. **Create the administrator.** On first start the page asks for a name and password for BombeCam's own sign-in (it has nothing to do with Osaio). Phones, tablets and other computers always need it; tick **Also ask for this sign-in on this PC** if other people use this computer. Your browser or password manager can remember it. Two-step sign-in with an authenticator app can be turned on later under **Settings → Sign-in settings**. You can add a password hint; it is shown under **Forgot the password?** on this PC only. The password changes only under **Settings → Sign-in settings**; **Delete all stored information** keeps it. There is no password reset: if nothing else works, **Forgot the password?** on this PC offers **Shut down and delete saved data**, which starts BombeCam over from scratch. Other devices can't do that.
3. **Give BombeCam its own Osaio login.** In the Osaio app, create a second login (for example `yourname+cams@example.com`; many email providers deliver `+tag` addresses to the same inbox), then share your cameras with it from your usual login (camera → Settings → Share).
   A camera streams live video to one viewer per Osaio login at a time. If the Osaio app and BombeCam used the same login, opening live view in the app would stop BombeCam's video until the app closed it. With separate logins, both work at once, your main password stays on your phone, and un-sharing a camera cuts BombeCam off.
4. **Add the login** on the page with that second login and your account's country, tick your cameras and click **Add to BombeCam**.
5. Use **+ Add cameras** later to add more, from the same login or another one; cameras from several logins appear together.
   - The **Cameras** tab lists every camera, with a search box for long lists. It shows which are streaming, renames their stream addresses, opens one in the viewer, and removes cameras.
   - The **Osaio logins** tab lists the logins BombeCam keeps (encrypted, on this computer only, used only to find and reach cameras). Add another login to bring in its cameras, **Update password** after changing it in the Osaio app (it is checked with Osaio before it is saved), or **Remove** a login together with the cameras added through it.
   - **Settings** holds the sign-in settings (password, sign-in on this PC, two-step sign-in, sign out), the server key and app ID, stops and starts all cameras, turns **Start with Windows** on or off, and shuts BombeCam down. **Delete all stored information** removes every stored Osaio login, camera and setting from the computer; your administrator sign-in stays.

On first launch BombeCam downloads its video server, [MediaMTX](https://github.com/bluenviron/mediamtx) 1.9.3 (about 20 MB), from its official GitHub release into `%LOCALAPPDATA%\bombecam\bin`, and checks the archive against a SHA-256 checksum built into BombeCam. To avoid the download, put `mediamtx.exe` next to `bombecam.exe` or on your PATH.

Windows may ask whether `mediamtx.exe` may use the network. Allow it on **Private** networks so NVRs and VLC on your LAN can read the streams.

## Linux and Docker

**Docker** (gateway and MediaMTX, host networking), from the `deploy` folder of the source (download it with the green **Code** button or `git clone`):

```
cd deploy
cp .env.example .env
docker compose up -d
```

This runs BombeCam's published image, `ghcr.io/fever-r/bombecam-gateway`, for amd64, arm64 and 32-bit ARM. It holds the same program as the Linux download, with the Osaio server key and app ID built in, so there is nothing to set up. Open `http://127.0.0.1:8654` on the Docker host and sign in, or set `OSAIO_EMAIL`, `OSAIO_PASSWORD`, `OSAIO_COUNTRY` and `OSAIO_ENROLLED` in `.env`. To reach the web page from other devices, set `GATEWAY_HTTP_BIND=0.0.0.0` (see [Security and privacy](#security-and-privacy)). In Docker, MediaMTX runs from `deploy/mediamtx.yml`, so ports and the stream password are set in that file.

To update: `docker compose pull && docker compose up -d`. The compose file follows the newest 1.x release; to stay on one release, set `BOMBECAM_GATEWAY_IMAGE=ghcr.io/fever-r/bombecam-gateway:1.0.0` in `.env`.

To run your own build instead, build the image at the top of the source folder and point `.env` at it:

```
docker build -f deploy/Dockerfile.gateway -t bombecam-gateway:local .
```

and set `BOMBECAM_GATEWAY_IMAGE=bombecam-gateway:local`. That image includes the Osaio server key and app ID only if the source folder has `osaio-setup.txt` (see [Building from source](#building-from-source)); without one of them, its web page asks for it at first start, or you can set it in `.env` (see [Server key and app ID](#server-key-and-app-id)).

**Native Linux:** download the archive for your machine from the [Releases page](https://github.com/Fever-r/BombeCam/releases/latest), then:

```
tar -xzf BombeCam-<version>-linux-amd64.tar.gz
cd BombeCam-<version>-linux-amd64
./bombecam-gateway -headless
```

Open `http://127.0.0.1:8654` on that machine (or start it with `-http 0.0.0.0:8654` to reach it from the LAN; see [Security and privacy](#security-and-privacy)). MediaMTX is found next to the binary, in `~/.local/share/bombecam/bin`, or on PATH, or downloaded as on Windows. The encrypted profile is stored in `/var/lib/bombecam` when writable, otherwise in `$XDG_DATA_HOME/bombecam` (or `~/.local/share/bombecam`).

**A Linux machine as the cameras' router** can apply Block cloud video with nftables. See [docs/linux-router.md](docs/linux-router.md).

## Server key and app ID

Osaio's cloud only answers requests that carry the app ID of its app and are signed with the app's server key, so BombeCam needs both. The two work the same way. The downloads on the Releases page and the published Docker image come with the latest server key and app ID known when the version was released, so there is nothing to set up. **Settings → Server key** and **Settings → App ID** say which ones are in use; the page never shows the key or the app ID itself.

Neither is in the source code: a build from source, including a Docker image you build yourself, has them only when they were set up for the build (see [Building from source](#building-from-source)); without one of them, the sign-in page asks for it at first start.

### If Osaio changes the server key or app ID

Sign-in and new live sessions start failing, usually right after an Osaio app update. Paste the new key under **Settings → Server key** and press **Save key**, or the new app ID under **Settings → App ID** and press **Save app ID**: it takes effect at once, no restart and no new BombeCam version needed, and logins that failed sign in again by themselves. A later BombeCam release ships the new values too; if you saved one by hand, **Use built-in key** or **Use built-in app ID** goes back to the shipped one.

The key can also be pinned with `-server-key-file <path>`, `BOMBECAM_SERVER_KEY` or `BOMBECAM_SERVER_KEY_FILE`, and the app ID with `-app-id-file <path>`, `BOMBECAM_APP_ID` or `BOMBECAM_APP_ID_FILE`. These override the page, which then can't change that value. Details, order of sources and file format: [docs/technical/CONFIGURATION.md](docs/technical/CONFIGURATION.md); symptoms and checks: [docs/technical/TROUBLESHOOTING.md](docs/technical/TROUBLESHOOTING.md).

## The web page

- **Viewer:** live view of the chosen camera (pick it at the top left) with zoom, sound and full screen, and the controls beside it: pan/tilt, talk, night vision, status light, motion and sound detection, spotlight. A badge shows whether video arrives **Direct (LAN)** or **Relayed via Osaio**.
- **Streams & ports:** each camera's RTSP and HLS address with copy buttons, the ports other devices connect to, and settings for Frigate and Home Assistant.
- **Firewall:** Block cloud video, with a switch per camera and **Block all cameras**. The header badge shows how many cameras are blocked.

### Does BombeCam need to keep running?

| | While BombeCam runs | After it is closed |
|---|---|---|
| RTSP / HLS / WebRTC streams and snapshots | Served from this computer | Stop. Frigate, Home Assistant and VLC reconnect when BombeCam starts again; the addresses stay the same. |
| Controls from BombeCam and Home Assistant (MQTT) | Work | Stop. The Osaio app keeps working as usual. |
| Block cloud video | Changes apply at once | Keeps working. The rules live on the router, which reloads them at boot. BombeCam is only needed to change them. |

If an NVR records from BombeCam, turn on **Start with Windows** under **Settings** so the streams come back after a restart. It needs no administrator rights.

## Using the streams (Frigate, Home Assistant, VLC)

The **Streams & ports** tab lists each camera's addresses. **Full Frigate / Home Assistant guide** (or `http://127.0.0.1:8654/integrations.html`) shows ready-to-paste settings for every camera, with this computer's network address filled in, for Frigate or Home Assistant on another device, in Docker Desktop on this PC, or on this PC directly.

- **Stream addresses.** Each camera gets a short name from its camera name, for example `rtsp://192.168.1.20:8554/front_door` and `http://192.168.1.20:8888/front_door/index.m3u8` (HLS). Rename it on the **Cameras** tab. The camera-ID form `rtsp://<pc>:8554/<camera-id>` also works.
- **Frigate.** Paste the generated block into Frigate's **Configuration editor**. It adds each camera to go2rtc (live view with sound), records with the camera's own AAC audio, and runs detection at 5 fps. A complete starter configuration is on the page too.
- **Home Assistant.** **Settings → Devices & services → Add integration → Generic Camera**, then paste the **Stream source URL** (and the **Still image URL** if snapshots are on).
- **Camera controls in Home Assistant (optional).** With an MQTT broker (for example Home Assistant's Mosquitto add-on) and the MQTT integration, turn on **MQTT** on the page. Each camera then appears as a device with night vision, status light, motion and sound detection, spotlight, pan/tilt buttons and a connection status.
- **Stream password (optional).** When on, other devices need the user name `bombecam` and a generated password; the addresses on the page include them. This computer never needs it.
- **Snapshots (optional).** A JPEG of the current picture on port 8655, for example for Home Assistant's still image. Needs FFmpeg.
- **Ports:** RTSP 8554, HLS 8888, WebRTC 8889 (TCP) and 8189 (UDP), snapshots 8655. They can be changed on the page.
- **Which address?** BombeCam uses the network adapter that has the internet connection. With several adapters (Wi-Fi and Ethernet, a VPN, Docker), pick one on the page.
- **Another device can't connect?** Allow `mediamtx.exe` (streams) and `bombecam.exe` (snapshots) through Windows Firewall on **Private** networks. If Windows treats your home network as **Public**, the page warns you; change it under **Settings → Network & internet → (your connection) → Network profile type → Private**.
- **Quick check:** in VLC use **Media → Open Network Stream** with the RTSP address, or run `ffprobe -rtsp_transport tcp rtsp://...`.

## How BombeCam works

| | Path |
|---|---|
| Sign-in, camera list, starting each live session | Osaio's servers introduce BombeCam to the camera (WebRTC signaling). |
| Video and sound from the camera | Directly from the camera to BombeCam over your network (WebRTC). If the camera can only be reached through Osaio's relay, the viewer says so. |
| RTSP / HLS / WebRTC streams for NVRs and players | Served by MediaMTX on your computer. BombeCam publishes each camera to it without re-encoding. |
| Your talk audio | Directly from BombeCam to the camera over the same connection (FFmpeg encodes it). |
| Controls: pan/tilt, night vision, status light, detection, starting and stopping talk | Sent through Osaio's servers, the same commands the Osaio app sends. The camera accepts control commands only from Osaio. |

BombeCam never modifies camera firmware.

**What BombeCam contacts on the internet:** Osaio's servers (sign-in, camera list, stream setup and controls, using your Osaio login) and, only if MediaMTX isn't installed, GitHub to download MediaMTX once. Nothing else.

## Block cloud video

The **Firewall** tab has **Block all cameras** and a switch per camera.

- **On:** the Osaio app can still control the camera (light, pan/tilt, night vision), and BombeCam's local streams keep working. Other internet traffic from the camera is dropped.
- **Off:** the camera works normally. Turn a camera off for a while to let it update its firmware.

> Camera internet access is limited by destination and rate; encrypted traffic may still carry image data.

Osaio can still see that the camera is online, your internet (IP) address, and which commands are sent to it.

**How it works.** The rules go on the router your cameras connect to: a GL.iNet router on firmware 4.x or OpenWrt 21.02+, with either the fw3 (iptables) or fw4 (nftables) firewall. They match each blocked camera by its MAC address and only allow, drop and rate-limit traffic:

| Traffic from a blocked camera to | Result |
|---|---|
| Your own network (10.x, 172.16–31.x, 192.168.x), such as the BombeCam computer | Untouched |
| Osaio control (US: `mqtts02-us.osaio.net`; EU: `mqtts-eu.osaio.net`, `mqtts01-eu.osaio.net`; TCP 8883) | Allowed, capped |
| Osaio stream setup (US: `wss-us.osaio.net`; EU: `wss-eu.osaio.net`; TCP 443) | Allowed, capped (can be blocked too) |
| DNS (53) and time sync (UDP 123), IPv4 | Allowed, capped |
| Everything else, including cloud clip and snapshot uploads, Osaio's relay servers and all IPv6 | Dropped |

All allowed traffic shares a cap of 4 KB/s per camera with an 8 KB burst (some fw3 routers use a 4-packet/s fallback; router status shows which). The cap cannot inspect encrypted content, so it cannot rule out small images or very low-rate video. Nothing is redirected or rewritten.

When blocking turns on, the router also clears that camera's open connections, so a connection opened before the block can't carry on past the rules. The page asks for confirmation first. The camera reconnects its allowed connections by itself. Right after clearing, the router checks the camera's connections again; if one outside the allowed list is back, it reports that the block is not working instead of showing the camera as blocked.

**Same network:** a phone or tablet on the same network as a blocked camera may still show its live view in the Osaio app. That video travels directly over your own network, not the internet, so the camera is still blocked from sending video out. Away from your network, the app's live view has to come through Osaio's relay servers, which a blocked camera can't reach.

**Region:** the allowlist uses the control and stream-setup servers of one Osaio region. Choose **US** or **EU** under **Connect router → Advanced → Osaio region**; it defaults to US. A camera enrolled in a different region would lose its app controls while blocked, and BombeCam could not start new local streams, so set the region your Osaio account belongs to before blocking. Changing it reapplies the rules.

**Connecting the router.** **Connect router** asks for the router's address and admin password once. BombeCam connects over SSH, installs its router script and adds its own SSH key, restricted on the router to the script's commands (apply the camera list, status, connections, version, uninstall): it can't open a shell or forward connections. The password is used for that one connection and is never saved. The router's SSH host key is remembered on first use, and BombeCam refuses to connect if it changes. **Disconnect** removes everything BombeCam put on the router.

Details, manual setup without BombeCam, and how to check that blocking works: [docs/block-cloud-video.md](docs/block-cloud-video.md). Without such a router, **Other ways to block camera traffic** (a page linked from the Firewall tab) compares the alternatives.

## Security and privacy

- **Web page and API** listen on `127.0.0.1:8654` by default, so only this computer can use them. BombeCam has its own **administrator** sign-in, created on this computer at first start; Osaio logins never sign anyone in to BombeCam. If you make the page reachable from the LAN (`-http 0.0.0.0:8654`), other devices must sign in as the administrator before they can see or change anything. On this computer the page opens without signing in unless you chose **Also ask for this sign-in on this PC**. Optional **two-step sign-in** asks for a code from an authenticator app (one-time recovery codes cover a lost phone). Five wrong passwords or codes from one address lock it out for 10 minutes. Passwords are stored as Argon2id hashes inside the encrypted profile. Cross-origin and DNS-rebinding requests are rejected. For a machine whose page you only open from other devices (no browser on it), set `BOMBECAM_ADMIN_PASSWORD` (and optionally `BOMBECAM_ADMIN_NAME`) once to create the administrator at start-up.
- **Streams** (RTSP, HLS, WebRTC) can be read by any device on your network, so NVRs can connect without setup. Turn on the **stream password** to require a user name and password. Only BombeCam on this computer may publish streams or use MediaMTX's management API, which listens on `127.0.0.1` only.
- **Stored data:** your Osaio logins, camera list, settings and BombeCam's router key are stored in an AES-256-GCM encrypted profile: on Windows in `%LOCALAPPDATA%\Bombecam\` with the key protected by Windows DPAPI; on Linux in the data directory described above, with a key file readable only by its owner. A server key or app ID saved from the web page is kept next to it in `server.key` or `app.id`, readable only by your user. Passwords, the server key and the app ID are never written to logs.
- **Router access:** see [Connecting the router](#block-cloud-video).
- **Downloads:** MediaMTX is only installed from its official release archive when its SHA-256 matches the checksum built into BombeCam.

To report a security problem, see [SECURITY.md](SECURITY.md).

## Troubleshooting

The live view always says what it is waiting for:

| Message | Meaning |
|---|---|
| *Video server problem: ...* | MediaMTX isn't running. The reason is shown there and in `%LOCALAPPDATA%\bombecam\logs\mediamtx.log`. |
| *Cameras are stopped...* | **Stop cameras** was pressed; press **Start cameras** under **Settings**. |
| *Connecting to the camera...* | The session is being set up (usually 5–20 s). |
| *Connected. Waiting for the first video frames...* | The camera is connected; BombeCam is waiting for a keyframe. |
| *No video yet: the Osaio app is watching this camera with the same Osaio login...* | Close live view in the app, and give BombeCam its own login (Quick start, step 2). |
| *...failed to connect 3 times...* | BombeCam retries by itself after 30 s, 1 minute, then every 2 minutes. **Retry now** skips the wait. |
| *Osaio rejected the sign-in* | Wrong email or password, or, after an Osaio app update, an outdated server key or app ID: see [If Osaio changes the server key or app ID](#if-osaio-changes-the-server-key-or-app-id). |
| *Osaio server key needed* | There is no key BombeCam can use (for example a broken key file, or a build without one). Paste a working one under **Settings → Server key**; see [docs/technical/TROUBLESHOOTING.md](docs/technical/TROUBLESHOOTING.md). |
| *Osaio app ID needed* | There is no app ID BombeCam can use (for example a broken app ID file, or a build without one). Paste a working one under **Settings → App ID**; see [docs/technical/TROUBLESHOOTING.md](docs/technical/TROUBLESHOOTING.md). |

- Everything BombeCam prints is saved to `%LOCALAPPDATA%\bombecam\logs\gateway.log` (the previous run is kept as `gateway.prev.log`).
- Camera sound is off by default; use the speaker button on the video.
- **Talk** needs FFmpeg to encode your voice for the camera.
- Firewall problems: see [docs/block-cloud-video.md](docs/block-cloud-video.md#troubleshooting).

## Command-line options

Every option can also be set with the environment variable shown.

```
-http string              web page / API address (default 127.0.0.1:8654; OSAIO_HTTP)
-headless                 don't open a browser or show a tray icon (BOMBECAM_HEADLESS)
-startup                  started with Windows at sign-in: don't open the browser
-ffmpeg string            FFmpeg path (default: search next to the exe, the cache folder, PATH, winget/scoop/choco; FFMPEG_PATH)
-mediamtx-path string     MediaMTX binary (default: next to the exe, cache folder, PATH, else download; BOMBECAM_MEDIAMTX_PATH)
-mediamtx-config string   use your own mediamtx.yml instead of the built-in one (BOMBECAM_MEDIAMTX_CONFIG)
-no-mediamtx-supervisor   use an external MediaMTX instead of starting one (BOMBECAM_NO_MEDIAMTX_SUPERVISOR)
-rtsp-port / -hls-port / -webrtc-port / -webrtc-udp-port
                          ports of the built-in MediaMTX (default 8554 / 8888 / 8889 / 8189;
                          BOMBECAM_RTSP_PORT etc.). When set, they replace the Ports option on the page.
-consumer-rtsp-base string  RTSP base address advertised to other devices (default: detected; BOMBECAM_RTSP_CONSUMER_BASE)
-publisher string         native (built-in, default) or ffmpeg (BOMBECAM_PUBLISHER)
-retry-max-wait duration  longest wait between automatic retries after 3 failed starts (default 2m; BOMBECAM_RETRY_MAX_WAIT)
-strict-manual            after 3 failed starts, wait for Retry now instead of retrying (STRICT_MANUAL)
-server-key-file string   file holding an Osaio server key to use instead of the built-in or saved one
                          (or BOMBECAM_SERVER_KEY / BOMBECAM_SERVER_KEY_FILE; see Server key and app ID)
-app-id-file string       file holding an Osaio app ID to use instead of the built-in or saved one
                          (or BOMBECAM_APP_ID / BOMBECAM_APP_ID_FILE; see Server key and app ID)
-email / -password / -country / -enrolled
                          sign in and choose cameras without the web page
                          (OSAIO_EMAIL, OSAIO_PASSWORD, OSAIO_COUNTRY, OSAIO_ENROLLED)
-profile-path / -profile-key-file   location of the encrypted profile and its key
-control-transport string cloud (default) or mqtt (experimental; the camera accepts only Osaio's own server)
```

## Building from source

You only need this to change BombeCam or to run a version that isn't released yet. It requires [Go](https://go.dev/dl/) 1.26 or newer (on Windows: `winget install GoLang.Go`).

**The Osaio server key and app ID.** Neither is in the source code. Both come from the Osaio app; BombeCam's maintainers build them into the release downloads, so if you don't have them, use a release download. If you do, run `go run ./tools/setup` once and paste both: they are saved, obscured, in `osaio-setup.txt`, which git ignores, and every build after that includes them, obscured in the binary too (`make`, `build.ps1`, `build.cmd`, a Docker image you build and `tools/release`). A build without one of them asks for it on its web page.

**Windows, the easy way:** download the source (the green **Code** button → **Download ZIP**, or a release's **Source code (zip)**), extract it, and double-click **`build.cmd`**. It asks for the Osaio server key and app ID while either is not set up (Enter skips one; the build then asks for it on its web page). It builds `bombecam.exe` in that folder and offers to start it.

**Command line:**

```
git clone https://github.com/Fever-r/BombeCam.git
cd BombeCam
go run ./tools/setup            # once: the Osaio server key and app ID that builds include
make build                      # Linux/macOS: all programs into bin/native
.\build.ps1                     # Windows: bin\windows-amd64\ and .\bombecam.exe, with SHA256SUMS.txt
go run ./tools/release          # all release downloads into dist/; needs the server key and app ID (see docs/releasing.md)
go test ./...                   # tests (some need FFmpeg, MediaMTX or root; see CONTRIBUTING.md)
```

`bombecam-gateway -version` prints the version a binary was built from. See [CONTRIBUTING.md](CONTRIBUTING.md) for the test setup and [docs/releasing.md](docs/releasing.md) for how releases are made.

## Project layout

| Path | Contents |
|---|---|
| `cmd/bombecam-gateway` | The gateway: HTTP API, embedded web page, camera sessions, streams, controls, Frigate/Home Assistant output, tray icon. |
| `pkg/bridge` | Osaio cloud client, WebRTC session with the camera, depacketizer, built-in RTSP publisher, talk audio. |
| `pkg/osaiovalue`, `pkg/serverkey` | The server key and app ID: the values built into a binary (set at build time) and the ways to replace them. The app ID's part is in `pkg/bridge`. |
| `tools/setup`, `internal/buildkey`, `internal/obscure` | Saves the Osaio server key and app ID that builds include (`osaio-setup.txt`) and passes them, obscured, to the linker. |
| `internal/mediamtx` | Starts and watches MediaMTX, generates its configuration, downloads and verifies it. |
| `internal/ui`, `web/` | The web page, embedded in the binary. |
| `pkg/profile` | Encrypted profile (AES-256-GCM; DPAPI on Windows). |
| `pkg/auth` | Sign-in, sessions, CSRF and host/origin checks for the API. |
| `pkg/policy` | The Block cloud video allowlist: hosts, ports and cap. |
| `pkg/renderer/openwrt`, `pkg/routerpush`, `router-setup/` | The OpenWrt/GL.iNet router script and its SSH installation. |
| `pkg/renderer/nftables`, `pkg/netstack`, `cmd/bombecam-net` | The same rules on a Linux router. |
| `cmd/bombecam-verify`, `pkg/verifier`, `pkg/prober` | Checks a Linux router's rules with real traffic. |
| `cmd/bombecam-policy`, `cmd/bombecam-certgen` | Print the compiled policy; generate TLS material for the experimental local MQTT mode. |
| `deploy/` | Docker Compose files, MediaMTX configuration, Linux boot guard. |
| `tests/` | End-to-end tests with a mock Osaio cloud, and router tests against real kernel traffic. |
| `internal/version` | The release version. |
| `tools/release`, `.github/workflows` | Builds the release downloads; CI and the tag-triggered release workflow. |

## License

MIT; see [LICENSE](LICENSE) and [DISCLAIMER.md](DISCLAIMER.md). Third-party components and their licenses are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). Technical documentation for maintainers is in [docs/technical](docs/technical/README.md).
