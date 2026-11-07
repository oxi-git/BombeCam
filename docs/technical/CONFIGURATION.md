# Configuration: the server key and app ID

## Why BombeCam needs them

Osaio's cloud answers only requests that carry the app ID of its app and are signed with the key of its app (the app ID is part of what is signed). BombeCam has to send every sign-in, camera-list and stream-setup request the same way, or the cloud refuses it. Without a usable key or app ID, BombeCam sends nothing.

BombeCam's downloads and its published Docker image come with the **latest key and app ID known when that version was released**, so they work without any setup. Neither is in the source code: a build from source includes them only when they were set up for the build (see [Building with a key](#building-with-a-key)), and otherwise asks for the missing one on the web page. Osaio can change its key or its app ID at any time, usually with an app update. When that happens, sign-in and new live sessions fail until BombeCam uses the new value. You can paste a new key yourself under **Settings → Server key**, and a new app ID under **Settings → App ID**; either takes effect at once, without a restart or a new BombeCam version.

The key and the app ID work the same way. This page explains each step once and names both.

## Where BombeCam looks

For each value, BombeCam uses the **first source that is set**:

| Order | Server key | App ID | Set by | Change it from the web page |
|---|---|---|---|---|
| 1 | `-server-key-file <path>` | `-app-id-file <path>` | command line | No |
| 2 | `BOMBECAM_SERVER_KEY` | `BOMBECAM_APP_ID` | environment variable holding the value | No |
| 3 | `BOMBECAM_SERVER_KEY_FILE` | `BOMBECAM_APP_ID_FILE` | environment variable holding a file path | No |
| 4 | `server.key` in the data folder | `app.id` in the data folder | **Settings → Server key** or **Settings → App ID**, or you | Yes |
| 5 | The built-in key | The built-in app ID | the build: release downloads and the published Docker image have both, a build from source only what was set up ([below](#building-with-a-key)) | Yes (replace it, or go back to it) |

The two are looked up separately: the key can come from the environment while the app ID is the one saved on the page, for example.

A source that is set but broken (a missing file, a file with two value lines) is reported as broken. BombeCam does not quietly move on to the next source, so you always know which key and app ID are in use. A broken `server.key` or `app.id` in the data folder is fixed by saving a new value on the web page; a broken command-line or environment source has to be fixed where it is set.

At start, the log says where each value comes from, never the value itself:

```
[server-key] using the key from the key built into this version
[app-id] using the app ID from the saved app ID file (app.id)
[app-id] BOMBECAM_APP_ID_FILE is set but unusable: no usable Osaio app ID: the file named in BOMBECAM_APP_ID_FILE does not exist
[app-id] no app ID configured; enter it on the web page (cloud sign-in waits until then)
```

The data folder is the folder of the encrypted profile:

| System | Data folder |
|---|---|
| Windows | `%LOCALAPPDATA%\Bombecam\` |
| Linux service or Docker | `/var/lib/bombecam/` |
| Linux, normal user | `$XDG_DATA_HOME/bombecam/` or `~/.local/share/bombecam/` |
| `-profile-path` set | the folder of that path |

## Changing the key

**On the web page (easiest).** Open **Settings → Server key**. The dialog says which key is in use; it never shows the key. Paste the new one and press **Save key**; the next request uses it, including for sessions that are already open, and BombeCam signs in again by itself if the last attempt failed. The key is saved as `server.key` in the data folder and kept across restarts.

**Going back to the built-in key.** Press **Use built-in key** in the same dialog (or save the built-in value). This deletes `server.key`. Do this after updating BombeCam if you had saved a key earlier: a saved key always wins over the built-in one, even when the new version ships a newer key.

## Changing the app ID

**On the web page (easiest).** Open **Settings → App ID** (the **🪪 App ID** button). The **Osaio app ID** dialog says which app ID is in use; it never shows the app ID. Paste the new one under **New app ID** and press **Save app ID**; the next request uses it, including for sessions that are already open (live video that reconnects uses it too), and BombeCam signs in again by itself if the last attempt failed. The app ID is saved as `app.id` in the data folder and kept across restarts.

**Going back to the built-in app ID.** Press **Use built-in app ID** in the same dialog (or save the built-in value). This deletes `app.id`. Do this after updating BombeCam if you had saved an app ID earlier: a saved app ID always wins over the built-in one, even when the new version ships a newer app ID.

The **Use built-in key** and **Use built-in app ID** buttons appear only when this version has a built-in value, another one is in use, and the page can change it.

## When one is missing

Without a usable key or app ID, sign-in stops before anything is sent to Osaio, and the page asks for the missing value:

| | Server key | App ID |
|---|---|---|
| Sign-in page | Panel **BombeCam needs a working Osaio server key**, field **Osaio server key**, button **Save key** | Panel **BombeCam needs a working Osaio app ID**, field **Osaio app ID**, button **Save app ID** |
| Status chip in the header | **Osaio server key needed (Settings)** | **Osaio app ID needed (Settings)** |
| **Osaio logins** tab | **Server key needed** | **App ID needed** |

Saved logins wait instead of retrying. As soon as the missing value is saved on the page, BombeCam continues starting up and signs them in again by itself. If the value comes from the command line or the environment, the panel says so and the field is locked: fix it there and restart BombeCam.

## Setting them outside the web page

**In a file.** A text file with the value on one line. Blank lines and lines starting with `#` are ignored. BombeCam writes its own files like this:

```
# BombeCam server key. Keep this file private.
# If Osaio changes its key, replace the line below (or use Settings > Server key on the web page).
<server-key>
```

```
# BombeCam: the Osaio app ID. Keep this file private.
# If Osaio changes its app ID, replace the line below (or use Settings > App ID on the web page).
<app-id>
```

Save it as `server.key` or `app.id` in the data folder, or point to it:

| | Server key | App ID |
|---|---|---|
| Command line | `-server-key-file /path/to/server.key` | `-app-id-file /path/to/app.id` |
| Environment | `BOMBECAM_SERVER_KEY_FILE=/path/to/server.key` | `BOMBECAM_APP_ID_FILE=/path/to/app.id` |

`server.key` and `app.id` hold the value as plain text; unlike `osaio-setup.txt`, they are not obscured. BombeCam creates them with file mode 600, so on Linux only its own user can read them. Git and Docker builds ignore `server.key`, `app.id`, `*.server.key` and `*.app.id`.

**In the environment or on the command line.**

```bash
BOMBECAM_SERVER_KEY=<server-key> BOMBECAM_APP_ID=<app-id> ./bombecam-gateway -headless
./bombecam-gateway -headless -server-key-file /path/to/server.key -app-id-file /path/to/app.id
```

**With Docker.** The published image (`ghcr.io/fever-r/bombecam-gateway`, what `deploy/docker-compose.yml` runs) has the built-in key and app ID of its release, like the downloads. An image you build yourself has them only if `osaio-setup.txt` was set up before it was built ([below](#building-with-a-key)); otherwise paste each missing one on the web page once. A key or app ID saved on the page is kept in the `gateway_state` volume (`server.key` and `app.id` in `/var/lib/bombecam`), so it survives updating the image. To pin one, set `BOMBECAM_SERVER_KEY` or `BOMBECAM_APP_ID` in `deploy/.env`, or mount a file and set `BOMBECAM_SERVER_KEY_FILE` or `BOMBECAM_APP_ID_FILE`. Leave them empty to use the built-in or saved values.

Values set on the command line or in the environment are read once at start; restart BombeCam after changing them. The web page says where they come from but cannot change them.

## Building with a key

The server key and the Osaio app ID that goes with it are never written into the code. Both are set when a binary is linked, each from the first of these that is set:

1. `BOMBECAM_SERVER_KEY` or `BOMBECAM_APP_ID` in the environment of the build.
2. `osaio-setup.txt` at the top of the source folder: `server_key=...` and `app_id=...` lines, `#` comments allowed. Setup writes the values obscured; a plain value typed by hand works too. Git ignores the file.

In the file and in the binary both are obscured (`internal/obscure`), not plain text. That keeps them out of a casual look with `cat`, `strings` or grep; it is not protection, since the code that reads them is open source and the app ID travels to Osaio with every request.

`go run ./tools/setup` asks for both and writes `osaio-setup.txt` (`-key <key>` and `-app-id <id>` save without asking). `make`, `build.ps1`, `build.cmd` and a Docker image built from source (`deploy/Dockerfile.gateway`) read it through `go run ./tools/setup -ldflags`, which prints the linker flags (or nothing when nothing is set up); `tools/release` reads it directly. The Docker build sees only `osaio-setup.txt`, not the environment of the computer running Docker. `build.cmd` runs the setup the first time. A value that is set but unusable stops the build instead of building without it.

Only letters, digits and `+ / = . _ -` can be built in, because the values pass through make, shells and the linker unquoted.

A build without the server key or without the app ID asks for the missing one on the web page ([When one is missing](#when-one-is-missing)), or takes it from the command line or the environment ([Where BombeCam looks](#where-bombecam-looks)). Until then, sign-in waits and nothing is sent to Osaio.

The release workflow takes both from the repository's `OSAIO_SERVER_KEY` and `OSAIO_APP_ID` secrets, and `tools/release` refuses to build downloads without them unless given `-allow-no-key` (CI uses that to check that everything builds). The published Docker image takes the gateway program from those Linux downloads (`GATEWAY_FROM=release` in `deploy/Dockerfile.gateway`), so its build never sees the values or the secrets.

## What counts as a valid key or app ID

BombeCam treats both as opaque strings and only checks that the value can be one at all:

| Problem | Server key | App ID |
|---|---|---|
| Empty value | `the key is empty` | `the app ID is empty` |
| Spaces or line breaks inside the value | `the key contains spaces or line breaks` | `the app ID contains spaces or line breaks` |
| Longer than 512 characters | `the key is longer than 512 characters` | `the app ID is longer than 512 characters` |
| File missing or unreadable | `server.key does not exist` / `cannot be read` | `app.id does not exist` / `cannot be read` |
| File with two value lines | `has more than one key line` | `has more than one app ID line` |
| File with only blank lines and comments | `has no key in it` | `has no app ID in it` |
| File larger than 16 KB | `is larger than 16 KB` | `is larger than 16 KB` |

File messages start with `server.key` or `app.id` for the file in the data folder. A file you pointed to is named by its setting (`the file named by -app-id-file`, `the file named in BOMBECAM_APP_ID_FILE`), never by its path, in case a value was pasted there by mistake. The log, the web page and the API show each message after `no usable Osaio server key:` or `no usable Osaio app ID:`.

Spaces and line endings around the value, and a byte-order mark added by Windows Notepad, are removed first. A key or app ID that passes these checks is not necessarily the right one; only a successful sign-in shows that.

## The settings API

Each value has its own endpoint; both work the same way:

| | Server key | App ID |
|---|---|---|
| Endpoint | `/api/v1/server-key` | `/api/v1/app-id` |
| `POST` body | `{"key": "..."}` | `{"app_id": "..."}` |
| Saved to | `server.key` | `app.id` |
| Field in `/api/v1/onboarding/status` | `server_key` | `app_id` |
| `session_status` while it is missing | `server_key_missing` | `app_id_missing` |

```bash
curl http://127.0.0.1:8654/api/v1/server-key
curl http://127.0.0.1:8654/api/v1/app-id
```

```json
{"configured": true, "source": "built_in", "editable": true, "has_built_in": true}
```

`source` is `command_line`, `environment`, `env_file`, `key_file` (the saved `server.key` or `app.id`) or `built_in`, and empty when nothing is set. A `problem` field explains a broken source. The reply never contains the key or the app ID itself, and neither does `/api/v1/onboarding/status`; the page can only send a new value.

| Request | Effect |
|---|---|
| `POST` with the body above | Use this value from now on (saved to its file; saving the built-in value deletes the file instead) |
| `DELETE` | Go back to the built-in value (in a build without one, no value is in use afterwards) |

A successful request replies `{"ok": true, "server_key": {...}}` or `{"ok": true, "app_id": {...}}` with the new status. A failed one replies with `error` and `message`:

| Reply | Server key | App ID | Meaning |
|---|---|---|---|
| `200` | | | Done; the new value is in use |
| `400` | `invalid_server_key` | `invalid_app_id` | The value failed the checks above; the old one stays in use |
| `400` | `invalid_request` | `invalid_request` | The body is not JSON |
| `409` | `server_key_not_editable` | `app_id_not_editable` | The value comes from the command line or environment; change it there |
| `500` | `server_key_not_saved` | `app_id_not_saved` | The file could not be written or removed |
| `503` | `server_key_unavailable` | `app_id_unavailable` | BombeCam is still starting; try again in a moment |

While a value is missing, a sign-in replies `428 server_key_missing` or `428 app_id_missing`.

The key and the app ID are never written to the log or to the encrypted profile.

## Testing without a real account

The tests never use a real account and mostly use made-up keys and app IDs:

- Unit tests pass a made-up key with `serverkey.Static("synthetic-test-key-0001")`, and always put a made-up app ID in `BOMBECAM_APP_ID`, overriding any value set in the environment.
- The end-to-end tests start the real gateway against a mock cloud that **refuses any request not signed with the key it expects or not carrying the app ID it expects**. Most tests set a made-up key through `BOMBECAM_SERVER_KEY`. The harness builds the gateway with a made-up built-in key and app ID; tests switch the mock between those and a made-up "new" key or app ID to check that a swap works without a restart, and others run a build with no built-in key, or with neither value, as from source.

See [MAINTAINERS.md](MAINTAINERS.md#tests) for the commands.

## Local firewall (bombecam-net)

"Block cloud video" can be applied in two places, chosen in the **Firewall** tab:

- **Router (OpenWrt / GL.iNet)** — the default: the rules go to the router over SSH with BombeCam's restricted key.
- **This computer (bombecam-net)** — the rules go to the local `bombecam-net` daemon's API instead, as on Home Assistant. No router connection is needed.

A profile that does not name a backend uses the router, so existing setups are unchanged.

The gateway finds the daemon through the environment:

| Variable | Default | What it is |
|---|---|---|
| `BOMBECAM_LOCAL_FIREWALL_URL` | `http://127.0.0.1:8653` | Base URL of the bombecam-net API |
| `BOMBECAM_LOCAL_FIREWALL_KEY` | empty | Bearer token, when the daemon was started with `--api-key` (or `BOMBECAM_NET_API_KEY`) |

Both are read when the gateway starts and whenever it talks to the daemon. If the daemon is not running, the Firewall tab says the local firewall is unreachable; the camera choices are still saved and are applied on the next successful sync.

## Other keys that are not the server key

| Name | What it is |
|---|---|
| `BOMBECAM_PROFILE_KEY`, `BOMBECAM_PROFILE_KEY_FILE`, `profile.key` | Encrypts the saved profile on this computer |
| `BOMBECAM_NET_API_KEY` | Protects the Linux firewall service's API |
| `bombecam-certgen` output | TLS certificates for the experimental local MQTT mode |
| BombeCam's router SSH key | Lets BombeCam switch camera blocking on the router |
| Osaio email and password | The account BombeCam signs in with |

Changing any of these does not change the server key or the app ID, and neither of those replaces any of them.
