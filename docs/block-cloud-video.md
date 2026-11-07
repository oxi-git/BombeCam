# Block cloud video

Block cloud video puts rules on the router your cameras connect to. For each blocked camera, the router lets the Osaio app keep its controls (light, pan/tilt, night vision) and lets BombeCam start local streams, and drops the rest of the camera's internet traffic.

> Camera internet access is limited by destination and rate; encrypted traffic may still carry image data.

Osaio can still see that the camera is online, your internet (IP) address, and which commands are sent to it.

## Requirements

- **Router:** a GL.iNet router on firmware 4.x, or OpenWrt 21.02 or newer, in **Router** mode (not access point, extender or WDS mode). Both OpenWrt firewalls are supported: fw4 (nftables) and fw3 (iptables). Which one a router uses depends on the model; the script detects it.
- **The cameras connect to that router** (its Wi-Fi or LAN). If your main router comes from your internet provider, a small GL.iNet travel router in Router mode can host the cameras.
- **SSH access** as `root` with the router's admin password. GL.iNet has SSH on by default.
- **Each camera's MAC address.** BombeCam learns it when the camera first streams to it and shows it in the Firewall tab. On the router, `bombecam-router clients` lists DHCP clients.
- **The Osaio region of your account.** The allowlist names one region's control and stream-setup servers: US (`mqtts02-us.osaio.net`, `wss-us.osaio.net`) or EU (`mqtts-eu.osaio.net`, `mqtts01-eu.osaio.net`, `wss-eu.osaio.net`). Pick it under **Connect router → Advanced → Osaio region**; US is the default. A camera enrolled in another region would lose its app controls while blocked, and BombeCam could not start new local streams.

## What the router does

For traffic from a blocked camera, matched by its MAC address:

| Destination | Result |
|---|---|
| Your own network (10.x, 172.16–31.x, 192.168.x), such as the BombeCam computer | Untouched |
| Osaio control, the region's hosts (US `mqtts02-us.osaio.net`; EU `mqtts-eu.osaio.net` and `mqtts01-eu.osaio.net`) TCP 8883 | Allowed, capped |
| Osaio stream setup, the region's host (US `wss-us.osaio.net`; EU `wss-eu.osaio.net`) TCP 443 | Allowed, capped (can be blocked too, see below) |
| DNS (port 53) and time sync (UDP 123), IPv4 | Allowed, capped |
| Everything else, including cloud clip and snapshot uploads, Osaio's relay servers and all IPv6 | Dropped |

- All allowed traffic of a camera shares one cap of 4 KB/s with an 8 KB burst. On fw3 routers without byte-rate limiting (`hashlimit`), the script falls back to 4 packets per second with a burst of 8; the result line shows `cap=bytes` or `cap=packets`.
- The router resolves the region's Osaio host names itself and re-checks them every 10 minutes.
- The rules only allow, drop and rate-limit. Nothing is redirected or rewritten, and the camera is not modified.
- The cap cannot inspect encrypted traffic, so small images or very low-rate video could fit inside allowed connections.

### Connections that were already open

When blocking turns on for a camera, the router clears the camera's entries in its connection-tracking table. Every connection the camera had open, such as a live view in the Osaio app, must start again and meet the rules, so none can carry on past them. The camera reconnects its allowed connections (control, DNS, time) by itself within seconds. Only the blocked cameras' own addresses are cleared; other devices are not touched.

The router uses the `conntrack` tool when it is installed and otherwise OpenWrt's `/proc/net/nf_conntrack` interface, where it only ever writes a single checked camera address. If neither is available, the result line says `cleared=no` and a warning explains that older connections end only when they close or the router restarts.

About two seconds after clearing, the router lists the camera's connections again. If a connection outside the allowed list is already back, traffic is getting past the rules, and the script ends with an error listing that connection instead of reporting the camera as blocked.

The same clearing happens when the router reloads the rules at boot, after an fw3 firewall reload, and when the rules had gone missing.

### The Osaio app on the same network

A phone or tablet on the same network as a blocked camera may still show its live view in the Osaio app. That video travels directly over your own network, not the internet, so the camera is still blocked from sending video out. Away from your network, the app's live view has to come through Osaio's relay servers, which a blocked camera can't reach. To see the block working, use the app on mobile data with Wi-Fi off.

## Using it from BombeCam (recommended)

1. Open the **Firewall** tab. Each camera shows its MAC address once it has streamed.
2. Press **Connect router**, enter the router address (GL.iNet default `192.168.8.1`) and the admin password, and press **Connect**.
3. Turn on a camera's switch, or **Block all cameras**. BombeCam asks before it clears the camera's open connections. The chip changes to **Blocked on router** within a few seconds.
4. The result box shows the firewall type and any warnings; **Router output** shows the router's full report.

**What connecting does.** BombeCam uses the password for that one SSH connection and never saves it. It installs the router script and adds its own SSH key to the router's `authorized_keys`, restricted like this:

```
command="/usr/sbin/bombecam-router gate",no-port-forwarding,no-agent-forwarding,no-X11-forwarding,no-pty ssh-ed25519 AAAA... bombecam-gateway
```

With that key BombeCam can only run the script's own commands: apply the camera list, status, connections, version and uninstall. It cannot open a shell, run anything else, or forward connections. The private half of the key is stored in BombeCam's encrypted profile.

**Identity check.** The first time it connects, BombeCam remembers the router's SSH host key. If the key changes later (for example after a router reset), BombeCam refuses to connect until you press **Forget this router** under **Advanced**, so another device can't impersonate the router to collect the password.

**Turning things off.** Turn a camera's switch off (for example to let it update its firmware) and it works normally again. **Block all cameras** off unblocks every camera; BombeCam stays connected so turning it back on is instant. **Disconnect** removes everything BombeCam put on the router, including its key. Removing a camera from BombeCam also removes it from the router.

**Updates.** When a new BombeCam version carries a newer router script, the Firewall tab shows **Update router**; enter the router password once to install it.

## Manual setup (without BombeCam)

The script is `router-setup/bombecam-router.sh`. Windows 10/11 and Linux include `ssh` and `scp`. Replace `192.168.8.1` with your router's address.

1. Copy the script to the router:

   ```
   scp -O router-setup/bombecam-router.sh root@192.168.8.1:/tmp/
   ```

   If `scp` says `unknown option -- O`, run it without `-O`. If it says `sftp-server: not found`, you left out `-O` (GL.iNet's SSH server has no SFTP). Without scp, from Command Prompt (not PowerShell, which changes line endings):

   ```
   type router-setup\bombecam-router.sh | ssh root@192.168.8.1 "cat > /tmp/bombecam-router.sh"
   ```

2. List the router's DHCP clients to find each camera's MAC:

   ```
   ssh root@192.168.8.1 "sh /tmp/bombecam-router.sh clients"
   ```

3. Turn blocking on, one `--camera` per camera:

   ```
   ssh root@192.168.8.1 "sh /tmp/bombecam-router.sh apply yes --camera 'Front Door=AA:BB:CC:DD:EE:FF'"
   ```

   The last line reads like:

   ```
   BOMBECAM_RESULT status=ok block=yes firewall=fw4 cameras=1 cap=bytes stream_setup=allowed cleared=yes connected=no version=1.0.0
   ```

   `cleared=none` means the camera wasn't on the network, so there was nothing to clear.

The script installs itself as `/usr/sbin/bombecam-router`, so afterwards:

```
ssh root@192.168.8.1 "bombecam-router status --names"   what is in force, counters, recent blocked attempts
ssh root@192.168.8.1 "bombecam-router connections"      what the cameras are connected to right now
ssh root@192.168.8.1 "bombecam-router render"           print the rules it would load, change nothing
ssh root@192.168.8.1 "bombecam-router apply yes --camera \"Front Door=AA:BB:CC:DD:EE:FF\" --region eu"
ssh root@192.168.8.1 "bombecam-router apply no"         stop blocking
ssh root@192.168.8.1 "bombecam-router uninstall"        remove everything, including BombeCam's key
```

### Blocking stream setup too

Stream setup is allowed so that BombeCam can start local streams through Osaio's signaling server. To block it as well (BombeCam may then be unable to start new streams):

```
ssh root@192.168.8.1 "bombecam-router apply yes --block-stream-setup"
```

Undo with `--allow-stream-setup`. BombeCam's own switches send its setting (stream setup allowed), so a later change from BombeCam turns it back on.

## What gets installed on the router

- `/usr/sbin/bombecam-router` (the script) and `/etc/bombecam/` (camera list, cached Osaio addresses, options).
- With **Connect router**: one line in `/etc/dropbear/authorized_keys` (or `/root/.ssh/authorized_keys` with OpenSSH): BombeCam's restricted key. Other keys in the file are left alone.
- `/etc/init.d/bombecam`, which reloads the rules at boot, right after the firewall.
- A cron line that re-checks the Osaio addresses every 10 minutes.
- **fw4:** a separate nftables table, `inet bombecam`. **fw3:** chains `bombecam_out`, `bombecam_cap` and `bombecam_v6`, plus a firewall include that restores them after a firewall reload.
- OpenWrt's firewall flow offloading is switched off while rules are in force, because offloaded connections skip the cap; the previous setting is restored when blocking is turned off.

With no camera blocked, the rules, boot entry and cron line are removed and offloading is restored. The script and key stay while BombeCam is connected; **Disconnect** or `bombecam-router uninstall` removes them too.

After a router firmware upgrade or reset, press **Connect router** again.

## Checking that it works

1. `bombecam-router status` shows `Block cloud video: YES   rules loaded: yes`, each camera with its MAC and IP, and addresses for `control` and `setup`. It should also show `Flow offloading: 0` and `Acceleration modules: none detected`.
2. With your phone on **mobile data (Wi-Fi off)**, live view in the Osaio app should fail to start for a blocked camera, while the app's controls (light, pan/tilt, night vision) keep working.
3. BombeCam's live view keeps working.
4. `bombecam-router connections` lists the cameras' current connections. Anything under **not allowed** means traffic got past the rules.
5. `bombecam-router status` counts dropped packets under **everything else**, and **Recent blocked attempts** lists where the camera tried to go.

**Reading the counters.** The allowed categories (control, stream_setup, dns, time) count packets before the cap check; only **allowed, under cap** traffic actually left the router.

## Troubleshooting

| Message | What to do |
|---|---|
| `could not resolve <region host> yet` | The router has no internet or DNS right now, or the wrong region is selected. Control stays blocked until the name resolves (retried every 10 minutes). |
| `no supported firewall found` | Not an OpenWrt/GL.iNet router, or its firewall is missing. |
| Acceleration warning | Turn off **Network Acceleration** (GL.iNet admin panel, usually under Network), then press **Update router**. Accelerated connections can skip the rules and the cap. |
| `access point, extender or WDS mode` warning | Switch the router to Router mode with the cameras on its Wi-Fi or LAN: bridged traffic never reaches the rules. |
| `... got past the rules after the cameras' connections were cleared` | The router forwards the camera's traffic past its firewall, for example through hardware acceleration or a bridged path. The error lists the connection. Check the two rows above, then **Check router**. |
| `cleared=no` warning | The router has neither `conntrack` nor OpenWrt's connection clearing. Connections opened before the block end when they close or when the router restarts. |
| BombeCam: "router rejected the username or password" | Use the admin-panel password; the SSH user is `root`. |
| BombeCam: "router's identity changed" | The router was reset or replaced, or another device answers at its address. If you know why, press **Forget this router** under **Advanced**. |
| BombeCam: "The router no longer accepts BombeCam's key" | The router was probably reset. Press **Connect router** again. |
| BombeCam: "each change will ask for the router password" | The router refuses key logins. Blocking still works, but each change asks for the password. |
| BombeCam: "... BombeCam rule(s) are still in its firewall" | Restart the router, which clears unsaved rules, then press **Check router**. |
| BombeCam: "older BombeCam router script" | Press **Update router** and enter the password once. |
| App controls stop working while blocked | Run `bombecam-router status`. If `over cap, dropped` keeps climbing, the cap is too tight for that camera; turn its switch off. |
| BombeCam can't start a stream | Check that `status` lists addresses for `setup` and that stream setup isn't blocked. |

## Without a suitable router

The page **Other ways to block camera traffic** (linked from the Firewall tab, `/blocking-options.html`) compares the alternatives: a travel router, a full internet block, a guest network, your own firewall rules, a Linux router ([linux-router.md](linux-router.md)) and DNS filtering.
