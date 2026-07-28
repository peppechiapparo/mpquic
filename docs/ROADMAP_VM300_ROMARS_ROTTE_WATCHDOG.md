# Roadmap: rotte e watchdog sulla VM 300 ROMARS (TBOX-EVO)

Data: 2026-07-27. Autore: sessione Claude Code con Giuseppe Chiapparo.
Stato: **APPLICATA** (fasi 0-4 eseguite il 27 luglio dopo review e risposte ai punti aperti; la Fase 5 di collaudo attende l'installazione del software ROMARS). L'esito dell'applicazione e le decisioni prese sono in fondo al documento.

Obiettivo: permettere a OpenWRT (10.10.11.254) di usare i tunnel della VM 300 per uscire su internet, intervenendo solo su rotte e watchdog. Il software che gestisce i tunnel è di proprietà ROMARS e non va toccato in nessun modo.

Basi di partenza: analisi ROMARS del 16 luglio ([ROMARS_WAN_DHCP_Analisi.md](ROMARS_WAN_DHCP_Analisi.md), TS-001) e le correzioni maturate sui troubleshooting IBLEA-M (TS-013, TS-014, TS-017, TS-018, TS-020, più la [checklist di verifica client](CHECKLIST_VERIFICA_CLIENT_MPQUIC.md)).

## Identità della macchina (verificata alla sorgente)

Regola post incidente del 24 luglio: prima si conferma il VMID, poi si lavora.

Sul Proxmox `TBOX-10237840` (10.10.11.2), `qm list` di oggi:

| VMID | Nome | Stato |
|---|---|---|
| 200 | MPQUIC (nostra) | stopped |
| 300 | MPQ-ROMARS | **running** |

La MAC di `net0` della VM 300 (`BC:24:11:E1:A9:8B`) coincide con `enp6s18` della macchina che risponde su 10.10.11.100 (hostname `mpquic`, Debian 12, boot 2026-07-27 08:02). Nessuna MAC corrisponde alla VM 200 (`bc:24:11:3d:9b:59`). Stiamo quindi parlando con la VM 300, quella giusta.

Nota: la host key SSH di 10.10.11.2 è cambiata rispetto al mio known_hosts (voce 993). Se il Proxmox è stato reinstallato è normale, ma va confermato prima di accettare la chiave nuova in modo definitivo.

## Stato rilevato (ricognizione read-only del 27 luglio)

1. **`wan-watchdog.service` in crash loop**: unit presente e abilitata (è la nostra, datata 11 marzo 08:37), ma `/usr/local/bin` è vuoto e lo script non c'è. Stesso quadro del 16 luglio (TS-001): il servizio non ha mai eseguito una riga.
2. **Default fantasma sulla management**: `01-mgmt1.network` ha `Gateway=10.10.11.1`. Quell'IP non risponde e l'ARP è `INCOMPLETE`; sul Proxmox la VM 101 OPNsense risulta spenta, ed è il candidato più probabile come ex titolare di quell'indirizzo (da confermare, è un'inferenza). Risultato: una default a metrica 0 verso un gateway morto, che vince su ogni uplink DHCP reale (metriche 105/106). È il pattern esatto di TS-018 su IBLEA-M: tutto il traffico instradato che cade nella tabella main finisce in un buco nero. Con questa rotta in piedi, OpenWRT non uscirà mai da questa VM, tunnel o non tunnel.
3. **Policy routing assente**: esiste solo la ip rule 1017 (bd1, statica da `27-bd1.network`). Le rule 1001-1006 (`172.16.N.0/30 → tabella wanN`) non ci sono. Le tabelle wan1..wan6 sono definite in `rt_tables` ma vuote.
4. **Catena di trigger rotta**: l'hook networkd-dispatcher `50-mpquic-auto` (base image, 15 marzo) chiama `/usr/local/lib/mpquic/mpquic-if-event.sh`, che non esiste. In `/usr/local/sbin` c'è un `lan-wan-policy-routing.sh` del 25 febbraio, versione vecchia basata su lease dhclient (qui lo stack è systemd-networkd): niente lo invoca, è inerte.
5. **NAT già pronto**: nftables ha il masquerade per `enp7s3..enp7s8` e per i nomi tunnel standard `mpq1..mpq6`, `mp1`, `cr/br/df/bk`. Su questo fronte non serve nulla.
6. **WAN attive**: `enp7s7` = 100.64.86.226/10, gw 100.64.0.1, metrica 105; `enp7s8` = 9.246.8.61/23, gw 9.246.8.1, metrica 106. `enp7s3..s6` senza IP. Attenzione: il 16 luglio `enp7s7` portava l'LTE (192.168.1.100); oggi ha un indirizzo CGNAT 100.64/10, tipico Starlink. Il cablaggio è cambiato, e i commenti nei file `14-wan5.network`/`15-wan6.network` ("Starlink #1"/"Starlink #2") restano inaffidabili.
7. **Software ROMARS non trovato**: nessuna unit custom oltre a wan-watchdog, nessun processo applicativo (a parte l'agent Wazuh), nessun device TUN, `/opt` vuoto, niente in ascolto oltre a sshd. O il loro software deve ancora essere installato su questo restore, o parte con meccanismi che non ho visto. Da chiarire con te o con ROMARS prima del collaudo.
8. **Lato OpenWRT**: le default per-WAN verso i /30 di transito ci sono già (172.16.1.1 metrica 10, poi .3.1, .4.1, .5.1, .6.1 e 172.16.17.1 metrica 17), mwan3 configurato con policy BONDING/FAILOVER. Manca solo la default verso 172.16.2.1 (WAN2). Al momento non prevedo modifiche su OpenWRT.
9. **Repo nostro**: `scripts/mpquic-policy-routing.sh` è ancora la versione pre TS-020: cancella e riaggiunge le rule statiche 1001-1006 a ogni giro, il bug della finestra di bypass. Il fix idempotente validato sul lab VM 200 il 24 luglio non è mai stato committato (decisione su branch rimasta aperta). Il fix TS-014 (host route disaccoppiata dal TUN) invece è già nel repo.

## Cosa non viene toccato

- Software, binari, configurazioni e servizi ROMARS: niente installazioni, niente restart, niente file loro. Sulla VM oggi non risulta alcun file ROMARS; se durante l'intervento ne comparissero, mi fermo e ti avviso.
- I modem e il lease DHCP del modem LTE (la raccomandazione di abbassarlo resta in carico a ROMARS, come da analisi del 16 luglio).
- `lan-wan-policy-routing.sh`: è inerte e della nostra base image, lo lascio dov'è come riferimento storico.
- La VM 200, che resta spenta.

Tutti i file che la roadmap tocca sono nostri: la unit wan-watchdog (verificata byte per byte identica alla nostra il 16 luglio), i file `.network` della base image Telespazio, gli script del nostro repo.

## Fasi proposte

### Fase 0. Gate di sicurezza (prima di ogni modifica)

- Riconfermare il VMID con `qm list` sul Proxmox nel momento dell'intervento (non fidarsi della ricognizione di oggi: le VM si accendono e spengono).
- Snapshot Proxmox: `qm snapshot 300 pre-rotte-wd-20260727`. Dopo l'incidente del 24 luglio, il rollback rapido non è negoziabile.
- Backup puntuale (`.bak-20260727`) di ogni file prima di modificarlo.

### Fase 1. Repo: fix TS-020 sullo script di routing

Riportare in `scripts/mpquic-policy-routing.sh` il fix idempotente validato sul lab: le rule statiche 1001-1006 aggiunte solo se assenti, `del`+`add` mantenuto solo per le rule che dipendono dal DHCP (1101-1106, 1201-1206). Senza questo fix, ogni evento di rete apre la finestra di bypass di TS-020 e il traffico dei /30 scapperebbe sul fisico invece che nel tunnel.

Va deciso il branch per il commit (punto rimasto aperto dal 24 luglio: `feat/crypto-abstraction-layer` è pieno di modifiche estranee).

### Fase 2. wan-watchdog: installare lo script mancante

Il rimedio già documentato a ROMARS in TS-001, mai applicato perché la VM è rimasta com'era:

```bash
sudo cp scripts/wan-watchdog.sh /usr/local/bin/wan-watchdog.sh   # versione corrente, post b498c63
sudo chmod +x /usr/local/bin/wan-watchdog.sh
sudo systemctl restart wan-watchdog.service
```

La unit non si tocca, con una proposta opzionale: decommentare `Environment=WAN_INTERFACES=enp7s7 enp7s8` per limitare il probe alle due WAN cablate ed evitare churn sulle quattro senza IP (lezione TS-020 sul rumore da watchdog). Verifica: `journalctl -t wan-watchdog -f`, il crash loop `203/EXEC` deve sparire e devono comparire i check sui gateway.

### Fase 3. Rimozione della default fantasma (TS-018)

Togliere la riga `Gateway=10.10.11.1` da `/etc/systemd/network/01-mgmt1.network` (backup prima), poi `networkctl reload` e flush della rotta viva. La subnet 10.10.11.0/24 resta raggiungibile come rete direttamente connessa, quindi la sessione SSH dalla management non cade; per prudenza tengo comunque aperta la console Proxmox come accesso di riserva.

Dopo questa fase la tabella main ha come default solo i gateway DHCP reali (metriche 105/106) e la VM smette di buttare nel vuoto il traffico instradato.

### Fase 4. Policy routing per-WAN verso i tunnel

Installare i nostri tre pezzi (versioni correnti del repo, con il fix di Fase 1):

- `mpquic-policy-routing.sh` in `/usr/local/sbin/`
- `mpquic-routing.service` (oneshot) in `/etc/systemd/system/`, enable
- `mpquic-if-event.sh` in `/usr/local/lib/mpquic/`, che ripara la catena hook già presente sulla VM (dispatcher → `50-mpquic-auto` → if-event → routing)

Effetto a regime: rule 1001-1006 stabili, tabelle wanN con `default dev mpqN` quando il tunnel della WAN è su e `blackhole default` quando è giù (mai deviare su un'altra WAN, principio della checklist), rotte di ritorno /30 verso OpenWRT sul bridge giusto, rotte management e transito. Con la config `/etc/mpquic` assente, lo script salta da solo host route e rule verso il VPS: degrada bene su questa macchina.

Due scelte da confermare in review:

- `MPQUIC_BYPASS_WANS`: il default attuale dello script è `enp7s8` (bypass TS-017 di IBLEA-M, il traffico esce diretto sul fisico). Qui l'obiettivo è il contrario, usare i tunnel: propongo di impostarlo vuoto nella unit. Da confermare.
- `mpquic-if-event.sh` contiene `systemctl restart mpquic@N`: su questa VM le unit `mpquic@` non esistono, quindi quei comandi falliscono in no-op innocui (`|| true`) e nessun servizio ROMARS viene gestito da noi. Se preferisci evitare anche i tentativi a vuoto, la variante è non installare l'if-event e agganciare il refresh del routing a un timer periodico. Io propongo la prima strada, che ripara anche il recovery DHCP sugli eventi di interfaccia.

Vincolo di nomenclatura: rotte e NAT assumono tunnel chiamati `mpq1..mpq6`, come da specifica di interoperabilità consegnata a ROMARS (il masquerade nftables della base image li prevede già). Se il loro software usa nomi diversi, mi fermo e si riapre il discorso.

### Fase 5. Collaudo (richiede i tunnel ROMARS attivi)

Oggi sulla VM non c'è nessun TUN, quindi questa fase resta in attesa finché il software ROMARS non gira. Quando succede, collaudo con il metodo della checklist:

- conteggio delle rule sotto churn: `ip rule | grep -E "^100[1-6]:"` deve restare a 6 dopo 30-60 secondi di eventi;
- per ogni tunnel giù, la tabella wanN deve avere `blackhole default`, mai un default su un fisico;
- trace del traffico reale per ogni WAN: da OpenWRT `mwan3 use <WAN> ping -c 15 9.9.9.9`, sul client `tcpdump -ni any host 9.9.9.9 and icmp`; i pacchetti devono entrare dal /30 giusto e uscire dal tunnel giusto, con le reply che tornano al `172.16.N.2`;
- test scambio cavi per il wan-watchdog: recupero atteso in 60-70 secondi invece delle ore del lease LTE.

### Fase 6. Chiusura

Entry in `docs/TROUBLESHOOTING_HISTORY.md` con le evidenze numeriche del collaudo. Aggiornamento dell'entry di handoff verso ROMARS se serve (il tema lease LTE va riverificato: oggi `enp7s7` non ha più un indirizzo 192.168.1.x). Niente wiki: i pattern in gioco (TS-018, TS-020, TS-014) sono già distillati, salvo sorprese dal collaudo.

## Rollback

Per ogni fase: ripristino del file dal `.bak` e reload del servizio interessato; in ultima istanza rollback dello snapshot Proxmox di Fase 0. Le fasi 2, 3 e 4 sono indipendenti e reversibili singolarmente.

## Punti aperti per la review

1. Dove sta il software tunnel ROMARS? Sulla VM restaurata non ce n'è traccia. Se devono reinstallarlo loro, le fasi 1-4 si possono fare subito e il collaudo aspetta.
2. Conferma `MPQUIC_BYPASS_WANS` vuoto (tutto il traffico LAN nei tunnel).
3. if-event con no-op sulle unit inesistenti, oppure variante a timer.
4. Branch per il commit del fix TS-020.
5. Ruolo reale di `enp7s7` oggi (indirizzo CGNAT 100.64/10: seconda Starlink? LTE dietro CGNAT?). Cambia solo la lettura dei collaudi, non le fasi.
6. Host key del Proxmox 10.10.11.2 cambiata: confermare la reinstallazione prima di fidarsi della chiave nuova.
7. Su OpenWRT manca la default verso 172.16.2.1 (WAN2): decidere se va aggiunta o se WAN2 è fuori scope.

Come concordato: se confermi che nulla di tutto questo tocca codice o script ROMARS (ed è così per ogni file elencato), aspetto il tuo via per applicare le fasi.

## Decisioni dalla review (27 luglio)

1. La VM 300 è un restore del backup fatto prima del collaudo di luglio: il software ROMARS non era ancora installato. Verrà chiesto a ROMARS di installarlo a valle di queste modifiche.
2. Bypass confermato spento: tutto il traffico LAN va nei tunnel.
3. Confermata l'installazione di `mpquic-if-event.sh` (ripara anche il recovery DHCP sugli eventi di interfaccia).
4. Commit dei fix sul branch, ma deploy via scp: sulla VM non deve restare traccia del repository.
5. Configurazione finale dei test: Starlink su `enp7s8`, LTE su `enp7s7`. Per il pre-collaudo girano due Starlink, ma le etichette nei `.network` restano "Starlink" e "LTE" rispettivamente.
6. Host key del Proxmox: chiarita, effetto delle reti di management identiche tra TBOX (accesso alternato a TBOX-MAX e TBOX-EVO).
7. WAN2 fuori scope, nessuna modifica su OpenWRT.

## Esito dell'applicazione (27 luglio, fasi 0-4)

- Fase 0: VMID riconfermato alla sorgente (`qm list`: 300 running, 200 stopped), snapshot `pre-rotte-wd-20260727` creato alle 10:34.
- Fase 1: fix TS-020 committato (`8cfa38d` su `ottavionovella`, solo `scripts/mpquic-policy-routing.sh`).
- Fase 2: `wan-watchdog.sh` installato via scp (md5 `9e318ab5...` identico al repo), unit con override `WAN_INTERFACES=enp7s7 enp7s8`, servizio attivo e stabile (`NRestarts=0`, journal pulito). Trovato e corretto un difetto nella unit: la riga `Environment=` senza virgolette faceva scartare `enp7s8` a systemd ("Invalid environment assignment"), il watchdog partiva su una sola interfaccia. Corretto sulla VM e nel repo (`3dd1543`).
- Fase 3: `Gateway=10.10.11.1` rimosso da `01-mgmt1.network` (backup `.bak-ts018-20260727`); in main restano solo i due default DHCP (metriche 105/106), egress fisico 0% loss, sessione SSH mai caduta.
- Fase 4: installati `mpquic-policy-routing.sh` (versione TS-020), `mpquic-if-event.sh`, `mpquic-routing.service` con drop-in `10-vm300-nobypass.conf`. Nota tecnica: per disattivare il bypass serve `MPQUIC_BYPASS_WANS=none`, non stringa vuota, perché lo script usa `${MPQUIC_BYPASS_WANS:-enp7s8}` e una variabile vuota ricade sul default. Verifica: rule 1001-1006 presenti e stabili (6 su 6 dopo 45 secondi), tutte le tabelle wanN con `blackhole default` (atteso: nessun TUN esiste ancora), rotte di ritorno /30 sui bridge corretti.
- Etichette `.network` aggiornate: WAN5 `enp7s7` (LTE), WAN6 `enp7s8` (Starlink), backup `.bak-20260727`.

Stato atteso fino all'installazione ROMARS: il traffico dei /30 di OpenWRT finisce nel blackhole delle tabelle wanN. È il comportamento voluto (mai bypass sul fisico); OpenWRT uscirà su internet quando i tunnel `mpq1..mpq6` esisteranno. Alla comparsa dei TUN, l'hook networkd-dispatcher e `mpquic-routing.service` mettono `default dev mpqN` nelle tabelle senza altri interventi.

Restano da fare: Fase 5 (collaudo con il metodo trace, dopo l'installazione ROMARS) e Fase 6 (entry in TROUBLESHOOTING_HISTORY con i numeri del collaudo).
