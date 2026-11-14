#!/usr/bin/env sh
# =============================================================================
# Scrive $OUT_DIR/osaio-setup.txt con i valori Osaio offuscati letti da
# un'immagine BombeCam gateway, così ha-bombecam-backends.sh non ne perde la
# chiave tra una build e l'altra.
#
# I valori restano offuscati (internal/obscure): lo script copia i blob
# 'obs1.*' dal binario e li assegna a server_key e app_id.
#
# Uso:
#   OUT_DIR=/root/bc/BombeCam-backup [IMAGE=bombecam-gateway:local] ./ha-bombecam-recupera-chiave.sh
# =============================================================================
set -eu

IMAGE="${IMAGE:-bombecam-gateway:local}"
OUT_DIR="${OUT_DIR:-$(pwd)/BombeCam-backup}"
say() { printf '%s\n' "$*"; }
die() { printf '[x] %s\n' "$*" >&2; exit 1; }

docker image inspect "$IMAGE" >/dev/null 2>&1 || die "immagine '$IMAGE' non presente."

tmpd=$(mktemp -d)
cid=$(docker create "$IMAGE")
trap 'docker rm -f "$cid" >/dev/null 2>&1 || true; rm -rf "$tmpd"' EXIT INT TERM

docker cp "$cid:/usr/local/bin/bombecam-gateway" "$tmpd/gw" >/dev/null 2>&1 \
	|| die "non riesco a copiare il binario da $IMAGE."

# I due valori sono le uniche stringhe 'obs1.' del binario. La chiave è più
# lunga dell'app ID: si assegnano per lunghezza e si verifica con la build
# (lo script confronta i blob vecchi e nuovi e si ferma se non combaciano).
mapfile=$(grep -a -o 'obs1\.[A-Za-z0-9_-]*' "$tmpd/gw" | grep -v '^obs1\.$' | sort -u)
count=$(printf '%s\n' "$mapfile" | grep -c . || true)
[ "$count" -eq 2 ] || die "trovati $count valori offuscati (attesi 2): non tocco nulla."

first=$(printf '%s\n' "$mapfile" | sed -n 1p)
second=$(printf '%s\n' "$mapfile" | sed -n 2p)
if [ "${#first}" -ge "${#second}" ]; then key=$first; appid=$second; else key=$second; appid=$first; fi

mkdir -p "$OUT_DIR"
chmod 700 "$OUT_DIR" 2>/dev/null || true
{
	printf '# Recuperato da %s il %s (valori offuscati).\n' "$IMAGE" "$(date '+%Y-%m-%d %H:%M')"
	printf 'server_key=%s\n' "$key"
	printf 'app_id=%s\n' "$appid"
} > "$OUT_DIR/osaio-setup.txt"
chmod 600 "$OUT_DIR/osaio-setup.txt"
say "[ok] scritto $OUT_DIR/osaio-setup.txt (mode 600)"
