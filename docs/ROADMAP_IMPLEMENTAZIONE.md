# Roadmap implementazione MPQUIC (allineata a documenti fornitore)

## Stato attuale consolidato

### Server VPS
- `mpquic@1..6` attivi
- interfacce `mpq1..mpq6` create
- listener UDP su `45001..45006` attivi

### Client MPQUIC
- `mpquic@4`, `mpquic@5` e `mpquic@6` attivi e testati end-to-end
- ping tunnel riusciti:
  - `mpq4 (10.200.4.1 -> 10.200.4.2)`
  - `mpq5 (10.200.5.1 -> 10.200.5.2)`
  - `mpq6 (10.200.6.1 -> 10.200.6.2)`
- bind sorgente QUIC verificato:
  - WAN4 `enp7s6` (`10.150.19.99`)
  - WAN5 `enp7s7` (`192.168.1.102`)
  - WAN6 `enp7s8` (`100.110.241.142`)

### Gap bloccanti attuali
- `enp7s3..enp7s5` senza IPv4 DHCP (solo link-local)
- di conseguenza `mpquic@1..@3` non possono bindare su WAN1..WAN3
- stato fase baseline: `3/6` tunnel realmente operativi

### Nota operativa (25/02)
- WAN1 e WAN2 sono temporaneamente senza modem collegato: il test DHCP/bring-up è pianificato a venerdì mattina.
- Questo è considerato scenario reale di esercizio (modem unplug/offline): il sistema deve degradare in modo controllato (istanza WAN assente stop, istanze sane up).

## Roadmap aggiornata

## Fase 1 — Baseline 6 sessioni QUIC 1:1 (NO multipath) [in corso]
Obiettivo: 6 sessioni indipendenti, una per WAN, senza cambiare la logica L3 esistente.

Passi:
1. Tenere VPS con `mpquic@1..6` attivi (completato)
2. Ripristinare IPv4 su WAN1..WAN4 (`enp7s3..enp7s6`) lato client (bloccante)
3. Attivare `mpquic@1..@4` e verificare E2E su tutte le coppie `/30`
4. Verificare bind sorgente su interfacce corrette (`ss -unap | grep mpquic`)

Done criteria:
- `6/6` sessioni QUIC attive e stabili
- `6/6` ping tunnel ok

## Fase 2 — Traffico LAN instradato nel tunnel corretto (priorità immediata)
Obiettivo: dimostrare che il traffico reale LAN entra nel tunnel dedicato e viaggia come QUIC su WAN.

Primo use-case obbligatorio:
1. traffico da `LAN1` (`enp6s20`, subnet `172.16.1.0/30`) instradato su `mpq1`
2. conferma che il traffico sul tratto WAN è UDP QUIC (porta 45001), non TCP raw

Passi operativi:
1. attivare pienamente `mpquic@1` (richiede IPv4 su `enp7s3`)
2. aggiungere routing/forwarding persistente `LAN1 -> mpq1` (senza alterare policy WAN esistente)
3. sul client fare capture su WAN1 (`tcpdump -ni enp7s3 udp port 45001`)
4. generare traffico test da LAN1 (ICMP/TCP/UDP)
5. verificare contemporaneamente:
   - pacchetti nel tunnel `mpq1`
   - pacchetti QUIC UDP su `enp7s3:45001`

Done criteria:
- traffico LAN1 passa nel tunnel `mpq1`
- evidenza packet-level dell'incapsulamento QUIC

## Fase 3 — Generalizzazione LAN2..LAN6 -> mpq2..mpq6
Obiettivo: estendere la logica validata su LAN1 a tutte le 6 LAN.

Passi:
1. replicare policy persistenti per ogni coppia `LANx -> mpqx`
2. validare per ogni WAN con test e capture
3. baseline prestazionale per canale (throughput, RTT, loss, jitter)

## Fase 4 — Multipath QUIC (single logical connection)
Obiettivo: superare limite single-flow e implementare aggregazione/strategie dinamiche.

Stato avanzamento (26/02):
- Step 1 completato in modalità sperimentale nel runtime (`multipath_enabled` + `multipath_paths` con scheduler path-aware, fail-cooldown e recovery path-level).
- Backward compatibility mantenuta: configurazioni single-path esistenti invariate.
- Aggiunta degradazione controllata: il multipath parte con subset path attivi se almeno un path è disponibile.
- Aggiunta telemetria path-level base su log runtime (`path telemetry ...`).
- Avviata prima versione policy engine statico (`multipath_policy`: `priority|failover|balanced`).
- TLS allineato a Go moderno: certificato server con SAN e trust CA esplicito lato client.

Capacità target (da documenti fornitore):
- bonding (aggregazione)
- backup/failover
- duplication per traffico mission critical
- policy/QoS applicative
- monitoraggio link in tempo reale

Passi:
1. introdurre sessione logica multipath con scheduler path-aware (completato, sperimentale)
2. aggiungere orchestrazione cross-sessione (policy engine) (in corso: policy statiche path-level disponibili)
3. implementare telemetria path-level (RTT/loss/capacità) (in corso: baseline log counters disponibile)
4. validare su scenari LEO variabili (handover/jitter)

Gap tecnici residui Fase 4:
- QoS applicativa per classi traffico non ancora implementata (oggi tuning via `priority/weight` e `multipath_policy`).
- Duplication mission-critical non ancora implementata.
- Metriche RTT/loss/capacità non ancora persistite/esposte via endpoint strutturato.

Track diagnostica stabilità (in parallelo):
- raccolta long-running (`mpquic-long-diagnostics.sh`) su client/server per correlare eventi crash/flap con stato path, routing e journal.

## Fase 5 — Sicurezza TLS hardening
Obiettivo: canale cifrato con gestione certificati persistente.

Stato:
- oggi TLS è già presente (self-signed runtime)

Evoluzione richiesta:
1. certificato server persistente su file (`cert.pem`/`key.pem`)
2. trust esplicito lato client (no `InsecureSkipVerify`)
3. chiave minima >= 1024 bit (raccomandato 2048)
4. rotazione certificati e procedure operative documentate

## Prossimo step operativo (immediato)

1. Stabilizzare test multipath senza contesa con `mpquic@4/@5/@6` (sessione dedicata di test o finestre controllate)
2. Implementare prima versione policy engine cross-sessione (regole statiche per classi traffico)
3. Aggiungere metriche RTT/loss per path e reporting strutturato
4. Validare scenario modem unplug su path multipath:
  - down di una WAN attiva
  - continuità traffico sui path residui
  - rientro automatico del path ripristinato
5. Parallelamente continuare Fase 1/2 su WAN1/WAN2 per completare baseline 6/6

## Comandi base di verifica

Server:
```bash
for i in 1 2 3 4 5 6; do systemctl is-active mpquic@$i.service; done
ip -br a | egrep '^mpq[1-6]'
ss -lunp | egrep '4500[1-6]'
```

Client:
```bash
for i in 1 2 3 4 5 6; do sudo systemctl is-active mpquic@$i.service; done
ip -br a show dev enp7s3
ip -br a show dev enp7s4
ip -br a show dev enp7s5
ip -br a show dev enp7s6
ip -br a show dev enp7s7
ip -br a show dev enp7s8
sudo ss -unap | grep mpquic
```
