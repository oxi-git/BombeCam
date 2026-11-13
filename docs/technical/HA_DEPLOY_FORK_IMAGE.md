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

## 3. Attivare la registrazione (file già pronti)

Nel repo ci sono già tutti i file necessari, così non devi modificarli a mano:

| File | Ruolo |
|---|---|
| `deploy/mediamtx.recording.yml` | Config MediaMTX con `record: yes`: scrive ogni stream in `/recordings/<uuid>/...`, fMP4, segmenti da 1s. |
| `deploy/docker-compose.recording.yml` | Override compose: monta la config che registra al posto di quella di default, condivide il volume `recordings` tra MediaMTX e gateway, e imposta `BOMBECAM_RECORDINGS_DIR=/recordings` sul gateway. |
| `deploy/scripts/up-recording.sh` | Helper: builda l'immagine dal fork e avvia `docker compose` con l'override (e forza `bombecam-gateway:local`, senza toccare `.env`). |

`deploy/mediamtx.yml` e `deploy/docker-compose.yml` restano **invariati**: il test
`TestDeployConfigMatchesGenerated` pretende che `mediamtx.yml` coincida con la
config di default (senza `record`), quindi la registrazione sta nei file appositi.

### Modo veloce (consigliato) — un solo comando

From the fork root on the box that will run the stack (la VM di HA):

```sh
git pull                      # porta l'ultimo codice del fork (deploy incl.)
deploy/scripts/up-recording.sh up
```

Lo script:
1. `docker build -f deploy/Dockerfile.gateway -t bombecam-gateway:local .`
2. `docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.recording.yml up -d`

> Nota su chiavi/app ID: `Dockerfile.gateway` può incorporare la server key e
> l'app ID di Osaio da `osaio-setup.txt`. Se non c'è, al primo avvio la pagina
> web chiede di configurarli (o li imposti in `.env`). Finché usi lo stesso
> volume `gateway_state` di prima, i valori salvati nelle impostazioni restano
> validi.

### Se costruisci su un altro computer

```sh
docker build -f deploy/Dockerfile.gateway -t bombecam-gateway:local .
docker save bombecam-gateway:local | ssh <vm-ha> docker load
```

poi sulla VM carichi i file `deploy/mediamtx.recording.yml`,
`deploy/docker-compose.recording.yml` (che stanno nella copia del fork) e sale
lo stack:

```sh
BOMBECAM_GATEWAY_IMAGE=bombecam-gateway:local \
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.recording.yml up -d
```

### Comandi utili

```sh
deploy/scripts/up-recording.sh logs      # log del gateway + MediaMTX
deploy/scripts/up-recording.sh down      # spegne lo stack (ma NON cancella il volume recordings)
```

---

## 4. Come avviene la registrazione

Nel deploy Docker il gateway **non** supervisiona MediaMTX
(`BOMBECAM_NO_MEDIAMTX_SUPERVISOR: "true"`): è l'override
`deploy/docker-compose.recording.yml` che monta la config che registra e crea
il volume condiviso `recordings`. MediaMTX scrive i file, il gateway li legge
per `/api/v1/recordings` e per riempire `latest_recording_url` nell'endpoint
Home Assistant.

Dopo l'avvio, con una live view o uno stream attivo, MediaMTX inizia a scrivere
segmenti nel volume `recordings` (`<uuid>/...`). L'endpoint del gateway
`/api/v1/recordings` li elenca e `/api/v1/recordings/download?path=...` li
scarica, e l'integrazione Home Assistant espone per ogni camera
`binary_sensor.xxx_recording` con attributo `latest_recording_url`.

> Per ispezionare/ritirare i file: `docker run --rm -it -v bombecam_recordings:/r sh -c 'ls -R /r'`
> (nome volume `bombecam_recordings`, vedi `docker compose config`). In
> alternativa, per tenere i file direttamente su disco, cambia nel file
> `deploy/docker-compose.recording.yml` il mount `recordings:/recordings` (di
> entrambi i servizi) in `./recordings:/recordings`.

---

## 5. Nota sullo storage e sulla modalità "on demand"

- La registrazione cattura solo mentre lo stream è pubblicato. `StreamManager`
  tiene le camere pubblicate 24/7 (runner attivo per ogni camera), quindi una
  volta attivata la registrazione i segmenti vengono prodotti in continuo
  finché lo streaming è in piedi; con l'immagine dal fork quindi è di fatto
  24/7 finché le camere streammano.
- Controlla lo spazio su disco: `.mp4` segmentati da 1s e codec copiate
  crescono in fretta.