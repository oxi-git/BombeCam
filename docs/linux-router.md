# Block cloud video on a Linux router

If your cameras route through a Linux machine (for example a Raspberry Pi or a small PC acting as the camera network's router), `bombecam-net` applies the same Block cloud video rules as the OpenWrt script, using nftables. OpenWrt and GL.iNet routers don't need this: BombeCam's Firewall tab sets them up over SSH (see [block-cloud-video.md](block-cloud-video.md)).

## Requirements

- Linux with nftables (`nft`) and iproute2, run as root (or with `CAP_NET_ADMIN`).
- The machine forwards the cameras' traffic (IP forwarding on, cameras on an interface of this machine).
- Each camera's MAC address.

## bombecam-net

```
bombecam-net block-cloud-video yes --camera "Front Door=AA:BB:CC:DD:EE:FF" [--camera ...]
bombecam-net block-cloud-video no
bombecam-net status
bombecam-net rules render --block yes --camera "Front Door=AA:BB:CC:DD:EE:FF"   print the ruleset, change nothing
bombecam-net daemon          keep the setting applied, re-resolve Osaio's hosts, serve the local API
bombecam-net router-script   print the OpenWrt / GL.iNet router script
```

Flags:

```
--camera              "Name=MAC" (repeatable), or CAMERA_MAC="Name=MAC,Name=MAC"
--block-stream-setup  also block Osaio's stream-setup server (BombeCam may then be unable to start local streams)
--region              Osaio region for the allowlist: "us" (default) or "eu" (also BOMBECAM_REGION)
--state-dir           persistent state (default /var/lib/bombecam)
--masquerade          add return-path NAT, only for a machine that routes the camera subnet without NAT of its own
--api-host/--api-port local REST API (default 127.0.0.1:8653)
```

The region selects the Osaio endpoints the allowlist trusts: US (`mqtts02-us.osaio.net`, `wss-us.osaio.net`) or EU (`mqtts-eu.osaio.net`, `mqtts01-eu.osaio.net`, `wss-eu.osaio.net`). Use the region the cameras' Osaio account belongs to; the wrong one blocks their control channel.

The rules are those described in [block-cloud-video.md](block-cloud-video.md#what-the-router-does): traffic to private networks untouched; Osaio control, stream setup, DNS and time sync allowed under a shared 4 KB/s cap per camera; everything else, including IPv6, dropped. They are loaded in one nftables transaction and read back from the kernel.

The daemon's local API (`/api/v1/net/status`, `/api/v1/net/block-cloud-video`, `/api/v1/net/verdict`) listens on `127.0.0.1:8653`. Set `BOMBECAM_NET_API_KEY` to require a bearer token for changes.

## Docker

`deploy/docker-compose.yml` has an `isolation` profile that runs `bombecam-net daemon` with host networking and `NET_ADMIN`:

```
cd deploy
cp .env.example .env      # set CAMERA_MAC="Front Door=aa:bb:cc:dd:ee:ff,...", BOMBECAM_REGION=us|eu, and the interface settings if needed
docker compose --profile isolation up -d
```

## Boot guard (optional)

A boot guard installs a forward drop on the camera interface before the network comes up, so the cameras have no internet path until the daemon loads the saved rules (which replace the guard in one transaction).

```
sudo CAMERA_VLAN_IF=<camera interface> sh deploy/scripts/install-guard.sh
```

- `CAMERA_VLAN_IF` must name an existing interface; the guard never guesses one. It is saved in `/etc/default/bombecam`.
- The installer adds `bombecam-guard.service` (required by `network-pre.target`, systemd-networkd, NetworkManager and networking) and an `if-pre-up.d` hook for that interface. If another service manages networking, give it `Requires=bombecam-guard.service` and `After=bombecam-guard.service`.
- The guard covers the configured interface only, IPv4 and IPv6. It cannot account for other interfaces or alternate routes to the cameras.

## Verifying with bombecam-verify

`bombecam-verify` checks a Linux router running `bombecam-net` by measuring real traffic: it confirms the router is on the camera's path, runs a short control leg in which a separate test device (the "canary", never a real camera) is allowed out, then checks that blocked destinations are dropped and allowed ones pass under the cap. The canary's exception is time-limited by the kernel, so the real cameras stay blocked even if the verifier is interrupted.

```
bombecam-verify -mac <camera MAC> -ip <camera IP> -iface <camera interface> \
                -canary <canary IP> -canary-mac <canary MAC> [-canary-netns canary]
```

Run it once after setup, not on a timer. `-verdict-file` stores the result for the daemon's `/api/v1/net/verdict`, and `-metrics-port` serves Prometheus metrics (`bombecam_*`).
