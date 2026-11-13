# Aggiornare BombeCam sulla VM di HA con un'immagine dal tuo fork

Questa guida spiega come far girare sulla VM di Home Assistant un'immagine
del gateway **costruita dal tuo fork** (`oxi-git/BombeCam`), così da avere le
funzionalità che non sono ancora nell'immagine pubblica, **inclusa la
registrazione locale** (Registrazione locale) e la sua esposizione
all'integrazione `ha-bombecam`.

> Perché serve: il download pubblicato è
> `ghcr.io/fever-r/bombecam-gateway:1`, che **non** contiene la registrazione.
> Per usare la registrazione (e i campi `recording_available` /
> `latest_recording_url` nell'endpoint Home Assistant) devi costruire
> l'immagine dal tuo fork e farla usare al `docker compose`.

---

## 1. Requisiti

- Docker `buildx` disponibile (Docker Desktop / plugin installato).
- Un clone aggiornato del fork `oxi-git/BombeCam`.
- Sulla VM dove gira Home Assistant: il deploy `deploy/docker-compose.yml`
  (di solito in una sottocartella tipo `bombecam/`). Puoi costruire l'immagine
  sia sulla VM che su un altro computer con accesa copia del codice; nel
  secondo caso devi `docker save`/`docker load` l'immagine (vedi sotto).

---

## 2. Costruire l'immagine dal fork

Dal clone del fork, alla radice del repository:

```sh
docker build -f deploy/Dockerfile.gateway -t bombecam-gateway:local .
```

Il Dockerfile usa `GATEWAY_FROM=source` di default: compila il codice del
fork. Se vuoi provare anche il binario della release resta su `bombecam-gateway:local`
(che è quello con la registrazione).

> Nota su chiavi/app ID: `Dockerfile.gateway` può incorporare la server key
> e l'app ID di Osaio da `osaio-setup.txt` nella cartella sorgente. Se non c'è
> `osaio-setup.txt`, al primo avvio la pagina web chiede di configurarli (oppure
> li imposti in `.env` come già fai). Finché usi lo stesso `gateway_state`
> volume di prima, i valori salvati nelle impostazioni restano validi.

### Se costruisci su un altro computer

Carica l'immagine sulla VM di HA:

```sh
docker save bombecam-gateway:local | ssh <vm-ha> docker load
```

---

## 3. Far usare l'immagine al deploy

Nel file `.env` del deploy in `deploy/`, imposta:

```sh
BOMBECAM_GATEWAY_IMAGE=bombecam-gateway:local
```

Poi aggiorna solo il servizio gateway:

```sh
docker compose up -d --no-deps gateway
```

Verifica che il gateway sia in funzione e raggiungibile sulla porta HTTP
usuale (8654).

---

## 4. Abilitare la registrazione nel deploy Docker

Nel deploy Docker il gateway **non** supervisiona MediaMTX
(`BOMBECAM_NO_MEDIAMTX_SUPERVISOR: "true"`); MediaMTX gira in un container
separato leggendo `deploy/mediamtx.yml`. Quindi, per registrare, vanno fatti
due interventi: dire a MediaMTX di scrivere i file, e dire al gateway dove leggerli.

### 4.1 MediaMTX scrive le registrazioni

In `deploy/mediamtx.yml`, sotto la sezione `paths:`, il blocco `all_others:`
(quello che gestisce ogni stream) diventa:

```yaml
paths:
  all_others:
    record: yes
    recordPath: /recordings/%path/%Y-%m-%d_%H-%M-%S
    recordFormat: fmp4              # un file .mp4 per segmento (1s)
    recordSegmentDuration: 1s
```

### 4.2 Volume condiviso tra MediaMTX e gateway

Aggiungi un volume per le registrazioni al servizio `mediamtx` in
`deploy/docker-compose.yml`:

```yaml
  mediamtx:
    ...
    volumes:
      - ./mediamtx.yml:/mediamtx.yml:ro
      - recordings:/recordings
```

e al servizio `gateway`:

```yaml
  gateway:
    ...
    environment:
      ...
      BOMBECAM_RECORDINGS_DIR: /recordings   # path dentro il container
    volumes:
      - gateway_state:/var/lib/bombecam
      - recordings:/recordings
```

poi dichiara il volume alla fine del file:

```yaml
volumes:
  mosquitto_data:
  gateway_state:
  firewall_state:
  recordings:
```

In alternativa puoi montare una cartella locale invece del volume Docker
(es. `./recordings:/recordings`) se vuoi tenere i file direttamente su disco.

### 4.3 Riavvio

```sh
docker compose up -d --no-deps mediamtx gateway
```

Dopo l'avvio, con una live view o uno stream attivo, MediaMTX inizia a scrivere
segmenti in `recordings/<uuid>/...`. L'endpoint del gateway
`/api/v1/recordings` li elenca e `/api/v1/recordings/download?path=...` li
scarica, e l'integrazione Home Assistant espone per ogni camera il nuovo
`binary_sensor.xxx_recording` con attributo `latest_recording_url`.

---

## 5. Nota sullo storage e sulla modalità "on demand"

- La registrazione cattura solo mentre lo stream è pubblicato. `StreamManager`
  tiene le camere pubblicate 24/7 (runner attivo per ogni camera), quindi una
  volta attivata la registrazione i segmenti vengono prodotti in continuo
  finché lo streaming è in piedi; con l'immagine dal fork quindi è di fatto
  24/7 finché le camere streammano.
- Controlla lo spazio su disco: `.mp4` segmentati da 1s e codec copiate
  crescono in fretta.