# CONTEXT — Return-channel blackout sui tunnel mpquic di IBLEA-M

**Data:** 2026-07-22
**Nave:** IBLEA-M
**Autore analisi:** tech-lead tbox (Claude Code), su richiesta operativa. Diagnosi live, read-only prima di ogni fix; fix applicati dal loop principale con backup.
**Destinatario:** team mpquic.

---

## TL;DR

Le sessioni TCP (ssh, https/LuCI) verso IBLEA-M via ZeroTier cadevano dopo pochi secondi mentre il ping restava vivo. Causa **non** OpenWrt: è un **blackout del canale di ritorno** sul VPS. Il moon ZeroTier risponde, il VPS de-NAT-a la risposta all'IP `/30` di OpenWrt (`172.16.6.2`) ma la instrada su **eth0 (Internet)** invece che nel tunnel, perché lo script di return-route della flotta (`mpquic-vps-routes.sh`) **non copriva i tunnel di IBLEA-M in modo persistente** (manca `mp1`/BOND1, e la rotta di `mpq6` sparisce a ogni flap del tunnel senza essere ripristinata). Ho messo dei cerotti operativi che rendono l'accesso utilizzabile; **il fix strutturale e la causa dei flap dei tunnel sono per voi.**

L'anomalia che l'utente segnala oggi — *"il traffico via mpq6 va, ma se pingo `10.200.6.254` a un certo punto il ping s'interrompe"* — è la manifestazione diretta di questo: **il tunnel mpq6 flappa**, e il flap è la radice di tutta la catena.

---

## AGGIORNAMENTO 2026-07-22 — causa radice del flap trovata (analisi lato server)

Il flap di `mpq6` **non è** Starlink né una path-migration QUIC. È il **watchdog di servizio che restarta l'istanza in loop**. Diagnosi read-only sul VPS, evidenze qui sotto.

**Cosa si vede.** In 30 minuti: `mpquic@6` restartato **22 volte** (~1/min), istanze `mpquic@1..5` e `mpquic@mp1` **0 volte**. Il timer `mpquic-server-watchdog.timer` gira ogni 60s; l'ultimo run coincide al secondo con il restart (es. 11:30:30). I restart li fa il watchdog via `systemctl restart mpquic@6` (per questo `NRestarts=0`: il restart esterno azzera il contatore).

**Il loop (latch che non si autocancella).** `mpquic-server-watchdog.sh` ha una condizione di restart:
```
journalctl -u mpquic@6 --since '-90 seconds' | grep -q 'write tun: input/output error'  → restart
```
La finestra di lookback è **90s** ma il timer gira ogni **60s**. Ogni restart genera lui stesso dei `write tun: input/output error` (le write sul TUN falliscono mentre l'interfaccia si chiude/ricrea). Al giro dopo, quell'errore è ancora dentro la finestra dei 90s → restart di nuovo → nuovo errore → **non esce più**. Basta un singolo errore transitorio iniziale (un vero micro-outage Starlink su `mpq6`) per innescarlo, poi si automantiene all'infinito anche a Starlink perfetto. Le altre istanze non ci sono mai entrate perché non hanno mai avuto l'errore-seme.

**Secondo difetto, indipendente.** Lo stesso watchdog chiama `ensure_tun.sh` come precondizione **su ogni tunnel sano, ogni 60s**. `ensure_tun.sh` fa `ip link set <tun> down` + `ip tuntap del` sul TUN **vivo**: la del fallisce (fd tenuto dal processo) ma il `down` passa → bounce down/up dell'interfaccia ogni minuto → flush della rotta connessa e write che falliscono. È generatore aggiuntivo di `input/output error`.

**Perché un restart uccide il ritorno.** Ad ogni restart: stop → `ExecStopPost` mette `mpq6` down → `ExecStartPre` `ensure_tun.sh` fa `del`+`add` del TUN → la rotta `172.16.6.0/30 dev mpq6` viene **flushata** dal kernel (ecco perché sparisce; il service oneshot `mpquic-vps-routes` non la ripristina) → il processo riparte con **zero sessioni** → per ~20s `dispatch failed for dst=10.200.6.1 (no path)`: il server non ha nessun pipe verso il client, quindi **ogni pacchetto di ritorno (incluse le echo-reply del ping) viene droppato**. Questa è esattamente l'anomalia del task #2: `ping 10.200.6.254` che si interrompe a intervalli. La finestra di re-registrazione dei 12 pipe è il buco.

**Fix proposto (in ordine di priorità):**
1. **Rompere il latch del watchdog.** La condizione `grep 'write tun: input/output error'` come trigger di restart va tolta o resa non-latch: finestra di lookback < periodo del timer, e/o soglia di errori sostenuti invece che presenza singola, e/o escludere gli errori attribuibili al restart stesso. Così com'è, un solo errore = restart loop permanente.
2. **Non chiamare `ensure_tun.sh` (down+del) sui TUN vivi e sani.** La precondizione deve ricreare il TUN solo se manca o è down davvero, non fare bounce incondizionato ogni 60s.
3. **Persistenza rotte event-driven (task #3).** `ExecStartPost=/usr/local/sbin/mpquic-vps-routes.sh` sul template `mpquic@.service`: parte ad ogni start del tunnel, ripristina la rotta subito, e rende inutile il route-keeper a polling 2s.

**Mitigazione operativa immediata** (rompe il loop da sola, reversibile): fermare/disabilitare `mpquic-server-watchdog.timer`. Con il timer fermo, `mpq6` smette di essere restartato e resta su. Da fare insieme al fix #3 così le rotte restano coerenti.

**Bug latente collaterale** trovato leggendo il codice: in `handleRegister` (`cmd/mpquic/stripe_server.go`) `sess.registered++` si incrementava ad **ogni** REGISTER di refresh periodico (ogni 30s per pipe) senza bound. Il campo è di fatto write-only (non viene mai letto), quindi nessun impatto funzionale oggi, ma era una trappola per chi domani ci appoggiasse un check `registered == totalPipes`. **Risolto** (nel sorgente): ora si incrementa solo quando lo slot passa da vuoto a pieno; il valore riflette i pipe distinti attivi. Nessuna urgenza di rideploy (campo inutilizzato), entra col prossimo build.

## VALIDAZIONE fix + chiusura task #4/#5 (2026-07-22, dopo l'applicazione)

**Il loop di restart è rotto — validato.** Dopo il fix, in ~13 min con il timer del watchdog che rigira ogni 60s: `mpquic@6` a PID stabile (0 restart, prima ~22/30min), 8 cicli di watchdog senza toccare nulla, 0 `dispatch failed for dst=10.200.6.1`. Return-path pulito.

**Task #4 — audit copertura `mpquic-vps-routes.sh` (tutte le navi).** Tunnel attivi vs rotte di ritorno: `mpq1..6`, `mp1`, `mt4/mt5/mt6` sono coperti. `mpquic@mt1` gira senza return-route `/30`, ma **non è un buco**: da `deploy/nftables/mpquic-vps.nft` mt1 è un **tunnel di test (`mt1=WAN5-test`)** che maschera la propria subnet di tunnel `10.200.10.0/24`, non una LAN `172.16.x/30` come i tunnel di produzione. Il suo ritorno è la **rotta connessa `10.200.10.0/24 dev mt1`**, gia' creata da `ensure_tun`. Nessuna `RETURN_SUBNETS` per lui (documentato con un commento nel suo `.env`). Ecco perché non andava indovinato un `/30`: gliene avrei messo uno sbagliato.

**Refactor strutturale FATTO.** La mappa `/30`↔tunnel non è più una lista a mano dentro `mpquic-vps-routes.sh`. Ogni `instances/<i>.env` dichiara la sua `RETURN_SUBNETS="..."` e lo script le deriva da lì (`mpquic-vps-routes.sh [istanza]`); `ExecStartPost=-/usr/local/sbin/mpquic-vps-routes.sh %i` sul template installa solo la rotta dell'istanza al suo up. Applicato sul VPS con backup (`.bak-ts012refactor-20260722`, tar `mpquic-instances-backup-ts012refactor-20260722.tgz`) e **route-invariant verificata** (tabella /30 byte-identica prima/dopo). Allineato anche nel repo: `scripts/mpquic-vps-routes.sh`, `scripts/mpquic-server-watchdog.sh`, `deploy/systemd/mpquic@.service`, `deploy/config/server/*.env`. **Nota di drift pre-esistente** (non introdotto ora, da riconciliare a parte): `deploy/config/server/` nel repo è un sotto-insieme del vivo (manca `mp1.env` e `mt1.env`) e alcuni `TUN_CIDR` divergono (repo `6.env`=`10.200.6.2/30` vs vivo `10.200.6.254/24`).

**Task #5 — modello NAT/routing (chiarito).** Sul VPS: `ip_forward=1`, forward-accept esplicite per ogni tunnel↔eth0, e in POSTROUTING un unico `oif "eth0" masquerade`. Quindi il modello è: **la sorgente `/30` del client è preservata fino al VPS; è il VPS a mascherare verso Internet su eth0** (è lui il gateway NAT Internet della flotta). Il client **non** viene mascherato all'IP del tunnel. Conseguenza diretta: le return-route per-`/30` **sono obbligatorie e devono essere persistenti** (il ritorno de-mascherato a `172.16.x.2` va instradato nel tunnel, non su eth0). Il design è coerente: la cura giusta è la persistenza event-driven delle rotte (fatto con `ExecStartPost`), **non** mascherare il client.

**Stato lato server:** causa radice risolta e validata. (a) `mt1` chiarito — tunnel di test, nessuna `/30`, non serve rotta; (b) refactor delle return-route dai config d'istanza **fatto** (VPS + repo, route-invariant verificata); bug latente `sess.registered++` **risolto** nel sorgente. Unico strascico non bloccante: riconciliare `deploy/config/server/` del repo col vivo (env mancanti + CIDR divergenti, drift pre-esistente).

---

## Topologia rilevante (IBLEA-M)

```
[client ZeroTier: PC utente] --ZT--> moon/controller 91.188.12.219 (:9994) <--ZT-- [OpenWrt 10.202.11.2 :9993]
OpenWrt (VM 100) --/30 interni--> mpquic client (VM 200, satcom@10.200.17.1) --tunnel mpquic--> VPS (vps-mpquic-it-tpz-iblea, 172.238.232.223)
```

Mappa WAN↔/30↔tunnel (IBLEA-M):

| WAN (OpenWrt) | /30 interno (OpenWrt src) | iface client | tunnel | IP tunnel client ↔ VPS |
|---|---|---|---|---|
| STARLINK | `172.16.6.2` (eth13) | enp7s2 (`172.16.6.1`) | **mpq6** | `10.200.6.1` ↔ `10.200.6.254` |
| LTE | `172.16.5.2` (eth12) | enp7s1 | mpq5 | `10.200.5.x` ↔ `10.200.5.254` |
| BOND1 (bonded) | `172.16.17.2` (eth8.17) | — | **mp1** | ↔ `10.200.17.254` |

Il moon `91.188.12.219` è il controller ZeroTier self-hosted Telespazio (filtra ICMP: `ping` al moon è sempre 100% loss da qualsiasi WAN — **non** diagnostico, usare i contatori nativi ZT).

---

## Sintomo

- `ping 10.202.11.2` (ZT) dal PC utente: **funziona**, ma latenza che oscilla 70↔200+ ms.
- `ssh` e `https` (LuCI) verso `10.202.11.2`: la **sessione TCP cade dopo pochi secondi** (browser: `ERR_NETWORK_CHANGED`; `ssh + tail -f` va offline dopo qualche secondo).
- ICMP (stateless, bassa frequenza) sopravvive; TCP (bidirezionale) no.

---

## Causa radice (verificata in diretta, con evidenze)

Il traffico ZeroTier di OpenWrt esce dal tunnel con **sorgente il suo IP privato di WAN** (es. `172.16.6.2`), non mascherato all'IP del tunnel. Il VPS lo NAT-a verso Internet (`172.238.232.223:<porta>`) e raggiunge il moon. **Il moon risponde correttamente al VPS.** Il problema è il ritorno:

1. **Il VPS de-NAT-a la risposta a `172.16.6.2` ma la instrada su `eth0` (default Internet), non nel tunnel.** tcpdump sul VPS:
   ```
   eth0 In   91.188.12.219.9994 > 172.238.232.223.54676        # moon → VPS (ok)
   eth0 Out  91.188.12.219.9994 > 172.16.6.2.29743             # de-NAT verso OpenWrt MA su eth0 (INTERNET) → PERSO
   ```
   confronto con un flusso che funziona (sorgente = IP tunnel):
   ```
   mpq6 Out  91.188.12.219.9994 > 10.200.6.1.29743             # ritorno corretto DENTRO mpq6
   ```
   `ip route get 172.16.6.2` sul VPS = `via 172.238.232.1 dev eth0` (dovrebbe essere `dev mpq6`).

2. **Perché manca la rotta.** Lo script `/usr/local/sbin/mpquic-vps-routes.sh` (service `mpquic-vps-routes.service`, oneshot `RemainAfterExit=yes`, gira **solo al boot**) installa:
   - `172.16.1.0/30 … 172.16.6.0/30 dev mpq1…mpq6` (single-link)
   - `172.16.11/12/13.0/30 dev mt4`, `…21/22/23 dev mt5`, `…31/32/33 dev mt6` (altre navi)
   - **NON** ha `172.16.17.0/30 dev mp1` (BOND1) → BOND1 dava **blackout permanente**.
   - La rotta `172.16.6.0/30 dev mpq6` **c'è**, ma è `dev mpq6`: quando **mpq6 flappa** il kernel la rimuove, e il service oneshot **non la ripristina** → blackout finché la rotta non riappare.

3. **Prova del meccanismo.** Aggiungendo a mano `ip route add 172.16.6.0/30 dev mpq6` sul VPS, il ritorno **rientra subito** (contatore nativo ZT `lastReceive` da >120s congelato a <1s). Poco dopo la rotta **torna su eth0** (persa al flap) → blackout di nuovo. L'oscillazione osservata dall'utente (latenza 70↔200ms, sessioni che cadono e rientrano) **è la rotta che va e viene col flap del tunnel.**

Evidenza lato OpenWrt (contatori nativi ZeroTier, `zerotier-cli -j peers`, path verso il moon):
- `lastSend` avanza regolarmente (OpenWrt invia), `lastReceive` **congelato per 6m35s** durante un blackout, path marcato `active/preferred`. Il conntrack del flusso moon su OpenWrt: `[UNREPLIED]`, reverse `packets=0`. → i pacchetti di ritorno **non arrivano** a OpenWrt (drop a monte, sul VPS).

**Nota metodologica per voi:** il ping ICMP al moon è inutilizzabile (il moon lo filtra). L'unico segnale affidabile del ritorno è `lastReceive`/`lastSend` per-path in `zerotier-cli -j peers` (campionare ogni pochi secondi: se `lastReceive` sale mentre `lastSend` resta basso ⇒ blackout del ritorno).

---

## Fix applicati (CEROTTI operativi — da rivedere/sostituire con fix propri)

Tutti reversibili, con backup. Sul **VPS** (`vps-mpquic-it-tpz-iblea`):

1. **Aggiunta la rotta di BOND1 mancante** in `/usr/local/sbin/mpquic-vps-routes.sh`:
   `safe ip route replace 172.16.17.0/30 dev mp1` (backup `…​.bak-bond1-20260722`).
2. **Route-keeper**: nuovo `mpquic-route-keeper.service` che **ri-esegue lo script ogni 2s** (`while true; do mpquic-vps-routes.sh; sleep 2; done`, `Restart=always`), così le rotte perse al flap tornano entro ~2s. È un **polling band-aid**, non la soluzione: serve un ripristino event-driven all'up del tunnel.

Sul **OpenWrt** IBLEA-M (per aggirare il flap di STARLINK, non è la causa ma peggiorava):
3. ZeroTier spostato da STARLINK (`mpq6`, single-link che flappa) a **BOND1** (`mp1`, bonded, più stabile): policy mwan3 dedicata `ZT_CLEAN=BOND1_M1_W1`, e ZeroTier **bindato** a `172.16.17.2` (`/var/lib/zerotier-one/local.conf` → `{"settings":{"bind":["172.16.17.2"]}}`, backup `.bak-star`). Il bind serve perché ZeroTier di default si legava a **tutte** le WAN e spariva da sorgenti miste (172.16.2.2/172.16.5.2) mentre mwan3 instradava su un'altra iface → routing asimmetrico. Con bind singolo, sorgente = uscita.

**Risultato attuale (misurato):**
- STARLINK/mpq6: peggior `lastReceive` **11.786 ms**, blackout permanenti prima del keeper.
- BOND1/mp1: peggior `lastReceive` **~4.700 ms** (per lo più <500ms, 4 campioni/24 sopra 1s su 3 min). Le sessioni TCP ora **sopravvivono** (stalli brevi che rientrano), non muoiono più.

Nessuno di questi tocca la vera causa: **i tunnel mpquic flappano e il ripristino delle rotte non è event-driven.**

---

## Anomalie APERTE — il task per voi

1. **Perché i tunnel mpquic flappano.** `mpq6` (STARLINK single-link) flappa spesso — log mwan3 OpenWrt: `STARLINK offline → connecting → online → disconnecting` a cicli di pochi minuti. `mp1` (BOND1 bonded) è più stabile ma ha comunque hiccup da ~4-5s. È handoff LEO Starlink che il data-plane assorbe ma che rompe control/rotte? È una **riconnessione QUIC** (path migration / re-handshake) del client o del server? Il TUN viene ricreato a ogni riconnessione (e quindi le rotte `dev mpqN` cadono)?

2. **L'anomalia segnalata oggi (importante, riproducibile):** *il traffico via `mpq6` passa, ma `ping 10.200.6.254` (IP VPS del tunnel) s'interrompe a intervalli.* Cioè: **il data-plane del tunnel funziona ma l'ICMP verso il peer del tunnel si ferma.** Ipotesi da verificare: durante la riconnessione/flap il peer TUN è brevemente irraggiungibile (ICMP, che serve round-trip immediato, lo vede) mentre i flussi dati bufferizzati/FEC rientrano; oppure c'è un problema di keepalive/liveness sul control-path del tunnel indipendente dal data-path. **Questo è il modo più pulito per riprodurre e cronometrare il flap**: `ping -D 10.200.6.254` dal client e correlare le interruzioni con i log dello scheduler mpquic (client e server) e con `lastReceive` di ZeroTier su OpenWrt.

3. **Persistenza delle return-route (bug di design lato server).** `mpquic-vps-routes.service` è oneshot al boot: quando un tunnel si ri-crea, la rotta `dev mpqN`/`dev mp1` sparisce e **non viene ripristinata**. Fix proprio: ripristinare le rotte **all'evento di up del tunnel** — `ExecStartPost=/usr/local/sbin/mpquic-vps-routes.sh` sul template `mpquic@.service`, o un hook `if-up`/hotplug per-interfaccia — invece del keeper a polling che ho messo io.

4. **Copertura incompleta dello script.** Mancava `mp1` (172.16.17.0/30). **Fare un audit** `mpquic-vps-routes.sh` vs la mappa reale WAN↔/30↔tunnel per **tutte** le navi, non solo IBLEA-M. È il gemello lato-server del bug lato-client già visto e corretto su IBLEA-M (la policy-routing del client mpquic non aveva la rotta di ritorno `/30` — vedi `tbox/docs/TROUBLESHOOTING_HISTORY.md` TS-011).

5. **Modello NAT/routing da chiarire.** Il traffico di OpenWrt arriva al VPS con la **sorgente privata `/30`** (`172.16.6.2`), non mascherata all'IP del tunnel (`10.200.6.1`). È **voluto** (il VPS fa il NAT Internet e quindi gli servono le return-route per-/30) o dovrebbe mascherare il client? Se il modello è "sorgente /30 preservata fino al VPS", allora le return-route per ogni `/30` di ogni nave **devono** esistere ed essere persistenti — cosa che oggi non è garantita.

---

## Accesso / riproduzione

- **VPS:** `ssh vps-mpquic-it-tpz-iblea` (root; l'IPS killa le sessioni non interattive → usare `-tt` per comandi lunghi). Utili: `ip route get 172.16.6.2` / `172.16.17.2`; `systemctl status mpquic-route-keeper mpquic-vps-routes`; `cat /usr/local/sbin/mpquic-vps-routes.sh`; `tcpdump -nni any 'udp and host 91.188.12.219'` (vedere i `Out` su `eth0` invece che su `mpqN` = il bug).
- **OpenWrt IBLEA-M:** `ssh -J vps-mpquic-it-tpz-iblea,satcom@10.200.17.1 root@10.10.10.254`. Utili: `zerotier-cli -j peers` (campionare `lastReceive`/`lastSend` del MOON); `mwan3 status`; `logread | grep mwan3track`.
- **Client mpquic (VM 200):** via Proxmox `ssh -J vps-mpquic-it-tpz-iblea,satcom@10.200.17.1 root@10.10.11.2` → `qm guest exec 200 -- sh -c '...'`. Qui girano i tunnel `mpq*`/`mp1` lato nave; correlare i log dello scheduler con i flap.

## Cosa NON è la causa (già escluso, non ri-verificare)

- Non è OpenWrt (sticky, MTU zt0, policy mwan3, bind: tutti provati, nessuno risolve — layer sbagliato).
- Non è l'hole-punching / il blocco dei PLANET root pubblici (il moon privato è DIRECT e stabile; il blocco dei PLANET su OpenWrt è voluto).
- Non è ICMP verso il moon (filtrato, non diagnostico).
- Non è la stabilità del link fisico Starlink in sé (`ping -I enp7s8 8.8.8.8` = 0% loss): è il **tunnel** sopra, e il **ritorno sul VPS**.
