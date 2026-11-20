#!/usr/bin/env bash
# =============================================================================
# BombeCam - deploy/aggiornamento branch su HA OS
#
# 0/8  controlli. Se il compose e' sparito lo RIGENERA dai container in
#      esecuzione (label di compose, env, volumi nominati) in /share/bombecam/,
#      che e' persistente: /root dell'add-on viene cancellato.
# 1/8  scarica il branch dal fork oxi-git/BombeCam
# 2/8  chiave Osaio: se e' compilata dentro l'immagine in uso (source=built_in)
#      la ricava dal binario stesso (e' offuscata, non cifrata: internal/obscure)
# 3/8  compila bombecam-firewall:local (demone con API /api/v1/net/status, /block-cloud-video, /verdict)
# 4/8  compila bombecam-gateway:local e verifica che contenga gli stessi valori
#      offuscati dell'immagine in uso (andata/ritorno)
# 5/8  ricrea gateway e firewall con docker compose (--no-deps, volumi intatti)
# 6/8  verifica l'API del demone su 127.0.0.1:8653
# 7/8  imposta region e backend, controlla in_sync
# 8/8  riepilogo: log, regole nft, pulizia cache
#
# Idempotente. NON usa 'docker compose down -v'. Non stampa segreti.
#
# Variabili sovrascrivibili:
#   BRANCH=main                 (branch da deployare; default: main)
#   CFG_DIR=/share/bombecam     (dir persistente per compose/env)
#   REGION=eu                   (regione Osaio)
#   BUILD_BASE=/root/bc         (base dir per build temporanee)
#   GW_IMAGE_TAG=bombecam-gateway:local
#   FW_IMAGE_TAG=bombecam-firewall:local
#   SWITCH=1                    (0 = non cambiare backend/regione)
#   ALLOW_MISSING_KEY=1         (build senza chiave Osaio: la pagina la chiede)
#   RECORDINGS_DIR=/share/bombecam/recordings  (dir per registrazioni locali)
# =============================================================================
set -euo pipefail

BRANCH="${BRANCH:-main}"
REGION="${REGION:-eu}"
CFG_DIR="${CFG_DIR:-/share/bombecam}"
COMPOSE="${COMPOSE:-$CFG_DIR/compose.yml}"
DOTENV="$(dirname "$COMPOSE")/.env"
BUILD_BASE="${BUILD_BASE:-/root/bc}"
GW_CONTAINER="${GW_CONTAINER:-bombecam-gateway}"
FW_CONTAINER="${FW_CONTAINER:-bombecam-firewall}"
MTX_CONTAINER="${MTX_CONTAINER:-bombecam-mediamtx}"
GW_IMAGE_TAG="${GW_IMAGE_TAG:-bombecam-gateway:local}"
FW_IMAGE_TAG="${FW_IMAGE_TAG:-bombecam-firewall:local}"
SWITCH="${SWITCH:-1}"
RECORDINGS_DIR="${RECORDINGS_DIR:-/share/bombecam/recordings}"
SRC="$BUILD_BASE/BombeCam-${BRANCH//\//-}"

say()  { printf '%s\n' "$*"; }
step() { printf '\n=== %s ===\n' "$*"; }
ok()   { printf '[ok] %s\n' "$*"; }
warn() { printf '[!] %s\n' "$*" >&2; }
die()  { printf '[x] %s\n' "$*" >&2; exit 1; }

# --- 0. controlli ------------------------------------------------------------
step "0/8  Controlli"

[ "$(id -u)" -eq 0 ] || die "esegui come root (nell'add-on Terminal & SSH lo sei già)."
command -v docker >/dev/null 2>&1 || die "docker non trovato nel PATH."
docker compose version >/dev/null 2>&1 \
	|| die "plugin 'docker compose' non trovato. Nell'add-on: apk add --no-cache docker-cli-compose"
command -v wget >/dev/null 2>&1 || command -v curl >/dev/null 2>&1 \
	|| die "serve wget o curl per scaricare il branch."

for c in "$GW_CONTAINER" "$FW_CONTAINER" "$MTX_CONTAINER"; do
	docker inspect "$c" >/dev/null 2>&1 \
		|| die "container '$c' non visibile da qui: il socket Docker dell'add-on non vede l'host."
done

GW_IMAGE=$(docker inspect "$GW_CONTAINER" --format '{{.Config.Image}}')
FW_IMAGE=$(docker inspect "$FW_CONTAINER" --format '{{.Config.Image}}')
PROJECT=$(docker inspect "$GW_CONTAINER" --format '{{index .Config.Labels "com.docker.compose.project"}}')
[ -n "$PROJECT" ] || die "il gateway non ha label di compose: non so a quale progetto appartiene."

# docker compose interpreta i '$' dentro env_file? Dipende dalla versione (da
# v2.30 in poi sì). Se li interpreta, li raddoppio quando scrivo i file: '$$'
# torna '$' e il valore arriva intatto al container. Lo chiedo a compose
# stesso invece di indovinare.
probe_dir=$(mktemp -d)
printf 'P=a$b\n' > "$probe_dir/p.env"
printf 'services:\n  x:\n    image: alpine\n    env_file: [p.env]\n' > "$probe_dir/c.yml"
probe_p=$(cd "$probe_dir" && docker compose -f c.yml config 2>/dev/null | sed -n 's/^ *P: *//p' | head -1)
rm -rf "$probe_dir"
case "$probe_p" in
'a$b') ENV_ESCAPE=0; ok "compose non interpreta '\$' negli env_file: li copio alla lettera" ;;
'a')   ENV_ESCAPE=1; ok "compose interpreta '\$' negli env_file: lo raddoppio quando scrivo" ;;
*)     ENV_ESCAPE=0; warn "sonda sul '\$' non conclusiva: copio alla lettera e controllo l'ambiente dopo l'avvio." ;;
esac

dump_env() { # container -> righe KEY=value (come le vede il container)
	if [ "$ENV_ESCAPE" = "1" ]; then
		docker inspect "$1" --format '{{range .Config.Env}}{{println .}}{{end}}' | sed 's/\$/$$/g'
	else
		docker inspect "$1" --format '{{range .Config.Env}}{{println .}}{{end}}'
	fi
}

# Il compose perso si ricostruisce dai container vivi. Nessun bind mount in
# gioco (solo volumi nominati), quindi il file puo' stare dove ci pare.
if [ ! -f "$COMPOSE" ]; then
	say "compose assente: lo rigenero dai container in esecuzione (progetto '$PROJECT')."
	mkdir -p "$(dirname "$COMPOSE")"
	chmod 700 "$(dirname "$COMPOSE")" 2>/dev/null || true

	for svc in gateway firewall mediamtx; do
		c="bombecam-$svc"
		dump_env "$c" > "$(dirname "$COMPOSE")/$svc.env"
		chmod 600 "$(dirname "$COMPOSE")/$svc.env"
	done
	ok "scritte gateway.env, firewall.env, mediamtx.env (mode 600, dai container vivi)"

	cat > "$COMPOSE" <<YAML
# BombeCam su Home Assistant OS.
# Rigenerato il $(date '+%Y-%m-%d %H:%M') dai container in esecuzione, perché
# il compose originale stava in /root dell'add-on SSH e non è persistente.
# Le variabili di ogni servizio stanno in *.env (mode 600); le immagini in .env.
name: $PROJECT
services:
  mediamtx:
    image: $(docker inspect "$MTX_CONTAINER" --format '{{.Config.Image}}')
    container_name: $MTX_CONTAINER
    restart: unless-stopped
    network_mode: host
    env_file:
      - mediamtx.env
  gateway:
    image: \${BOMBECAM_GATEWAY_IMAGE:-$GW_IMAGE}
    container_name: $GW_CONTAINER
    restart: unless-stopped
    network_mode: host
    env_file:
      - gateway.env
    volumes:
      - state:/var/lib/bombecam
      - recordings:${RECORDINGS_DIR}
  firewall:
    image: \${BOMBECAM_FIREWALL_IMAGE:-$FW_IMAGE}
    container_name: $FW_CONTAINER
    restart: unless-stopped
    network_mode: host
    env_file:
      - firewall.env
    cap_add:
      - NET_ADMIN
      - NET_RAW
    volumes:
      - fw:/var/lib/bombecam-net
volumes:
  state:
    external: true
    name: bombecam_state
  fw:
    external: true
    name: bombecam_bombecam_fw
  recordings:
    external: true
    name: bombecam_recordings
YAML
	ok "scritto $COMPOSE"
else
	ok "compose presente: $COMPOSE"
	# Verifica/aggiungi volume recordings se manca
	if ! grep -q "recordings:" "$COMPOSE"; then
		warn "volume 'recordings' mancante in compose: lo aggiungo"
		# Aggiunge il volume recordings in fondo alla sezione volumes:
		sed -i '/^volumes:/a\  recordings:\n    external: true\n    name: bombecam_recordings' "$COMPOSE"
		# Aggiunge il mount al servizio gateway (dopo la riga "volumes:")
		awk -v rec="${RECORDINGS_DIR}" '
			/gateway:/ { in_gw=1 }
			in_gw && /^[[:space:]]*firewall:/ { in_gw=0 }
			in_gw && /^[[:space:]]*volumes:/ { print; print "      - recordings:" rec; next }
			{ print }
		' "$COMPOSE" > "$COMPOSE.tmp" && mv "$COMPOSE.tmp" "$COMPOSE"
		ok "volume recordings aggiunto al compose"
	fi
fi

# .env del progetto: solo i tag immagine (le variabili stanno nei *.env)
if [ ! -f "$DOTENV" ]; then
	printf 'BOMBECAM_GATEWAY_IMAGE=%s\nBOMBECAM_FIREWALL_IMAGE=%s\n' "$GW_IMAGE" "$FW_IMAGE" > "$DOTENV"
	chmod 600 "$DOTENV"
	ok "creato $DOTENV"
fi

cd "$(dirname "$COMPOSE")"
services=$(docker compose -f "$COMPOSE" config --services 2>/dev/null || true)
for s in gateway firewall mediamtx; do
	printf '%s\n' "$services" | grep -qx "$s" \
		|| die "il compose in $COMPOSE non dichiara il servizio '$s' (visti: $(printf '%s ' $services))."
done
ok "il compose è valido: $(printf '%s ' $services)"

avail=$(df -Pk / | awk 'NR==2 {print $4}')
[ -n "$avail" ] || avail=0
if [ "$avail" -lt 1500000 ]; then
	die "spazio insufficiente su /: $((avail/1024)) MB liberi, servono ~2 GB per due build."
elif [ "$avail" -lt 3000000 ]; then
	warn "spazio libero su /: $((avail/1024)) MB (due build possono occupare ~2 GB; a fine script la cache viene liberata)."
fi

say "branch:    $BRANCH"
say "regione:   $REGION"
say "compose:   $COMPOSE"
say "recordings: $RECORDINGS_DIR"
say "firewall:  $FW_CONTAINER  ($FW_IMAGE)  ->  $FW_IMAGE_TAG"
say "gateway:   $GW_CONTAINER  ($GW_IMAGE)  ->  $GW_IMAGE_TAG"

# --- 1. sorgente -------------------------------------------------------------
step "1/8  Scarico $BRANCH dal fork"

mkdir -p "$BUILD_BASE"
rm -rf "$SRC"
tarball="$BUILD_BASE/bombecam-backends.tar.gz"
rm -f "$tarball"

ARCHIVE_URL="https://github.com/oxi-git/BombeCam/archive/refs/heads/$BRANCH.tar.gz"
if command -v curl >/dev/null 2>&1; then
	curl -fL --retry 3 --connect-timeout 15 -o "$tarball" "$ARCHIVE_URL"
else
	wget -O "$tarball" "$ARCHIVE_URL"
fi
tar -xzf "$tarball" -C "$BUILD_BASE"
rm -f "$tarball"

[ -d "$SRC" ] || die "estrazione fallita: $SRC non esiste."

# Verifiche specifiche per feature del branch
if [ "$BRANCH" = "feat/firewall-backends" ]; then
	grep -q 'api/v1/net/status' "$SRC/cmd/bombecam-net/main.go" || die "il branch scaricato non contiene l'API di status del demone."
	grep -q 'privacy/backend' "$SRC/cmd/bombecam-gateway/api_handlers.go" || die "il branch scaricato non contiene il selettore backend."
	grep -q 'BOMBECAM_LOCAL_FIREWALL_URL' "$SRC/pkg/netlocal/netlocal.go" || die "il branch scaricato non contiene il client del demone locale."
else
	# Branch main o altri: verifiche generiche
	grep -q 'api/v1/recordings' "$SRC/cmd/bombecam-gateway/api_handlers.go" || warn "il branch non espone /api/v1/recordings (feature recordings non presente)."
	grep -q 'RecordDir' "$SRC/internal/mediamtx/config.go" || warn "il branch non supporta RecordDir in MediaMTX."
fi
ok "sorgente pronto: $SRC"

# --- 2. chiave Osaio ---------------------------------------------------------
step "2/8  Chiave Osaio"

onb=$(docker exec "$GW_CONTAINER" sh -c 'wget -qO- http://127.0.0.1:8654/api/v1/onboarding/status' 2>/dev/null || true)
src_of() { # nome oggetto JSON -> valore di "source"
	printf '%s' "$onb" | sed -n "s/.*\"$1\":{\([^}]*\)}.*/\1/p" | sed -n 's/.*"source":"\([^"]*\)".*/\1/p'
}
say "provenienza: server_key=$(src_of server_key) app_id=$(src_of app_id)  (informativo)"

OLD_BLOBS=""
BLOBS_KEPT=""

# Riuso il setup di una build precedente in BUILD_BASE.
if [ ! -f "$SRC/osaio-setup.txt" ]; then
	for old in "$BUILD_BASE"/BombeCam-*/osaio-setup.txt; do
		[ -f "$old" ] || continue
		cp -p "$old" "$SRC/osaio-setup.txt"
		chmod 600 "$SRC/osaio-setup.txt"
		ok "riuso $old"
		break
	done
fi

# Estraggo il binario dell'immagine in uso: serve sia per i blob da confrontare
# sia per recuperare i valori.
tmpd=$(mktemp -d)
cid=$(docker create "$GW_IMAGE")
if ! docker cp "$cid:/usr/local/bin/bombecam-gateway" "$tmpd/gw" >/dev/null 2>&1; then
	docker rm "$cid" >/dev/null 2>&1 || true
	rm -rf "$tmpd"
	die "non riesco a estrarre /usr/local/bin/bombecam-gateway da $GW_IMAGE."
fi
docker rm "$cid" >/dev/null 2>&1 || true
OLD_BLOBS=$(grep -a -o 'obs1\.[A-Za-z0-9_-]*' "$tmpd/gw" | grep -v '^obs1\.$' | sort -u || true)

if [ -f "$SRC/osaio-setup.txt" ]; then
	ok "chiave e app ID nel sorgente: osaio-setup.txt"
	BLOBS_KEPT=1
elif [ -z "$OLD_BLOBS" ]; then
	ok "l'immagine in uso non ha valori compilati dentro: chiave in file/ambiente, la build non la perde"
else
	say "  l'immagine in uso ha la chiave compilata dentro: va recuperata dal binario."
	rm -rf "$tmpd"
	# Il recupero vive in ha-bombecam-recupera-chiave.sh: usa docker cp + docker
	# exec, perché il daemon dell'host non può montare /root (radice read-only).
	RECUPERA="$(cd "$(dirname "$0")" && pwd)/ha-bombecam-recupera-chiave.sh"
	if [ -x "$RECUPERA" ]; then
		say "  eseguo $RECUPERA"
		if ! OUT_DIR="$BUILD_BASE/BombeCam-backup" "$RECUPERA"; then
			die "recupero della chiave fallito. Il gateway non è stato toccato."
		fi
	else
		warn "manca $RECUPERA accanto a questo script."
	fi

	if [ -f "$BUILD_BASE/BombeCam-backup/osaio-setup.txt" ]; then
		install -m 600 "$BUILD_BASE/BombeCam-backup/osaio-setup.txt" "$SRC/osaio-setup.txt"
		ok "chiave recuperata: $SRC/osaio-setup.txt (mode 600)"
		BLOBS_KEPT=1
	elif [ "${ALLOW_MISSING_KEY:-0}" = "1" ]; then
		warn "ALLOW_MISSING_KEY=1: proseguo senza chiave. Dovrai reinserirla in Settings (Server key / App ID)."
	else
		die "chiave Osaio non recuperata. Il container attuale non è stato toccato. Metti ha-bombecam-recupera-chiave.sh accanto a questo script e rilancia."
	fi
fi
rm -rf "$tmpd"

# --- 3. firewall -------------------------------------------------------------
step "3/8  Compilo $FW_IMAGE_TAG (demone con API di configurazione)"

cd "$SRC"
docker build -f deploy/Dockerfile.firewall -t "$FW_IMAGE_TAG" .
cd - >/dev/null
docker image inspect "$FW_IMAGE_TAG" >/dev/null 2>&1 || die "immagine $FW_IMAGE_TAG non creata."
if docker run --rm --entrypoint sh "$FW_IMAGE_TAG" \
	-c 'grep -q "api/v1/net/status" /usr/local/bin/bombecam-net' >/dev/null 2>&1; then
	ok "immagine firewall costruita, binario con /api/v1/net/status"
else
	die "il binario nell'immagine non contiene /api/v1/net/status: build non aggiornata."
fi

# --- 4. gateway --------------------------------------------------------------
step "4/8  Compilo $GW_IMAGE_TAG (qualche minuto)"

cd "$SRC"
# Il Dockerfile upstream usa 'FROM --platform=$BUILDPLATFORM' e una stage
# 'release' con 'RUN --mount': servono BuildKit. Se il builder e' quello
# legacy, si ricade su una variante senza platform e senza quella stage, che
# compila per l'architettura dell'host (quella giusta per HA).
build_ok=0
if docker build -f deploy/Dockerfile.gateway -t "$GW_IMAGE_TAG" .; then
	build_ok=1
else
	warn "build con BuildKit fallito: riprovo con un Dockerfile ridotto (builder legacy)."
	alt="deploy/.Dockerfile.gateway.nobuildkit"
	sed 's/--platform=\$BUILDPLATFORM //g' deploy/Dockerfile.gateway \
		| awk '/^FROM .* AS release$/ {skip=1; next} skip && /^FROM / {skip=0} !skip {print}' > "$alt"
	if docker build -f "$alt" -t "$GW_IMAGE_TAG" .; then
		build_ok=1
	fi
	rm -f "$alt"
fi
[ "$build_ok" = "1" ] || die "build del gateway fallita."
cd - >/dev/null
docker image inspect "$GW_IMAGE_TAG" >/dev/null 2>&1 || die "immagine $GW_IMAGE_TAG non creata."

# Controllo di andata e ritorno: la nuova immagine deve contenere gli stessi
# valori offuscati di quella in uso. Se ne manca uno, la chiave non e' quella.
if [ -n "$OLD_BLOBS" ] && [ "$BLOBS_KEPT" = "1" ]; then
	tmpd=$(mktemp -d)
	cid=$(docker create "$GW_IMAGE_TAG")
	docker cp "$cid:/usr/local/bin/bombecam-gateway" "$tmpd/gw" >/dev/null 2>&1 || {
		docker rm "$cid" >/dev/null 2>&1 || true; rm -rf "$tmpd"
		die "non riesco a estrarre il binario dalla nuova immagine."
	}
	docker rm "$cid" >/dev/null 2>&1 || true
	grep -a -o 'obs1\.[A-Za-z0-9_-]*' "$tmpd/gw" | grep -v '^obs1\.$' | sort -u > "$tmpd/new"
	printf '%s\n' "$OLD_BLOBS" | sort -u > "$tmpd/old"
	grep -vxF -f "$tmpd/new" "$tmpd/old" > "$tmpd/missing" || true
	rm -rf "$tmpd/gw"
	if [ -s "$tmpd/missing" ]; then
		warn "la nuova immagine non contiene i valori offuscati di quella in uso:"
		sed 's/^/    /' "$tmpd/missing" >&2
		rm -rf "$tmpd"
		die "build scartata. Container e volumi non toccati. Controlla $SRC/osaio-setup.txt."
	fi
	rm -rf "$tmpd"
	ok "chiave Osaio identica a quella in uso (verifica andata/ritorno)"
fi

# --- 5. riavvio --------------------------------------------------------------
step "5/8  Ricreo gateway e firewall (volumi intatti)"

set_tag() { # FILE CHIAVE VALORE
	if grep -qE "^$2=" "$1"; then
		grep -qxF "$2=$3" "$1" && return 0
		cp -p "$1" "$1.bak.$(date +%Y%m%d-%H%M%S)"
		tmpf=$(mktemp)
		sed "s|^$2=.*|$2=$3|" "$1" > "$tmpf"
		cat "$tmpf" > "$1"
		rm -f "$tmpf"
	else
		cp -p "$1" "$1.bak.$(date +%Y%m%d-%H%M%S)"
		printf '%s=%s\n' "$2" "$3" >> "$1"
	fi
	ok "$(basename "$1"): $2=$3"
}

set_tag "$DOTENV" BOMBECAM_GATEWAY_IMAGE "$GW_IMAGE_TAG"
set_tag "$DOTENV" BOMBECAM_FIREWALL_IMAGE "$FW_IMAGE_TAG"

# Nomi con cui si apre BombeCam oltre agli IP (es. un reverse proxy HTTPS).
# Senza, il gateway risponde 403 forbidden_host: un nome DNS non è un IP.
ALLOWED_HOSTS="${ALLOWED_HOSTS:-bombecam.home.arpa}"
if [ -n "$ALLOWED_HOSTS" ] && ! grep -qE '^BOMBECAM_ALLOWED_HOSTS=' "$(dirname "$COMPOSE")/gateway.env"; then
	cp -p "$(dirname "$COMPOSE")/gateway.env" "$(dirname "$COMPOSE")/gateway.env.bak.$(date +%Y%m%d-%H%M%S)"
	printf 'BOMBECAM_ALLOWED_HOSTS=%s\n' "$ALLOWED_HOSTS" >> "$(dirname "$COMPOSE")/gateway.env"
	ok "gateway.env: BOMBECAM_ALLOWED_HOSTS=$ALLOWED_HOSTS"
fi

# Crea volume recordings se non esiste
docker volume inspect bombecam_recordings >/dev/null 2>&1 || {
	docker volume create bombecam_recordings >/dev/null
	ok "volume bombecam_recordings creato"
}

say "  i volumi bombecam_state, bombecam_bombecam_fw e bombecam_recordings restano (dichiarati external)."
cd "$(dirname "$COMPOSE")"
pull=()
if docker compose up --help 2>&1 | grep -q -- '--pull'; then
	pull=(--pull never)
fi

# Fotografia dell'ambiente attuale: dopo il riavvio deve essere identica.
env_snapshot() { docker inspect "$1" --format '{{range .Config.Env}}{{println .}}{{end}}' | sort -u; }
env_snapshot "$GW_CONTAINER" > /tmp/bc-env-gw.prima
env_snapshot "$FW_CONTAINER" > /tmp/bc-env-fw.prima

docker compose -f "$COMPOSE" up -d --no-deps ${pull[@]+"${pull[@]}"} gateway firewall
cd - >/dev/null

for pair in "$GW_CONTAINER:/tmp/bc-env-gw.prima" "$FW_CONTAINER:/tmp/bc-env-fw.prima"; do
	c="${pair%%:*}"; f="${pair#*:}"
	svc="${c#bombecam-}"
	env_snapshot "$c" > /tmp/bc-env-dopo
	# Ogni variabile che c'era prima deve esserci ancora, con lo stesso valore.
	# Variabili nuove aggiunte dall'immagine non sono un problema.
	persi=$(grep -vxF -f /tmp/bc-env-dopo "$f" || true)
	if [ -n "$persi" ]; then
		warn "l'ambiente di $c non corrisponde più a quello di prima (solo i nomi):"
		printf '%s\n' "$persi" | sed 's/^\([^=]*\)=.*/    \1/' | head -20 >&2
		warn "se un valore è stato tagliato: correggi $svc.env e rilancia l'up del solo servizio."
	else
		ok "ambiente di $c conservato ($(wc -l < "$f" | tr -d ' ') variabili, valori non stampati)"
	fi
done
rm -f /tmp/bc-env-dopo /tmp/bc-env-gw.prima /tmp/bc-env-fw.prima

wait_logs() { # container pattern
	i=0
	until docker logs "$1" 2>&1 | grep -q "$2"; do
		i=$((i + 1))
		# 150 s: su un hardware lento il bind del listener arriva solo dopo
		# l'inizializzazione del profilo, non entro la vecchia finestra di 60 s.
		[ "$i" -lt 150 ] || return 1
		sleep 1
	done
}

# Una risposta dall'API vale quanto la riga dei log: se il bind arriva tardi
# o la riga non è apparsa ancora in docker logs, questo conferma il gateway.
if ! wait_logs "$GW_CONTAINER" 'Gateway\|listening\|8654'; then
	warn "la riga dei log del gateway non è arrivata in tempo; provo l'API"
	up=no
	for _ in $(seq 1 30); do
		if docker exec "$GW_CONTAINER" 			wget -sSO- --timeout=2 http://127.0.0.1:8654/api/v1/onboarding/status 2>/dev/null | grep -q .; then
			up=yes
			break
		fi
		sleep 1
	done
	[ "$up" = yes ] || {
		warn "log gateway:"
		docker logs --tail 40 "$GW_CONTAINER" || true
		die "il gateway non è partito."
	}
fi
ok "gateway in esecuzione"

# --- 6. verifica API demone --------------------------------------------------
step "6/8  Verifico l'API del demone"

status=""
for _ in $(seq 1 30); do
	status=$(docker exec "$FW_CONTAINER" \
		sh -c 'wget -qO- http://127.0.0.1:8653/api/v1/net/status' 2>/dev/null || true)
	[ -n "$status" ] && break
	sleep 1
done
[ -n "$status" ] || {
	docker logs --tail 40 "$FW_CONTAINER" || true
	die "GET /api/v1/net/status non risponde su 127.0.0.1:8653."
}

for field in '"block_cloud_video"' '"applied"' '"ruleset_hash"' '"headline"'; do
	grep -q "$field" <<<"$status" || {
		printf '%s\n' "$status" >&2
		die "la risposta di /api/v1/net/status non contiene $field: immagine vecchia?"
	}
done
ok "status espone block_cloud_video, applied, ruleset_hash, headline"
say "  $status"

# --- 7. backend locale + regione --------------------------------------------
step "7/8  Imposto regione $REGION e backend locale"

post_json() { # path body -> "body\nHTTPCODE"
	if docker exec "$GW_CONTAINER" sh -c 'command -v curl >/dev/null 2>&1' 2>/dev/null; then
		docker exec "$GW_CONTAINER" curl -s -m 15 -w '\n%{http_code}' \
			-X POST -H 'Content-Type: application/json' -d "$2" "http://127.0.0.1:8654$1" 2>/dev/null
		return 0
	fi
	docker run --rm --network host alpine:3.22 sh -c '
apk add --no-cache -q curl >/dev/null 2>&1 || exit 3
curl -s -m 15 -w "\n%{http_code}" -X POST -H "Content-Type: application/json" \
	-d "$2" "http://127.0.0.1:8654$1"
' bc "$1" "$2" 2>/dev/null
}

if [ "$SWITCH" = "1" ]; then
	out=$(post_json /api/v1/privacy/region "{\"region\":\"$REGION\"}" || true)
	code=$(printf '%s' "$out" | tail -n1)
	say "  region -> HTTP ${code:-?} $(printf '%s' "$out" | sed '$d' | head -c 200)"

	out=$(post_json /api/v1/privacy/backend '{"backend":"local"}' || true)
	code=$(printf '%s' "$out" | tail -n1)
	say "  backend -> HTTP ${code:-?} $(printf '%s' "$out" | sed '$d' | head -c 200)"

	case "${code:-000}" in
	200) ok "backend locale impostato" ;;
	000) warn "chiamata HTTP non riuscita: imposta backend e regione dall'UI (vedi sotto)." ;;
	401|403) warn "il gateway richiede il login per cambiare backend: fallo dall'UI (vedi sotto)." ;;
	*) warn "risposta inattesa dal gateway (HTTP ${code}): controlla l'UI." ;;
	esac

	sleep 3
	privacy=$(docker exec "$GW_CONTAINER" sh -c 'wget -qO- http://127.0.0.1:8654/api/v1/privacy' 2>/dev/null || true)
	if [ -n "$privacy" ]; then
		grep -q '"backend":"local"' <<<"$privacy" && ok "il gateway riporta backend=local" \
			|| warn "il gateway non riporta ancora backend=local (fallo dall'UI)."
		grep -q '"in_sync":true' <<<"$privacy" && ok "in_sync=true: demone e scelte concordano" \
			|| warn "in_sync non è true: apri il Firewall e premi 'Check firewall'."
	else
		warn "/api/v1/privacy non leggibile senza login: verifica dall'UI."
	fi
else
	say "  SWITCH=0: nessuna modifica. Fai tu dall'UI."
fi

# --- 8. verifica recordings API (se presente nel branch) --------------------
step "8/8  Verifico API recordings e riepilogo"

# Test API recordings se il branch lo supporta
if docker exec "$GW_CONTAINER" sh -c 'wget -qO- http://127.0.0.1:8654/api/v1/recordings' 2>/dev/null | grep -q '\['; then
	ok "GET /api/v1/recordings risponde (lista registrazioni)"
else
	warn "GET /api/v1/recordings non risponde o vuoto (branch potrebbe non avere la feature)"
fi

# --- 9. riepilogo ------------------------------------------------------------
say ""
say "--- log firewall (ultime righe) ---"
docker logs --tail 12 "$FW_CONTAINER" 2>&1 || true
say ""
say "--- log gateway (ultime righe) ---"
docker logs --tail 12 "$GW_CONTAINER" 2>&1 || true
say ""
say "--- regole in vigore ---"
docker exec "$FW_CONTAINER" sh -c \
	"nft list table inet bombecam | grep -E 'chain |ether saddr|dport 8883|dport 443|drop|set (cameras|control_v4|control_2_v4|stream_setup_v4)'" \
	|| warn "lettura regole non riuscita"

say ""
say "--- cache di build (liberata) ---"
docker builder prune -f >/dev/null 2>&1 && say "build cache liberata" || true

say ""
say "============================================================"
say "Fatto: BombeCam aggiornato al branch '$BRANCH'."
say "============================================================"
say ""
say "Il compose ora vive in $COMPOSE (persistente) e i segreti in"
say "$(dirname "$COMPOSE")/*.env (mode 600)."
say "Volume registrazioni: bombecam_recordings -> $RECORDINGS_DIR"
say ""
if [ "$SWITCH" = "1" ]; then
	say "Se il cambio backend non è passato via API, fallo dall'UI:"
	say "  http://<IP-HA>:8654  ->  Firewall  ->  Where the firewall runs: This computer"
	say "                             ->  Osaio region: $REGION  ->  Check firewall"
fi
say ""
say "Rollback (immagine precedente):"
say "  sed -i 's|^BOMBECAM_GATEWAY_IMAGE=.*|BOMBECAM_GATEWAY_IMAGE=$GW_IMAGE|' $DOTENV"
say "  sed -i 's|^BOMBECAM_FIREWALL_IMAGE=.*|BOMBECAM_FIREWALL_IMAGE=$FW_IMAGE|' $DOTENV"
say "  cd $(dirname "$COMPOSE") && docker compose up -d --no-deps gateway firewall"
say ""
say "Controlli utili:"
say "  docker exec $FW_CONTAINER wget -qO- http://127.0.0.1:8653/api/v1/net/status"
say "  docker exec $GW_CONTAINER wget -qO- http://127.0.0.1:8654/api/v1/privacy"
say "  docker exec $GW_CONTAINER wget -qO- http://127.0.0.1:8654/api/v1/recordings"
say "  docker exec $FW_CONTAINER nft list table inet bombecam"
say ""
say "NON usare 'docker compose down -v': i volumi sono external, ma non serve."