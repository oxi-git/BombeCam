#!/bin/sh
# bombecam-router: "Block cloud video?" for OpenWrt and GL.iNet routers.
#
# Run as root on the router your cameras connect to.
#
#   sh bombecam-router.sh apply yes --camera "Front Door=AA:BB:CC:DD:EE:FF" [--camera ...]
#   sh bombecam-router.sh apply no
#
# After "apply yes" the script installs itself as /usr/sbin/bombecam-router:
#
#   bombecam-router status [--names]   what is in force, counters, recent blocks
#   bombecam-router connections        what the cameras are connected to right now
#   bombecam-router clients            DHCP clients, to find a camera's MAC
#   bombecam-router render             print the rules it would load, change nothing
#   bombecam-router uninstall          remove everything, including BombeCam's key
#
# BombeCam's Connect button runs "connect --key ..." once with the router
# password. That installs a key which can only run this script's "gate"
# command (apply, status, connections, version, uninstall), so BombeCam can
# turn blocking on and off per camera without the password or a shell.
#
# Internal commands: load (boot and firewall reloads), refresh (cron, re-resolves
# the two allowed Osaio host names), gate (the key's forced command).
#
# What "yes" does, per camera MAC, for traffic the router forwards:
#   - traffic to your own network (private IPv4 ranges) is left alone
#   - allowed, and sharing one bandwidth cap of about 4 KB/s per camera:
#       the Osaio control connection   the region's control hosts  TCP 8883
#       the stream-setup handshake     the region's setup host     TCP 443
#       DNS (UDP/TCP 53) and time sync (UDP 123), IPv4 only
#     The hosts are selected by region (us or eu), so the allowlist matches
#     the Osaio cloud the camera's account belongs to.
#   - everything else from the camera is dropped, IPv4 and IPv6
#   - when the rules go into force, the router's tracked connections of each
#     blocked camera are cleared, so a connection opened before the block
#     cannot carry on past it; the camera reconnects through the rules
# Only allow, drop and rate-limit rules are used. Nothing is redirected or
# rewritten, and the camera itself is not modified. "no" removes all of it.

set -u
LC_ALL=C
export LC_ALL

VERSION=1.1.0

# --- policy (must match pkg/policy; a Go test checks) ------------------------
# The default region, used when neither --region nor a saved choice is present.
REGION=us
# Osaio endpoints per region. A region may publish more than one control
# name; every one of them is allowed.
US_CONTROL_HOSTS="mqtts02-us.osaio.net"
US_SETUP_HOST=wss-us.osaio.net
EU_CONTROL_HOSTS="mqtts-eu.osaio.net mqtts01-eu.osaio.net"
EU_SETUP_HOST=wss-eu.osaio.net
CONTROL_PORT=8883
SETUP_PORT=443
CAP_BPS=4096
CAP_BURST=8192
LOCAL_NETS="10.0.0.0/8 172.16.0.0/12 192.168.0.0/16"
RESOLVERS="8.8.8.8 8.8.4.4 system"
KEEP_DAYS=7

# --- paths (BC_ROOT is only for tests) ---------------------------------------
ROOT="${BC_ROOT:-}"
BC_DIR="$ROOT/etc/bombecam"
CAMS="$BC_DIR/cameras"
OPTS="$BC_DIR/options"
IPS="$BC_DIR/ips"
FW3_INC="$BC_DIR/fw3-include.sh"
BIN="$ROOT/usr/sbin/bombecam-router"
INIT="$ROOT/etc/init.d/bombecam"
CRONTAB="$ROOT/etc/crontabs/root"
LEASES="$ROOT/tmp/dhcp.leases"
LOADED="$ROOT/tmp/bombecam.loaded"
CT_FILE="${BC_CONNTRACK:-/proc/net/nf_conntrack}"
KEYMARK="$BC_DIR/connected"
TABLE=bombecam

say() { printf '%s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() {
	printf 'ERROR: %s\n' "$*" >&2
	say "BOMBECAM_RESULT status=error"
	exit 1
}

# run executes a state-changing command, or prints it when BC_DRYRUN=1.
run() {
	if [ "${BC_DRYRUN:-0}" = 1 ]; then
		say "[dry-run] $*"
		return 0
	fi
	"$@"
}

have() { command -v "$1" >/dev/null 2>&1; }

detect_fw() {
	if [ -n "${BC_FW:-}" ]; then
		say "$BC_FW"
	elif [ -x /sbin/fw4 ] && have nft; then
		say fw4
	elif [ -x /sbin/fw3 ] && have iptables; then
		say fw3
	else
		say none
	fi
}

# --- camera list ---------------------------------------------------------------

norm_mac() {
	m=$(printf '%s' "$1" | tr 'A-F-' 'a-f:')
	printf '%s' "$m" | grep -Eq '^([0-9a-f]{2}:){5}[0-9a-f]{2}$' || return 1
	[ "$m" = "00:00:00:00:00:00" ] && return 1
	first=${m%%:*}
	[ $((0x$first & 1)) -eq 0 ] || return 1
	printf '%s' "$m"
}

clean_name() {
	n=$(printf '%s' "$1" | sed 's/^ *//' | tr -c 'A-Za-z0-9 ._-' '_' | cut -c1-40 | sed 's/ *$//')
	[ -n "$n" ] || n=camera
	printf '%s' "$n"
}

# cam_macs prints the configured MACs, sorted.
cam_macs() { [ -s "$CAMS" ] && cut -f1 "$CAMS" | sort -u; }

cam_name() { awk -F '\t' -v m="$1" '$1 == m { print $2; exit }' "$CAMS" 2>/dev/null; }

opt() {
	[ -f "$OPTS" ] || return 0
	sed -n "s/^$1=//p" "$OPTS" | tail -n 1
}

set_opt() {
	mkdir -p "$BC_DIR"
	touch "$OPTS"
	grep -v "^$1=" "$OPTS" >"$OPTS.tmp" 2>/dev/null
	printf '%s=%s\n' "$1" "$2" >>"$OPTS.tmp"
	mv "$OPTS.tmp" "$OPTS"
}

stream_setup_allowed() { [ "$(opt BLOCK_STREAM_SETUP)" != 1 ]; }

# --- BombeCam's key (instant per-camera updates) -----------------------------

is_connected() { [ -f "$KEYMARK" ]; }
yes_no() { if "$@"; then echo yes; else echo no; fi; }

# auth_key_files prints the authorized_keys file(s) of the router's SSH server.
auth_key_files() {
	if [ -n "${BC_AUTH_KEYS:-}" ]; then
		say "$BC_AUTH_KEYS"
		return 0
	fi
	found=1
	if [ -d "$ROOT/etc/dropbear" ] || have dropbear; then
		say "$ROOT/etc/dropbear/authorized_keys"
		found=0
	fi
	if [ -x /usr/sbin/sshd ]; then
		say "$ROOT/root/.ssh/authorized_keys"
		found=0
	fi
	return $found
}

# clean_key accepts "TYPE BASE64 [comment]" and prints "TYPE BASE64".
clean_key() {
	case "$1" in *"
"*) return 1 ;; esac
	printf '%s\n' "$1" | grep -Eq '^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)) [A-Za-z0-9+/]{40,}={0,3}( [ -~]{0,200})?$' || return 1
	printf '%s\n' "$1" | awk '{ print $1 " " $2 }'
}

# gate_command is the forced command for BombeCam's key. (With BC_ROOT set,
# i.e. in tests, it carries the test environment along.)
gate_command() {
	if [ -n "$ROOT" ]; then
		printf 'env BC_ROOT=%s BC_DRYRUN=%s BC_FW=%s BC_AUTH_KEYS=%s BC_CAP_MODE=%s BC_IPT_LOG=%s %s gate' \
			"$ROOT" "${BC_DRYRUN:-0}" "${BC_FW:-}" "${BC_AUTH_KEYS:-}" "${BC_CAP_MODE:-}" "${BC_IPT_LOG:-}" "$BIN"
	else
		printf '%s gate' "$BIN"
	fi
}

# write_keys FILE KEYS replaces BombeCam's lines in FILE with KEYS (one
# "TYPE BASE64" per line), each restricted to the gate command. Other keys in
# the file are left alone.
write_keys() {
	f=$1
	d=$(dirname "$f")
	[ -d "$d" ] || { mkdir -p "$d" && chmod 700 "$d"; }
	[ -f "$f" ] || { : >"$f" && chmod 600 "$f"; }
	grep -v -e 'bombecam-router gate' "$f" >"$f.bctmp" 2>/dev/null
	gc=$(gate_command)
	printf '%s\n' "$2" | while IFS= read -r k; do
		[ -n "$k" ] || continue
		printf 'command="%s",no-port-forwarding,no-agent-forwarding,no-X11-forwarding,no-pty %s bombecam-gateway\n' "$gc" "$k"
	done >>"$f.bctmp"
	chmod 600 "$f.bctmp"
	mv "$f.bctmp" "$f"
}

remove_keys() {
	for f in $(auth_key_files 2>/dev/null); do
		[ -f "$f" ] && grep -q 'bombecam-router gate' "$f" || continue
		grep -v -e 'bombecam-router gate' "$f" >"$f.bctmp"
		chmod 600 "$f.bctmp"
		mv "$f.bctmp" "$f"
	done
}

install_self() {
	src=$(self_path)
	if [ -n "$src" ] && [ "$src" != "$BIN" ]; then
		mkdir -p "$(dirname "$BIN")"
		cp "$src" "$BIN.tmp" && chmod 755 "$BIN.tmp" && mv "$BIN.tmp" "$BIN"
	fi
	[ -x "$BIN" ] || [ "${BC_DRYRUN:-0}" = 1 ] || die "could not install $BIN (run this script from a file, not a pipe)"
}

# bridge_mode_warning: in access point / extender / WDS mode the router
# bridges camera traffic instead of routing it, and forward rules never see it.
bridge_mode_warning() {
	have ip || return 0
	dev=$(ip route show default 2>/dev/null | awk '{ for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit } }')
	case "$dev" in
	br-lan | br-lan.*)
		warn "this router reaches the internet through its LAN bridge ($dev), which usually means access point, extender or WDS mode. Bridged traffic is not routed, so these rules never see it. Switch the router to Router mode, with the cameras on its Wi-Fi or LAN."
		;;
	esac
}

# --- host name resolution ------------------------------------------------------

public_v4() {
	grep -Eo '([0-9]{1,3}\.){3}[0-9]{1,3}' | awk -F. '
		$1 > 255 || $2 > 255 || $3 > 255 || $4 > 255 { next }
		$1 == 0 || $1 == 10 || $1 == 127 || $1 >= 224 { next }
		$1 == 172 && $2 >= 16 && $2 <= 31 { next }
		$1 == 192 && $2 == 168 { next }
		$1 == 169 && $2 == 254 { next }
		{ print }'
}

resolve4() {
	server=$2
	[ "$server" = system ] && server=""
	if have timeout; then
		timeout 8 nslookup "$1" $server 2>/dev/null
	else
		nslookup "$1" $server 2>/dev/null
	fi | awk '/^Name:/ { f = 1; next } f' | public_v4
}

# effective_region prints the region in force: the saved choice, else the
# built-in default.
effective_region() {
	r=$(opt REGION)
	[ -n "$r" ] || r=$REGION
	say "$r"
}

# control_hosts prints the region's control host names, space separated.
control_hosts() {
	case "$(effective_region)" in
	eu) say "$EU_CONTROL_HOSTS" ;;
	*) say "$US_CONTROL_HOSTS" ;;
	esac
}

# setup_host prints the region's stream-setup host name.
setup_host() {
	case "$(effective_region)" in
	eu) say "$EU_SETUP_HOST" ;;
	*) say "$US_SETUP_HOST" ;;
	esac
}

allowed_hosts() {
	for h in $(control_hosts); do say "$h"; done
	stream_setup_allowed && say "$(setup_host)"
}

# host_ips HOST prints the cached IPv4 addresses for HOST, sorted.
host_ips() { [ -f "$IPS" ] && awk -v h="$1" '$1 == h { print $2 }' "$IPS" | sort -u; }

today() { echo $(($(date +%s) / 86400)); }

# refresh_ips re-resolves the allowed hosts. Returns 0 when the address set
# changed (rules need reloading), 1 otherwise.
refresh_ips() {
	mkdir -p "$BC_DIR"
	day=$(today)
	fresh="$BC_DIR/.ips.fresh"
	: >"$fresh"
	for h in $(allowed_hosts); do
		for s in $RESOLVERS; do
			for ip in $(resolve4 "$h" "$s"); do
				say "$h $ip $day" >>"$fresh"
			done
		done
	done
	[ -f "$IPS" ] || : >"$IPS"
	# FILENAME test, not FNR==NR: the fresh file is empty when DNS fails.
	awk -v day="$day" -v keep="$KEEP_DAYS" -v f1="$fresh" -v hosts=" $(allowed_hosts | tr '\n' ' ')" '
		FILENAME == f1 { seen[$1 " " $2] = $3; next }
		{ old[$1 " " $2] = $3 }
		END {
			for (k in old) {
				split(k, a, " ")
				if (index(hosts, " " a[1] " ") == 0) continue
				d = (k in seen) ? seen[k] : old[k]
				if (d + keep >= day) out[k] = d
			}
			for (k in seen) out[k] = seen[k]
			for (k in out) print k, out[k]
		}' "$fresh" "$IPS" | sort >"$IPS.new"
	rm -f "$fresh"
	if [ "$(cut -d' ' -f1,2 "$IPS.new")" = "$(sort "$IPS" | cut -d' ' -f1,2)" ]; then
		# Same addresses. Rewrite only if a last-seen day moved, to spare flash.
		if [ "$(cat "$IPS.new")" != "$(sort "$IPS")" ]; then mv "$IPS.new" "$IPS"; else rm -f "$IPS.new"; fi
		return 1
	fi
	mv "$IPS.new" "$IPS"
	return 0
}

# --- rule generation: fw4 / nftables ---------------------------------------------

join_csv() { tr '\n' ' ' | sed 's/ *$//; s/ /, /g'; }

gen_nft() {
	macs=$(cam_macs | join_csv)
	[ -n "$macs" ] || die "no cameras configured"
	say "table inet $TABLE"
	say "delete table inet $TABLE"
	say ""
	say "table inet $TABLE {"
	printf '\tset cameras {\n\t\ttype ether_addr\n\t\telements = { %s }\n\t}\n' "$macs"
	gen_nft_set control_v4 $(control_hosts)
	stream_setup_allowed && gen_nft_set stream_setup_v4 $(setup_host)
	printf '\n\tchain forward {\n'
	printf '\t\ttype filter hook forward priority filter - 5; policy accept;\n'
	printf '\t\tether saddr @cameras jump camera_out\n\t}\n\n'
	printf '\tchain camera_out {\n'
	printf '\t\tip daddr { %s } return comment "your network: untouched"\n' "$(echo $LOCAL_NETS | tr ' ' '\n' | join_csv)"
	printf '\t\tip daddr @control_v4 tcp dport %s counter goto capped comment "control"\n' "$CONTROL_PORT"
	stream_setup_allowed &&
		printf '\t\tip daddr @stream_setup_v4 tcp dport %s counter goto capped comment "stream_setup"\n' "$SETUP_PORT"
	printf '\t\tmeta nfproto ipv4 udp dport 53 counter goto capped comment "dns"\n'
	printf '\t\tmeta nfproto ipv4 tcp dport 53 counter goto capped comment "dns"\n'
	printf '\t\tmeta nfproto ipv4 udp dport 123 counter goto capped comment "time"\n'
	printf '\t\tlimit rate 10/minute burst 20 packets log prefix "bombecam-blocked: " level info\n'
	printf '\t\tcounter drop comment "everything else, IPv4 and IPv6"\n\t}\n\n'
	printf '\tchain capped {\n'
	for m in $(cam_macs); do
		printf '\t\tether saddr %s limit rate over %s bytes/second burst %s bytes counter drop comment "cap: %s"\n' \
			"$m" "$CAP_BPS" "$CAP_BURST" "$(cam_name "$m")"
	done
	printf '\t\tcounter accept comment "allowed, under cap"\n\t}\n}\n'
}

# gen_nft_set NAME HOST... writes an nft set holding the cached addresses of
# every HOST.
gen_nft_set() {
	name=$1
	shift
	ips=$(for h in "$@"; do host_ips "$h"; done | sort -u | join_csv)
	printf '\tset %s {\n\t\ttype ipv4_addr\n' "$name"
	[ -n "$ips" ] && printf '\t\telements = { %s }\n' "$ips"
	printf '\t}\n'
}

# --- rule generation: fw3 / iptables ------------------------------------------------

ipt_can() {
	iptables -N bombecam_probe 2>/dev/null
	iptables -F bombecam_probe 2>/dev/null
	if iptables -A bombecam_probe "$@" 2>/dev/null; then r=0; else r=1; fi
	iptables -F bombecam_probe 2>/dev/null
	iptables -X bombecam_probe 2>/dev/null
	return $r
}

# CAP_MODE: "bytes" (hashlimit, exact) or "packets" (limit, fallback).
fw3_caps() {
	if [ -n "${BC_CAP_MODE:-}" ]; then
		CAP_MODE=$BC_CAP_MODE
		IPT_LOG=${BC_IPT_LOG:-1}
		return
	fi
	CAP_MODE=packets
	ipt_can -m hashlimit --hashlimit-above "${CAP_BPS}b/s" --hashlimit-burst "${CAP_BURST}b" \
		--hashlimit-name bc_probe -j DROP && CAP_MODE=bytes
	IPT_LOG=0
	ipt_can -m limit --limit 10/min -j LOG --log-prefix "bombecam-probe: " && IPT_LOG=1
}

gen_ipt() {
	say "*filter"
	say ":bombecam_out - [0:0]"
	say ":bombecam_cap - [0:0]"
	for n in $LOCAL_NETS; do
		say "-A bombecam_out -d $n -j RETURN"
	done
	for ip in $(for h in $(control_hosts); do host_ips "$h"; done | sort -u); do
		say "-A bombecam_out -d $ip/32 -p tcp -m tcp --dport $CONTROL_PORT -j bombecam_cap"
	done
	if stream_setup_allowed; then
		for ip in $(host_ips "$(setup_host)"); do
			say "-A bombecam_out -d $ip/32 -p tcp -m tcp --dport $SETUP_PORT -j bombecam_cap"
		done
	fi
	say "-A bombecam_out -p udp -m udp --dport 53 -j bombecam_cap"
	say "-A bombecam_out -p tcp -m tcp --dport 53 -j bombecam_cap"
	say "-A bombecam_out -p udp -m udp --dport 123 -j bombecam_cap"
	[ "$IPT_LOG" = 1 ] &&
		say '-A bombecam_out -m limit --limit 10/min --limit-burst 20 -j LOG --log-prefix "bombecam-blocked: " --log-level 6'
	say "-A bombecam_out -j DROP"
	for m in $(cam_macs); do
		if [ "$CAP_MODE" = bytes ]; then
			say "-A bombecam_cap -m mac --mac-source $m -m hashlimit --hashlimit-above ${CAP_BPS}b/s --hashlimit-burst ${CAP_BURST}b --hashlimit-name bc_$(echo "$m" | tr -d :) -j DROP"
		else
			# Fallback without hashlimit: at most 4 packets/s (burst 8), which
			# bounds a camera to roughly 6 KB/s even with full-size packets.
			say "-A bombecam_cap -m mac --mac-source $m -m limit --limit 4/sec --limit-burst 8 -j ACCEPT"
			say "-A bombecam_cap -m mac --mac-source $m -j DROP"
		fi
	done
	if [ "$CAP_MODE" = bytes ]; then say "-A bombecam_cap -j ACCEPT"; else say "-A bombecam_cap -j DROP"; fi
	say "COMMIT"
}

# ensure_jumps TOOL CHAIN: one jump per camera at the top of FORWARD, and no
# jumps for MACs that are no longer configured. New jumps go in before stale
# ones are removed, so a camera is never briefly unfiltered.
ensure_jumps() {
	tool=$1
	chain=$2
	for m in $(cam_macs); do
		$tool -C FORWARD -m mac --mac-source "$m" -j "$chain" 2>/dev/null ||
			run $tool -I FORWARD 1 -m mac --mac-source "$m" -j "$chain"
	done
	$tool -S FORWARD 2>/dev/null | grep -- "-j $chain\$" |
		sed -n 's/.*--mac-source \([0-9A-Fa-f:]*\).*/\1/p' | tr 'A-F' 'a-f' | sort -u |
		while read -r m; do
			cam_macs | grep -qx "$m" || run $tool -D FORWARD -m mac --mac-source "$m" -j "$chain"
		done
}

remove_jumps() {
	tool=$1
	chain=$2
	# Delete every FORWARD rule that jumps to the chain, whatever its match
	# (the -S line is turned into the matching -D). Bounded, in case a rule
	# cannot be deleted.
	i=0
	while [ $i -lt 64 ]; do
		line=$($tool -S FORWARD 2>/dev/null | grep -- "-j $chain\$" | head -n 1)
		[ -n "$line" ] || break
		eval "$tool -D ${line#-A }" 2>/dev/null || break
		i=$((i + 1))
	done
}

# firewall_residue prints how many BombeCam rules, chains and tables are in
# the live firewall, in every firewall tool present (0 when nothing is left).
firewall_residue() {
	if [ "${BC_DRYRUN:-0}" = 1 ]; then
		say 0
		return 0
	fi
	n=0
	if have iptables; then
		c=$(iptables -S 2>/dev/null | grep -c 'bombecam_')
		n=$((n + c))
	fi
	if have ip6tables; then
		c=$(ip6tables -S 2>/dev/null | grep -c 'bombecam_')
		n=$((n + c))
	fi
	if have nft; then
		c=$(nft list tables 2>/dev/null | grep -c " $TABLE\$")
		n=$((n + c))
	fi
	say "$n"
}

# --- load / unload ---------------------------------------------------------------------

load_rules() {
	[ -s "$CAMS" ] || { say "No cameras configured; nothing to load."; return 0; }
	fw=$(detect_fw)
	case "$fw" in
	fw4)
		if [ "${BC_DRYRUN:-0}" = 1 ]; then
			gen_nft
			return 0
		fi
		gen_nft | nft -f - || die "nft rejected the ruleset"
		nft list chain inet $TABLE camera_out >/dev/null 2>&1 || die "rules did not load (readback failed)"
		;;
	fw3)
		fw3_caps
		if [ "${BC_DRYRUN:-0}" = 1 ]; then
			gen_ipt
			return 0
		fi
		gen_ipt | iptables-restore -n || die "iptables-restore rejected the rules"
		ensure_jumps iptables bombecam_out
		if have ip6tables; then
			ip6tables -N bombecam_v6 2>/dev/null
			ip6tables -F bombecam_v6
			[ "$IPT_LOG" = 1 ] && ip6tables -A bombecam_v6 -m limit --limit 10/min --limit-burst 20 \
				-j LOG --log-prefix "bombecam-blocked: " --log-level 6 2>/dev/null
			ip6tables -A bombecam_v6 -j DROP
			ensure_jumps ip6tables bombecam_v6
		fi
		iptables -S bombecam_out >/dev/null 2>&1 || die "rules did not load (readback failed)"
		;;
	*) die "no supported firewall found (need OpenWrt fw4/nftables or fw3/iptables)" ;;
	esac
}

# mark_loaded notes the router uptime when the rules went into force (in RAM,
# so a reboot clears it). "connections" uses it to tell whether a listed
# connection could predate the rules. "force" is for apply; boot and firewall
# reloads only record the first load since boot.
mark_loaded() {
	[ "${BC_DRYRUN:-0}" = 1 ] && return 0
	[ "${1:-}" = force ] || [ ! -s "$LOADED" ] || return 0
	mkdir -p "$(dirname "$LOADED")"
	cut -d. -f1 /proc/uptime >"$LOADED" 2>/dev/null
}

unload_rules() {
	fw=$(detect_fw)
	if [ "${BC_DRYRUN:-0}" = 1 ]; then
		case "$fw" in
		fw4) run nft delete table inet $TABLE ;;
		fw3) say "[dry-run] remove bombecam iptables chains" ;;
		esac
		return 0
	fi
	# Every firewall tool present is cleaned, not only the one detected now,
	# so nothing BombeCam loaded can be left behind.
	have nft && nft delete table inet $TABLE 2>/dev/null
	if have iptables; then
		remove_jumps iptables bombecam_out
		iptables -F bombecam_out 2>/dev/null
		iptables -F bombecam_cap 2>/dev/null
		iptables -X bombecam_out 2>/dev/null
		iptables -X bombecam_cap 2>/dev/null
	fi
	if have ip6tables; then
		remove_jumps ip6tables bombecam_v6
		ip6tables -F bombecam_v6 2>/dev/null
		ip6tables -X bombecam_v6 2>/dev/null
	fi
	return 0
}

# unload_verified removes the rules and checks the live firewall afterwards,
# trying once more if anything is left. It prints the number left (0 = clean).
unload_verified() {
	unload_rules >&2
	left=$(firewall_residue)
	if [ "$left" != 0 ]; then
		sleep 1
		unload_rules >&2
		left=$(firewall_residue)
	fi
	say "$left"
}

# --- persistence ---------------------------------------------------------------------------

install_persistence() {
	fw=$1
	mkdir -p "$(dirname "$INIT")" "$(dirname "$CRONTAB")"
	cat >"$INIT" <<'EOF'
#!/bin/sh /etc/rc.common
# BombeCam: re-load the "Block cloud video" rules at boot (right after the
# firewall). Remove with: bombecam-router apply no
START=20
start() {
	[ -x /usr/sbin/bombecam-router ] && /usr/sbin/bombecam-router load
}
stop() {
	:
}
EOF
	chmod 755 "$INIT"
	if [ -f /etc/rc.common ] || [ "${BC_DRYRUN:-0}" = 1 ]; then run "$INIT" enable; fi

	touch "$CRONTAB"
	grep -v 'bombecam-router' "$CRONTAB" >"$CRONTAB.tmp" 2>/dev/null
	say '*/10 * * * * /usr/sbin/bombecam-router refresh >/dev/null 2>&1' >>"$CRONTAB.tmp"
	mv "$CRONTAB.tmp" "$CRONTAB"
	if [ -x "$ROOT/etc/init.d/cron" ] || [ "${BC_DRYRUN:-0}" = 1 ]; then
		run /etc/init.d/cron enable
		run /etc/init.d/cron restart
	fi

	if [ "$fw" = fw3 ]; then
		printf '#!/bin/sh\n# BombeCam: fw3 flushes FORWARD on reload; put our rules back.\n/usr/sbin/bombecam-router load >/dev/null 2>&1\n' >"$FW3_INC"
		chmod 755 "$FW3_INC"
		run uci -q delete firewall.bombecam
		run uci set firewall.bombecam=include
		run uci set firewall.bombecam.type=script
		run uci set firewall.bombecam.path=/etc/bombecam/fw3-include.sh
		run uci set firewall.bombecam.reload=1
		run uci commit firewall
	fi
}

remove_persistence() {
	if [ -x "$INIT" ]; then
		[ -f /etc/rc.common ] && run "$INIT" disable
		rm -f "$INIT"
	fi
	if [ -f "$CRONTAB" ] && grep -q 'bombecam-router' "$CRONTAB"; then
		grep -v 'bombecam-router' "$CRONTAB" >"$CRONTAB.tmp"
		mv "$CRONTAB.tmp" "$CRONTAB"
		[ -x "$ROOT/etc/init.d/cron" ] && run /etc/init.d/cron restart
	fi
	if have uci && uci -q get firewall.bombecam >/dev/null 2>&1; then
		run uci delete firewall.bombecam
		run uci commit firewall
	fi
}

# With flow offloading on, the firewall hands established connections to a
# flowtable, and later packets skip the forward chain, so an allowed
# connection would escape the cap after its first few packets. fw4 offloads
# before any custom rule runs; fw3 offloads when a reply packet (which the
# camera-MAC rules can't match) reaches its FLOWOFFLOAD rule. So offloading is
# switched off on both while BombeCam's rules are in force, and the previous
# setting is restored on "apply no". Blocked destinations never get that far:
# their first packet is dropped, so no connection exists to offload.
disable_offload() {
	have uci || return 0
	cur=$(uci -q get firewall.@defaults[0].flow_offloading)
	curhw=$(uci -q get firewall.@defaults[0].flow_offloading_hw)
	if [ -z "$(opt PREV_OFFLOAD)" ]; then
		set_opt PREV_OFFLOAD "${cur:-0}"
		set_opt PREV_OFFLOAD_HW "${curhw:-0}"
	fi
	if [ "${cur:-0}" = 1 ] || [ "${curhw:-0}" = 1 ]; then
		say "Turning off firewall flow offloading so the bandwidth cap sees every packet."
		run uci set firewall.@defaults[0].flow_offloading=0
		run uci set firewall.@defaults[0].flow_offloading_hw=0
		run uci commit firewall
		run /etc/init.d/firewall reload >/dev/null 2>&1
	fi
}

restore_offload() {
	have uci || return 0
	prev=$(opt PREV_OFFLOAD)
	prevhw=$(opt PREV_OFFLOAD_HW)
	if [ "$prev" = 1 ] || [ "$prevhw" = 1 ]; then
		say "Restoring the previous flow offloading setting."
		run uci set firewall.@defaults[0].flow_offloading="$prev"
		run uci set firewall.@defaults[0].flow_offloading_hw="$prevhw"
		run uci commit firewall
		run /etc/init.d/firewall reload >/dev/null 2>&1
	fi
}

accel_modules() {
	[ -r /proc/modules ] || return 0
	grep -oE '^(shortcut_fe[a-z_]*|fast_classifier|qca_nss_ecm|ecm|mtkhnat|mtk_hnat|hw_nat|hnat)( |$)' /proc/modules |
		tr -d ' ' | tr '\n' ' ' | sed 's/ *$//'
}

# --- commands -------------------------------------------------------------------------------

parse_cameras() {
	out=$1
	: >"$out"
	shift
	while [ $# -gt 0 ]; do
		case "$1" in
		--camera)
			[ $# -ge 2 ] || die "--camera needs NAME=MAC"
			spec=$2
			shift 2
			;;
		--camera=*)
			spec=${1#--camera=}
			shift
			;;
		--block-stream-setup) BSS=1; shift; continue ;;
		--allow-stream-setup) BSS=0; shift; continue ;;
		--region)
			[ $# -ge 2 ] || die "--region needs us or eu"
			RGN=$2
			shift 2
			continue
			;;
		--region=*)
			RGN=${1#--region=}
			shift
			continue
			;;
		*) die "unknown option: $1" ;;
		esac
		case "$spec" in *=*) ;; *) die "camera must be NAME=MAC, got: $spec" ;; esac
		name=$(clean_name "${spec%=*}")
		mac=$(norm_mac "${spec##*=}") || die "\"${spec##*=}\" is not a camera MAC address (use the MAC shown in BombeCam or by: bombecam-router clients)"
		grep -q "^$mac	" "$out" 2>/dev/null && die "MAC $mac listed twice"
		printf '%s\t%s\n' "$mac" "$name" >>"$out"
	done
	case "${RGN:-us}" in
	us | eu) ;;
	*) die "unknown region: ${RGN} (want us or eu)" ;;
	esac
}

self_path() {
	case "$0" in
	*/*) [ -f "$0" ] && { say "$0"; return; } ;;
	*) [ -f "./$0" ] && { say "./$0"; return; } ;;
	esac
	say ""
}

cmd_apply() {
	mode=${1:-}
	[ $# -gt 0 ] && shift
	case "$mode" in yes | no) ;; *) die "usage: apply yes|no [--camera NAME=MAC ...] [--block-stream-setup]" ;; esac
	[ "${BC_DRYRUN:-0}" = 1 ] || [ "$(id -u)" = 0 ] || die "run this as root on the router"
	fw=$(detect_fw)

	if [ "$mode" = no ]; then
		remove_persistence
		left=$(unload_verified)
		restore_offload
		rm -f "$LOADED"
		if [ "$left" != 0 ]; then
			rm -f "$CAMS" "$IPS" "$OPTS"
			say "BombeCam could not remove all of its rules: $left are still in the firewall. Restart the router, which clears them (nothing reloads them any more)."
			say "BOMBECAM_RESULT status=error block=no firewall=$fw cameras=0 residue=$left connected=$(yes_no is_connected) version=$VERSION"
			return 1
		fi
		if is_connected; then
			# BombeCam stays connected (program and key kept) so it can turn
			# blocking back on; "uninstall" removes those too.
			rm -f "$CAMS" "$IPS" "$OPTS"
			say "Block cloud video: NO for every camera. BombeCam's rules are removed; the cameras work normally."
			say "BombeCam stays connected to this router so it can block cameras again (\"uninstall\" removes that too)."
			say "BombeCam's rules are gone from the firewall (checked)."
			say "BOMBECAM_RESULT status=ok block=no firewall=$fw cameras=0 residue=0 connected=yes version=$VERSION"
			return 0
		fi
		remove_keys
		rm -rf "$BC_DIR"
		[ "${BC_DRYRUN:-0}" = 1 ] || rm -f "$BIN"
		say "Block cloud video: NO. BombeCam's rules are removed from this router; the camera works normally."
		say "BOMBECAM_RESULT status=ok block=no firewall=$fw cameras=0 residue=0 connected=no version=$VERSION"
		return 0
	fi

	[ "$fw" != none ] || die "no supported firewall found (need OpenWrt fw4/nftables or fw3/iptables)"
	mkdir -p "$BC_DIR"
	BSS=$(opt BLOCK_STREAM_SETUP)
	RGN=$(opt REGION)
	parse_cameras "$CAMS.new" "$@"
	if [ -s "$CAMS.new" ]; then
		sort "$CAMS.new" >"$CAMS"
	elif [ ! -s "$CAMS" ]; then
		rm -f "$CAMS.new"
		say "Which cameras? Pass each one as --camera \"NAME=MAC\". DHCP clients on this router:"
		cmd_clients
		die "no cameras given"
	fi
	rm -f "$CAMS.new"
	set_opt BLOCK_STREAM_SETUP "${BSS:-0}"
	set_opt REGION "${RGN:-us}"

	install_self

	say "Firewall: $fw ($([ "$fw" = fw4 ] && echo nftables || echo iptables))"
	disable_offload
	install_persistence "$fw"
	refresh_ips || true
	for h in $(allowed_hosts); do
		[ -n "$(host_ips "$h")" ] || warn "could not resolve $h yet; that traffic stays blocked until it does (retried every 10 minutes)"
	done
	load_rules
	mark_loaded force
	clear_camera_connections
	report_cleared
	acc=$(accel_modules)
	[ -n "$acc" ] && warn "hardware/network acceleration is active ($acc). Turn off \"Network Acceleration\" / \"Hardware Acceleration\" in the router's admin page, otherwise accelerated connections can bypass the bandwidth cap."
	bridge_mode_warning
	say ""
	say "Block cloud video: YES. $(cam_macs | wc -l | tr -d ' ') camera(s) selected for blocking."
	say "Osaio region: $(effective_region)"
	say "Camera internet access is limited by destination and rate; encrypted traffic may still carry image data."
	say "BOMBECAM_RESULT status=ok block=yes firewall=$fw cameras=$(cam_macs | wc -l | tr -d ' ') cap=${CAP_MODE:-bytes} stream_setup=$(stream_setup_allowed && echo allowed || echo blocked) region=$(effective_region) cleared=$CLEARED connected=$(yes_no is_connected) version=$VERSION"
}

# cmd_load runs at boot and, on fw3, after every firewall reload (which
# removed the rules for a moment): connections opened meanwhile are cleared.
cmd_load() {
	load_rules
	[ -s "$CAMS" ] || return 0
	mark_loaded
	clear_camera_connections
	return 0
}

cmd_refresh() {
	[ -s "$CAMS" ] || return 0
	fw=$(detect_fw)
	loaded=1
	case "$fw" in
	fw4) nft list table inet $TABLE >/dev/null 2>&1 || loaded=0 ;;
	fw3) iptables -S bombecam_out >/dev/null 2>&1 || loaded=0 ;;
	esac
	if refresh_ips || [ $loaded = 0 ]; then load_rules; fi
	# Rules that had gone missing: clear what the cameras opened meanwhile.
	[ $loaded = 1 ] || clear_camera_connections
}

cmd_render() {
	if [ $# -gt 0 ]; then
		# render yes --camera ...: preview without touching the saved config
		[ "$1" = yes ] || die "usage: render [yes --camera NAME=MAC ...]"
		shift
		tmpdir=$(mktemp -d)
		BSS=0
		RGN=$(opt REGION)
		parse_cameras "$tmpdir/cameras" "$@"
		[ -f "$IPS" ] && cp "$IPS" "$tmpdir/ips"
		CAMS=$tmpdir/cameras
		IPS=$tmpdir/ips
		OPTS=$tmpdir/options
		printf 'BLOCK_STREAM_SETUP=%s\n' "$BSS" >"$OPTS"
		printf 'REGION=%s\n' "${RGN:-us}" >>"$OPTS"
	fi
	fw=$(detect_fw)
	case "$fw" in
	fw4) gen_nft ;;
	fw3) fw3_caps; gen_ipt ;;
	*) die "no supported firewall found" ;;
	esac
	[ -n "${tmpdir:-}" ] && rm -rf "$tmpdir"
	return 0
}

cmd_clients() {
	if [ ! -s "$LEASES" ]; then
		say "(no DHCP leases found in /tmp/dhcp.leases)"
		return 0
	fi
	printf '%-18s %-16s %s\n' MAC IP NAME
	while read -r _exp mac ip host _id; do
		mark=""
		cam_macs 2>/dev/null | grep -qx "$(printf '%s' "$mac" | tr 'A-F' 'a-f')" && mark="  <- camera (protected)"
		printf '%-18s %-16s %s%s\n' "$mac" "$ip" "$host" "$mark"
	done <"$LEASES"
}

cam_ip() {
	ip=""
	[ -s "$LEASES" ] && ip=$(awk -v m="$1" 'tolower($2) == m { print $3; exit }' "$LEASES")
	[ -z "$ip" ] && have ip && ip=$(ip -4 neigh 2>/dev/null | awk -v m="$1" 'tolower($5) == m { print $1; exit }')
	say "$ip"
}

# cam_addrs prints "address mac" for every address the router knows for the
# blocked cameras: the IPv4 address (DHCP lease or neighbour table) and the
# IPv6 neighbours.
cam_addrs() {
	for m in $(cam_macs); do
		cip=$(cam_ip "$m")
		[ -n "$cip" ] && say "$cip $m"
		have ip && ip -6 neigh 2>/dev/null | awk -v m="$m" 'tolower($5) == m { print $1, m }'
	done
}

# ct_list prints the router's connection list in the format of
# /proc/net/nf_conntrack. That file is optional in the kernel (OpenWrt has it,
# many other Linux kernels don't); without it the conntrack tool prints the
# same lines. Returns 1 if neither can read the list.
ct_list() {
	if [ -r "$CT_FILE" ]; then
		cat "$CT_FILE"
	elif [ -z "${BC_CONNTRACK:-}" ] && have conntrack && conntrack -C >/dev/null 2>&1; then
		conntrack -L -f ipv4 -o extended 2>/dev/null || return 1
		conntrack -L -f ipv6 -o extended 2>/dev/null || :
	else
		return 1
	fi
}

# camera_conns DIR writes DIR/cams (cam_addrs) and DIR/out: one line
# "mac|category|connection" for each tracked connection a blocked camera
# opened. Category is control, stream_setup, dns, time, "your network",
# "this router" or "NOT ALLOWED". Returns 1 if the table can't be read.
camera_conns() {
	d=$1
	ct_list >"$d/ct" || return 1
	cam_addrs >"$d/cams"
	: >"$d/self"
	have ip && ip addr 2>/dev/null | awk '$1 == "inet" || $1 == "inet6" { a = $2; sub(/\/.*/, "", a); print a }' >"$d/self"
	cp "$IPS" "$d/ips" 2>/dev/null || : >"$d/ips"
	sa=0
	stream_setup_allowed && sa=1
	awk -v fc="$d/cams" -v fself="$d/self" -v fips="$d/ips" \
		-v ch="$(control_hosts)" -v cport="$CONTROL_PORT" -v shost="$(setup_host)" -v sport="$SETUP_PORT" -v sa="$sa" '
		function loc4(a, o) {
			split(a, o, ".")
			return o[1] == 10 || o[1] == 127 || o[1] == 0 || o[1] >= 224 ||
				(o[1] == 172 && o[2] >= 16 && o[2] <= 31) || (o[1] == 192 && o[2] == 168) ||
				(o[1] == 169 && o[2] == 254)
		}
		function loc6(a) {
			a = tolower(a)
			return a ~ /^fe[89ab]/ || a ~ /^f[cd]/ || a ~ /^ff/ || a == "::1"
		}
		function inhosts(list, ip,   n, a, i) {
			n = split(list, a, " ")
			for (i = 1; i <= n; i++) if ((a[i] " " ip) in allow) return 1
			return 0
		}
		FILENAME == fc { cam[$1] = $2; next }
		FILENAME == fself { self[$1] = 1; next }
		FILENAME == fips { allow[$1 " " $2] = 1; next }
		{
			fam = $1; proto = $3; src = ""; dst = ""; dport = ""; state = ""; unrep = 0
			for (i = 4; i <= NF; i++) {
				if (src == "" && $i ~ /^[A-Z][A-Z_]*$/) state = $i
				if ($i == "[UNREPLIED]") unrep = 1
				if (substr($i, 1, 4) == "src=") { if (src == "") src = substr($i, 5) }
				else if (substr($i, 1, 4) == "dst=") { if (dst == "") dst = substr($i, 5) }
				else if (substr($i, 1, 6) == "dport=") { if (dport == "") dport = substr($i, 7) }
			}
			if (!(src in cam)) next
			k = "NOT ALLOWED"
			if (dst in self) k = "this router"
			else if (fam == "ipv4" && loc4(dst)) k = "your network"
			else if (fam == "ipv6" && loc6(dst)) k = "your network"
			else if (fam == "ipv4") {
				if (proto == "tcp" && dport == cport && inhosts(ch, dst)) k = "control"
				else if (proto == "tcp" && sa == 1 && dport == sport && ((shost " " dst) in allow)) k = "stream_setup"
				else if ((proto == "udp" || proto == "tcp") && dport == "53") k = "dns"
				else if (proto == "udp" && dport == "123") k = "time"
			}
			d = proto " " (fam == "ipv6" ? "[" dst "]" : dst) (dport != "" ? ":" dport : "")
			if (state != "") d = d " " state
			if (unrep) d = d " (no reply)"
			print cam[src] "|" k "|" d
		}' "$d/cams" "$d/self" "$d/ips" "$d/ct" >"$d/out"
}

# clear_addr ADDRESS removes every tracked connection to or from ADDRESS from
# the router's connection list. Only a well-formed address is ever written:
# OpenWrt's conntrack file clears every device's connections when given
# anything else.
clear_addr() {
	case "$1" in
	'' | *[!0-9A-Fa-f:.]*) return 1 ;; # also rejects spaces and newlines
	*:*) [ ${#1} -le 45 ] || return 1; fam=ipv6 ;;
	*) printf '%s' "$1" | grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}$' || return 1; fam=ipv4 ;;
	esac
	if [ "${BC_DRYRUN:-0}" = 1 ]; then
		say "[dry-run] clear the router's tracked connections of $1"
		return 0
	fi
	if [ -z "${BC_CONNTRACK:-}" ] && have conntrack && conntrack -C >/dev/null 2>&1; then
		# -D exits 1 when nothing matched, so success is "conntrack works".
		for sel in -s -d -r -q; do conntrack -D -f "$fam" "$sel" "$1" >/dev/null 2>&1; done
		return 0
	fi
	# OpenWrt kernels accept an address written to the conntrack file.
	{ printf '%s\n' "$1" >"$CT_FILE"; } 2>/dev/null
}

# clear_camera_connections clears the tracked connections of every blocked
# camera, so none opened before the rules went into force can carry on past
# them: the next packet of such a connection is checked like a new one. The
# camera reopens what it is still allowed to use by itself. Sets CLEARED to
# yes, none (the router knows no camera address) or no (this router cannot
# clear connections).
clear_camera_connections() {
	CLEARED=none
	for a in $(cam_addrs | cut -d' ' -f1 | sort -u); do
		if clear_addr "$a"; then
			[ "$CLEARED" = no ] || CLEARED=yes
		else
			CLEARED=no
		fi
	done
}

# report_cleared runs after clear_camera_connections during an apply. It waits
# briefly, then lists what the cameras are connected to: a connection outside
# the allowed list that is open again after clearing got past the rules.
report_cleared() {
	case "$CLEARED" in
	none)
		say "No blocked camera is on the network right now, so it has no open connections to clear."
		return 0
		;;
	no)
		warn "this router cannot clear connections opened before the block (it has neither the conntrack tool nor OpenWrt's connection clearing). They end only when they close or the router restarts."
		;;
	esac
	[ "${BC_DRYRUN:-0}" = 1 ] && return 0
	[ "$CLEARED" = yes ] && sleep "${BC_SETTLE:-2}"
	tmp=$(mktemp -d)
	if ! camera_conns "$tmp"; then
		rm -rf "$tmp"
		warn "could not read the router's connection list to confirm the cameras' connections were cleared."
		return 0
	fi
	bad=$(grep -c '|NOT ALLOWED|' "$tmp/out")
	list=$(grep '|NOT ALLOWED|' "$tmp/out" | cut -d'|' -f3 | head -n 10 | sed 's/^/    /')
	rm -rf "$tmp"
	if [ "$bad" = 0 ]; then
		[ "$CLEARED" = yes ] && say "Cleared the cameras' open connections; none outside the allowed list came back."
		return 0
	fi
	if [ "$CLEARED" = yes ]; then
		say "Camera connections outside the allowed list are open again right after clearing:"
		say "$list"
		die "$bad camera connection(s) outside the allowed list got past the rules after the cameras' connections were cleared. The block is not working on this router."
	fi
	say "Camera connections outside the allowed list are still open:"
	say "$list"
	warn "$bad camera connection(s) outside the allowed list opened before the block are still open. Restart the router to end them."
}

cmd_status() {
	names=0
	[ "${1:-}" = "--names" ] && names=1
	fw=$(detect_fw)
	say "BombeCam router status (script $VERSION)"
	say "  Connected to BombeCam: $(yes_no is_connected)"
	if [ ! -s "$CAMS" ]; then
		left=$(firewall_residue)
		say "  Block cloud video: NO (no cameras blocked)"
		if [ "$left" != 0 ]; then
			say "  WARNING: $left BombeCam rule(s) are still in the firewall. Press Block cloud video off again in BombeCam, or restart the router."
		else
			say "  BombeCam rules in the firewall: none (checked)"
		fi
		say "BOMBECAM_RESULT status=ok block=no firewall=$fw cameras=0 residue=$left connected=$(yes_no is_connected) version=$VERSION"
		return 0
	fi
	loaded=yes
	case "$fw" in
	fw4) nft list chain inet $TABLE camera_out >/dev/null 2>&1 || loaded=no ;;
	fw3) iptables -S bombecam_out >/dev/null 2>&1 || loaded=no ;;
	esac
	say "  Block cloud video: YES   rules loaded: $loaded"
	say "  Firewall: $fw"
	say "  Region: $(effective_region)"
	say "  Cameras:"
	for m in $(cam_macs); do
		cip=$(cam_ip "$m")
		say "    $(cam_name "$m")  $m  ${cip:-(IP not seen yet)}"
	done
	say "  Allowed, sharing a cap of $CAP_BPS bytes/s per camera (burst $CAP_BURST):"
	for h in $(control_hosts); do
		say "    control   $h:$CONTROL_PORT -> $(host_ips "$h" | tr '\n' ' ')"
	done
	if stream_setup_allowed; then
		shost=$(setup_host)
		say "    setup     $shost:$SETUP_PORT -> $(host_ips "$shost" | tr '\n' ' ')"
	else
		say "    setup     $(setup_host):$SETUP_PORT -> BLOCKED (--block-stream-setup)"
	fi
	say "    DNS (53) and time sync (123), IPv4"
	if have uci; then
		say "  Flow offloading: $(uci -q get firewall.@defaults[0].flow_offloading || echo 0)  hardware: $(uci -q get firewall.@defaults[0].flow_offloading_hw || echo 0)"
	fi
	acc=$(accel_modules)
	say "  Acceleration modules: ${acc:-none detected}"
	say ""
	say "  Camera traffic since the rules were last loaded:"
	case "$fw" in
	fw4)
		nft list table inet $TABLE 2>/dev/null | awk '
			/counter packets/ && /comment "/ {
				c = $0; sub(/.*comment "/, "", c); sub(/".*/, "", c)
				if (c ~ /^cap: /) c = "over cap, dropped: " substr(c, 6)
				p = $0; sub(/.*counter packets /, "", p); split(p, a, " ")
				pk[c] += a[1]; by[c] += a[3]
				if (!(c in seen)) { seen[c] = 1; order[n++] = c }
			}
			END { for (i = 0; i < n; i++) printf "    %-40s %8d packets %10d bytes\n", order[i], pk[order[i]], by[order[i]] }'
		;;
	fw3)
		iptables -vnxL bombecam_out 2>/dev/null | awk '
			NR > 2 {
				w = ""
				if ($3 == "bombecam_cap") {
					w = "other"
					for (i = 1; i <= NF; i++) if ($i ~ /^dpt:/) {
						d = substr($i, 5)
						w = (d == 8883) ? "control" : (d == 443) ? "stream_setup" : (d == 53) ? "dns" : (d == 123) ? "time" : d
					}
				}
				if ($3 == "DROP") w = "everything else, IPv4"
				if (w == "") next
				pk[w] += $1; by[w] += $2
				if (!(w in seen)) { seen[w] = 1; order[n++] = w }
			}
			END { for (i = 0; i < n; i++) printf "    %-40s %8d packets %10d bytes\n", order[i], pk[order[i]], by[order[i]] }'
		iptables -vnxL bombecam_cap 2>/dev/null | awk '
			NR > 2 {
				m = ""
				for (i = 1; i <= NF; i++) if ($i == "MAC") m = tolower($(i + 1))
				if ($3 == "DROP" && m != "") printf "    %-40s %8d packets %10d bytes\n", "over cap, dropped: " m, $1, $2
				if ($3 == "ACCEPT") printf "    %-40s %8d packets %10d bytes\n", "allowed, under cap" (m != "" ? ": " m : ""), $1, $2
			}'
		have ip6tables && ip6tables -vnxL bombecam_v6 2>/dev/null | awk '
			NR > 2 && $3 == "DROP" { printf "    %-40s %8d packets %10d bytes\n", "everything else, IPv6", $1, $2 }'
		;;
	esac
	say "    (Allowed categories count packets before the cap check. Only \"allowed, under cap\""
	say "    packets were forwarded to the internet; everything else was dropped.)"

	say ""
	say "  Recent blocked attempts (system log, newest 200 lines):"
	if have logread; then
		logread 2>/dev/null | grep 'bombecam-blocked' | tail -n 200 |
			sed -n 's/.*DST=\([^ ]*\).*PROTO=\([A-Z0-9]*\).*DPT=\([0-9]*\).*/\1 \2 \3/p' |
			sort | uniq -c | sort -rn | head -n 15 | while read -r count dst proto dport; do
				label=""
				if [ $names = 1 ]; then
					label=$(nslookup "$dst" 2>/dev/null | sed -n 's/.*name = \(.*\)/\1/p; s/^Address[^:]*: *[0-9a-f.:]* \(.*\)/\1/p' | head -n 1)
				fi
				say "    $count x $proto $dst:$dport ${label:+ ($label)}"
			done
	else
		say "    (logread not available)"
	fi
	say "BOMBECAM_RESULT status=ok block=yes firewall=$fw cameras=$(cam_macs | wc -l | tr -d ' ') loaded=$loaded region=$(effective_region) connected=$(yes_no is_connected) version=$VERSION"
}

# cmd_connections lists what the cameras are connected to, from the kernel's
# connection tracking table. A packet the rules drop never creates an entry
# there, so every listed connection is one the router let through (or one the
# camera opened before the rules went into force).
cmd_connections() {
	if [ ! -s "$CAMS" ]; then
		say "Block cloud video: NO (no BombeCam rules installed); nothing to check."
		say "BOMBECAM_RESULT status=ok block=no"
		return 0
	fi
	tmp=$(mktemp -d)
	camera_conns "$tmp" || { rm -rf "$tmp"; die "cannot read the router's connection list ($CT_FILE, or the conntrack tool)"; }

	up=$(cut -d. -f1 /proc/uptime 2>/dev/null)
	ld=$(cat "$LOADED" 2>/dev/null)
	since_boot=0
	[ -n "$ld" ] && [ "$ld" -le 180 ] && since_boot=1
	say "What your cameras are connected to right now (router's connection list)."
	say "Blocked attempts never appear here; see \"status\" for those."
	for m in $(cam_macs); do
		addrs=$(awk -v m="$m" '$2 == m { printf "%s ", $1 }' "$tmp/cams")
		say ""
		say "  $(cam_name "$m")  $m  ${addrs:-(IP not seen yet: nothing to check)}"
		for k in control stream_setup dns time "your network" "this router"; do
			n=$(grep -c "^$m|$k|" "$tmp/out")
			[ "$n" -gt 0 ] && printf '    %-14s %s\n' "$k" "$n"
		done
		printf '    %-14s %s\n' "not allowed" "$(grep -c "^$m|NOT ALLOWED|" "$tmp/out")"
		grep "^$m|NOT ALLOWED|" "$tmp/out" | cut -d'|' -f3 | sed 's/^/      /'
	done
	total=$(wc -l <"$tmp/out" | tr -d ' ')
	bad=$(grep -c '|NOT ALLOWED|' "$tmp/out")
	rm -rf "$tmp"
	say ""
	if [ "$bad" = 0 ]; then
		say "Result: nothing outside the allowed list (control, stream setup, DNS, time, your own network)."
	elif [ $since_boot = 1 ]; then
		say "Result: $bad connection(s) outside the allowed list got through although the rules have been in force since the router started."
		say "The block is not working as intended. Run \"bombecam-router status\" and check for acceleration warnings."
	else
		if [ -n "$ld" ] && [ -n "$up" ]; then
			say "Result: $bad connection(s) outside the allowed list. The rules went into force $(((up - ld) / 60)) min ago, $((ld / 60)) min after the router started."
		else
			say "Result: $bad connection(s) outside the allowed list. It is not known when the rules went into force."
		fi
		say "Connections the camera opened before that stay listed until they time out, even though their packets are now dropped."
		say "For a clean check: reboot the router, wait 5 minutes, run this again. After a reboot, anything listed here got through."
	fi
	say "BOMBECAM_RESULT status=ok block=yes connections=$total not_allowed=$bad since_boot=$since_boot"
}

# cmd_connect installs this script and BombeCam's key(s). Run once, over the
# router password; nothing about the camera rules changes.
cmd_connect() {
	[ "${BC_DRYRUN:-0}" = 1 ] || [ "$(id -u)" = 0 ] || die "run this as root on the router"
	keys=""
	while [ $# -gt 0 ]; do
		case "$1" in
		--key)
			[ $# -ge 2 ] || die "--key needs a public key"
			k=$(clean_key "$2") || die "not a supported SSH public key"
			keys="$keys$k
"
			shift 2
			;;
		*) die "unknown option: $1" ;;
		esac
	done
	[ -n "$keys" ] || die "connect needs at least one --key"
	fw=$(detect_fw)
	[ "$fw" != none ] || die "no supported firewall found (need OpenWrt fw4/nftables or fw3/iptables)"
	files=$(auth_key_files) || die "could not find the router's SSH server (dropbear or OpenSSH)"
	install_self
	for f in $files; do write_keys "$f" "$keys"; done
	mkdir -p "$BC_DIR"
	date +%s >"$KEYMARK"
	say "Connected. BombeCam can now update this router's camera rules without the password."
	say "Its key can only run BombeCam's own commands; it cannot open a shell or forward connections."
	say "BOMBECAM_RESULT status=ok connected=yes firewall=$fw version=$VERSION"
}

# cmd_uninstall removes the rules, the boot and cron entries, BombeCam's key
# and this script.
cmd_uninstall() {
	[ "${BC_DRYRUN:-0}" = 1 ] || [ "$(id -u)" = 0 ] || die "run this as root on the router"
	fw=$(detect_fw)
	remove_persistence
	left=$(unload_verified)
	restore_offload
	remove_keys
	rm -f "$LOADED"
	rm -rf "$BC_DIR"
	[ "${BC_DRYRUN:-0}" = 1 ] || rm -f "$BIN"
	if [ "$left" != 0 ]; then
		say "BombeCam's key and files are removed, but $left of its rules are still in the firewall. Restart the router, which clears them."
		say "BOMBECAM_RESULT status=error block=no firewall=$fw cameras=0 residue=$left connected=no version=$VERSION"
		return 1
	fi
	say "BombeCam is removed from this router: no camera rules and no BombeCam key. The cameras work normally."
	say "BOMBECAM_RESULT status=ok block=no firewall=$fw cameras=0 residue=0 connected=no version=$VERSION"
}

# cmd_gate is the forced command of BombeCam's key. It runs only the verbs
# below. "apply" reads the complete list of cameras to block from stdin:
#   camera <mac> <name>        (one line per camera; none = block no camera)
#   block_stream_setup 0|1
#   region us|eu               (optional; omitted keeps the saved choice)
cmd_gate() {
	case "${SSH_ORIGINAL_COMMAND:-}" in
	apply) gate_apply ;;
	status) cmd_status ;;
	connections) cmd_connections ;;
	version)
		say "bombecam-router $VERSION"
		say "BOMBECAM_RESULT status=ok connected=$(yes_no is_connected) firewall=$(detect_fw) version=$VERSION"
		;;
	uninstall) cmd_uninstall ;;
	*) die "BombeCam's key can only run: apply, status, connections, version, uninstall" ;;
	esac
}

gate_apply() {
	spec=$(mktemp)
	head -c 65536 >"$spec"
	bss=""
	rgn=""
	set --
	while IFS= read -r line || [ -n "$line" ]; do
		case "$line" in
		"block_stream_setup 1") bss=1 ;;
		"block_stream_setup 0") bss=0 ;;
		"region us" | "region eu") rgn=${line#region } ;;
		"camera "*)
			rest=${line#camera }
			mac=${rest%% *}
			name=""
			[ "$rest" != "$mac" ] && name=${rest#* }
			set -- "$@" --camera "$name=$mac"
			;;
		"" | "#"*) ;;
		*)
			rm -f "$spec"
			die "unexpected line in the camera list"
			;;
		esac
	done <"$spec"
	rm -f "$spec"
	if [ $# -eq 0 ]; then
		cmd_apply no
		return
	fi
	[ "$bss" = 1 ] && set -- "$@" --block-stream-setup
	[ "$bss" = 0 ] && set -- "$@" --allow-stream-setup
	[ -n "$rgn" ] && set -- "$@" --region "$rgn"
	cmd_apply yes "$@"
}

usage() {
	awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0" 2>/dev/null
}

cmd=${1:-help}
[ $# -gt 0 ] && shift
case "$cmd" in
apply) cmd_apply "$@" ;;
load) cmd_load ;;
refresh) cmd_refresh ;;
status) cmd_status "$@" ;;
connections) cmd_connections ;;
connect) cmd_connect "$@" ;;
uninstall) cmd_uninstall ;;
gate) cmd_gate ;;
clients) cmd_clients ;;
render) cmd_render "$@" ;;
version) say "$VERSION" ;;
help | -h | --help) usage ;;
*)
	usage
	exit 1
	;;
esac
