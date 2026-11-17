# Untangle: problemi trovati e come si risolvono

Registro delle decisioni per il refactor strutturale del codebase
(skill `untangle-codebase`). Go, 244 file, 30 package, circa 60k righe.

Tre misure del successo: quante special case restano, se un cambio tipico
cade in un solo posto, se ogni import ha senso.

## Problemi trovati

### Hidden links

- `cmd/bombecam-gateway/stream_manager.go` (1488 righe) — `StreamManager` fa
  getter/setter, URL derivati dalla richiesta, credenziali, pubblicazione e
  stato della camera. Le parti cambiano per ragioni diverse ma stanno insieme.
- `cmd/bombecam-gateway/api_handlers.go:23` — `SetupAPIMux` costruisce il mux
  con handler di domini diversi (`csrfHandler`, `tokenHandler`, ...) in un solo
  file da 2143 righe.
- `cmd/bombecam-gateway/privacy_handlers.go:52,56,59` — stato di modulo
  (`routerMu`, `routerLast`, `localFirewallMu`) letto e scritto 36 volte, tutte
  nello stesso file: un mini-stato senza proprietario.
- Il blocco "valida Host → scrivi `forbidden_host` → return" è copiato in
  `api_handlers.go:44,88`, `main.go:396` e `pkg/auth/operator.go:375`.
  Cambia il messaggio o il codice e quattro posti restano indietro.

### Copie di valori

- `"block_cloud_video"` è un letterale ripetuto in 8 package: `pkg/policy`,
  `pkg/netstack`, `pkg/netlocal`, `pkg/verifier`, `pkg/profile`,
  `cmd/bombecam-net`, `cmd/bombecam-gateway` (privacy/onboarding), `tests/e2e`.
  Cambia la chiave e tutti si rompono senza avviso.
- `"/api/v1/cameras/"` costruito a mano in `stream_manager.go:618`,
  `snapshot.go:199,276`, `api_handlers.go:419,423`.
- 63 risposte JSON scritte a mano (header `Content-Type` + `WriteHeader` +
  `Encode`) dove esiste già `writeJSON` (`cameras_lifecycle.go:34`, usato 155
  volte). Concentrate in `api_handlers.go` (38) e `onboarding_handlers.go` (18).

### Surplus

- `main.go:306 startControlAPI` — nessun chiamante, nessun test; il server
  parte da `serveControlAPI` (`main.go:755`). **Rimosso.**
- `pkg/bridge/media.go:1387 onMessage` — wrapper pass-through attorno a
  `OnMessage`, nessun chiamante. **Rimosso.**
- `main.go:750 time.Sleep(250ms)` prima di `showPage` — il listener è già in
  ascolto, l'attesa non serve. **Rimossa.**

### Special cases

- Il `tokenHandler` è escluso dalla validazione del middleware
  (`publicAPIPaths`, `admin_handlers.go:63`) e si rifà i controlli a mano.
  Intenzionale: valida Bearer *e* cookie con regole sue. Documentato, non tolto.

## Soluzioni

### 1. Un solo punto scrive le risposte JSON

`writeJSON` esiste ed è già usato 155 volte: il problema è la duplicazione,
non l'API. Sostituire le 63 scritture a mano con `writeJSON`.
- risolve: le 63 risposte JSON scritte a mano.

### 2. Le chiavi del protocollo diventano costanti

Una costante per i valori ripetuti (`block_cloud_video`, i percorsi API), nel
package che li pubblica; gli altri la importano.
- risolve: le copie del letterale `"block_cloud_video"`.

### 3. Un solo punto valida la sicurezza

`OperatorManager.ValidateSecurityBoundary` copre host, origin e credenziali, e
il middleware la chiama per ogni richiesta. I controlli ripetuti vanno
sostituiti da quella.
- risolve: le 3 copie più la quarta in `pkg/auth/operator.go`.

### 4. `StreamManager` si divide per ragione di cambiamento

`stream_manager.go` (stato e ciclo di vita), URL derivati dalla richiesta,
credenziali. Tre file, tre ragioni.
- risolve: 1488 righe con tre ragioni di cambiare.

### 5. Lo stato del router non è più globale

`routerMu`/`routerLast`/`localFirewallMu` diventano una struct `routerState`
con i suoi metodi.
- risolve: il mini-stato globale senza proprietario.

## I test restano fino alla fine

Le modifiche 1-3 sono meccaniche e già coperte dai test esistenti.
I punti 4 e 5 vanno verificati con la suite intera prima e dopo.

## SetupAPIMux diviso per dominio

`SetupAPIMux` era una funzione da 1.230 righe con 66 `HandleFunc`, un
multiplexer a stringa per `/api/v1/cameras/...` e 14 handler legacy inline.

Ora `api_routes.go` contiene solo le registrazioni, una funzione per dominio:

- `registerOperatorRoutes` — sessione operatore, CSRF, token API
- `registerOnboardingRoutes` — prima configurazione, login Osaio, camere
- `registerPrivacyRoutes` — allowlist del router, avvio/arresto gateway
- `registerIntegrationRoutes` — Frigate, Home Assistant
- `registerRecordingRoutes` — registrazioni locali
- `registerCameraRoutes` — route REST per camera
- `registerLegacyRoutes` — le route pre-`/api/v1`

`api_handlers.go` tiene solo gli handler dei domini camera.

I 14 handler legacy, prima chiusure di 50 righe dentro `SetupAPIMux`, sono
ora funzioni nominate: `handleLegacyPTZ`, `handleLegacyIR`, `handleLegacyLED`,
`handleLegacyLight`, `handleLegacyMic`, `handleLegacyCommand`,
`handleLegacyTalkStart/Stop/Stream`, `handleLegacyStatus`,
`handleLegacyUnlock`, `handleLegacyMode`, `handleLegacyIRStatus`,
`handleLegacyLEDStatus`.

Spariti tre blocchi ripetuti:

- `allowLegacyPost(w, r, signIn)` — POST obbligatorio più security boundary, in
  ogni route legacy. Prima ogni handler ripeteva le stesse quattro righe, e il
  `true` o `signInRequired(r)` era copiato a mano.
- `legacyOnOff(s)` — il vocabolario on/1/true (e vuoto) contro off/0/false,
  prima ripetuto in `/led`, `/light` e `/mic`, ognuno con una variante propria.
- `ptzDirection(s)` — le direzioni left/1 … stop/0, prima solo in `/ptz`.

`handleOperatorToken` è stato estratto da `tokenHandler`: era una chiusura da
140 righe dentro `SetupAPIMux`.

Le risposte JSON scritte a mano in `api_handlers.go` e `onboarding_handlers.go`
sono passate a `writeJSON` (21 blocchi). Restano solo quelle con un
`Content-Type` diverso (`image/jpeg`, `application/sdp`, `text/yaml`).

Effetto collaterale da ricordare: nel riscrivere `/mic` ho invertito la
lettura di `?enable=`. `TestAPI_SettingSwitchesUseControlChannel` l'ha
beccato subito, ed è la ragione per cui la fase Tests va fatta per ultima e
con i test verdi come rete.
