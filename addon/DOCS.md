# BombeCam

BombeCam mette in locale le telecamere Osaio/Anticore: streaming RTSP, HLS e WebRTC
sul tuo Home Assistant, senza passare dal cloud del produttore, con registrazione
sul disco e ponte già pronto per l'integrazione Home Assistant.

## Cosa include

- **Gateway BombeCam** (il servizio che gestisce le telecamere)
- **MediaMTX** (server streaming locale)
- **Registrazione automatica** di ogni stream in MP4 frammentato
- **Binding già configurato** per l'integrazione
  [`ha-bombecam`](https://github.com/oxi-git/ha-bombecam), senza comandi da
  terminale

## Prima di iniziare

1. Disattiva un'eventuale installazione manuale di BombeCam già in esecuzione
   (`docker compose -p bombecam down`): le porte `8654`, `8554`, `8888` e `8889`
   non possono essere condivise.
2. Prendi nota delle credenziali del tuo account **Osaio** (email e password).
3. L'add-on usa la porta 8654 in chiaro sulla rete locale. Non esporla a
   internet.

## Installazione

1. **Impostazioni → Add-on → Add-on store → ⋮ → Repositories**
2. Aggiungi `https://github.com/oxi-git/BombeCam`
3. Installa **BombeCam** e configuralo
4. Avvia l'add-on
5. Lancia il discovery di Home Assistant: **Impostazioni → Dispositivi e
   servizi → Aggiungi integrazione → BombeCam**

## Configurazione

Opzioni principali, tutte nuovamente modificabili dopo l'installazione:

| Opzione | Descrizione |
|---|---|
| Nome / password amministratore | Crea l'account amministratore di BombeCam. Serve per l'integrazione Home Assistant. |
| Email / password Osaio | Credenziali usate da BombeCam per scoprire e configurare le telecamere. |
| Regione Osaio | Lascia vuoto se non sai quale scegliere. |
| Telecamere da streammare | ID separati da virgola, presi dal discovery autenticato. Lascia vuoto per non avviare alcuno stream. |
| Indirizzo e porta pagina/API | Lascia `0.0.0.0:8654` per permettere all'integrazione Home Assistant di raggiungere il gateway. |
| Trasporto controlli (`cloud` / `mqtt`) | `cloud` è quello stabile. `mqtt` è sperimentale e richiede un broker Mosquitto locale. |
| Registra stream in locale | Attiva per conservare ogni stream come frammenti MP4 nell'archivio `recordings`. |
| Giorni di conservazione | Scaduto il numero di giorni, le registrazioni vengono cancellate. `0` le conserva per sempre. |

## Porte usate (network host)

| Porta | Servizio |
|---|---|
| 8654 | Pagina web e API di BombeCam |
| 8554 | RTSP |
| 8888 | HLS |
| 8889 | WebRTC (TCP) |
| 8189 | WebRTC (UDP) |
| 9997 | API MediaMTX (solo loopback) |

## Integrazione Home Assistant

Una volta trovato il gateway con **Aggiungi integrazione → BombeCam**, l'add-on
chiede solo l'indirizzo del gateway e i credenziali amministratore: il token API
viene generato in automatico, senza `curl` e senza passare dalla riga di comando.

L'esposizione nel contesto Home Assistant comprende per ogni telecamera:

- entità `camera` con lo stream RTSP
- pulsanti PTZ (sinistra, destra, su, giù, stop)
- interruttori luce di stato, rilevamento movimento, rilevamento suono,
  faretto
- selettore visione notturna
- sensore stato stream
- sensore registrazioni disponibili

## Registrazione

Quando la registrazione è attiva, ogni stream viene salvato come MP4
frammentato sotto il percorso `recordings` dell'add-on. Le registrazioni più
vecchie del numero di giorni impostato vengono cancellate automaticamente.
Usa la cronologia dell'entità `sensor.*recordings` per navigare le registrazioni
da Home Assistant.

## Risoluzione problemi

| Problema | Cosa controllare |
|---|---|
| Add-on non parte | Log dell'add-on. Verifica che non ci sia già un BombeCam in esecuzione (porte occupate). |
| Nessuna telecamera trovata | Verifica email/password Osaio e ripeti il discovery autenticato dalla pagina del gateway. |
| `invalid_auth` dall'integrazione | Controlla nome e password amministratore. L'integrazione genera un nuovo token automaticamente tramite re-auth. |
| Stream assente ma telecamera raggiungibile | Avvia prima lo stream dalla pagina web di BombeCam, poi aggiorna lo stato nell'integrazione. |
| Registrazioni vuote | Verifica che `recording_enabled` sia attivo e che `recording_retention_days` non stia già cancellando troppo presto. |

## Disinstallazione

Dalla pagina dell'add-on, usa **Disinstalla**. Le registrazioni restano nel
volume `recordings` finché non cancelli l'add-on anche dal punto di vista dei dati.

## Link

- Sorgenti e segnalazioni: <https://github.com/oxi-git/BombeCam>
- Integrazione Home Assistant: <https://github.com/oxi-git/ha-bombecam>
- Progetto originario: <https://github.com/Fever-r/BombeCam>