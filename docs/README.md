# MPQUIC — Indice documentazione

Navigazione rapida per tutta la documentazione del progetto MPQUIC IP-over-QUIC.

## Documenti principali

| Documento | Contenuto | Linee |
|-----------|-----------|------:|
| [README.md](../README.md) | Panoramica progetto, motivazioni, scope | 82 |
| [ARCHITETTURA.md](ARCHITETTURA.md) | Architettura 3 livelli, diagrammi, wire protocol, stripe engine | 425 |
| [INSTALLAZIONE_TEST.md](INSTALLAZIONE_TEST.md) | Installazione completa, configurazione YAML (§11), rete, TLS, systemd, monitoring stack | 1968 |
| [TUNNEL_OPERATIONS_DEBUG.md](TUNNEL_OPERATIONS_DEBUG.md) | Runbook operativo: uso quotidiano, debug per sintomo, TLS, watchdog | 380 |
| [DATAPLANE_ORCHESTRATOR.md](DATAPLANE_ORCHESTRATOR.md) | QoS dataplane: classificazione traffico, policy, Control API | 292 |
| [METRICS.md](METRICS.md) | Metriche Prometheus esposte (globali, sessione, path, XOR) | 499 |
| [ROADMAP_IMPLEMENTAZIONE.md](ROADMAP_IMPLEMENTAZIONE.md) | Roadmap completa: tutte le fasi e step implementativi (cronologico) | 1961 |
| [NOTA_TECNICA_MPQUIC.md](NOTA_TECNICA_MPQUIC.md) | Nota tecnica formale: test report, analisi, benchmark, conclusioni | 2215 |
| [NOTA_COMMERCIALE.md](NOTA_COMMERCIALE.md) | Sintesi commerciale: architettura TBOX, prestazioni, sicurezza | 208 |
| [SECURITY.md](SECURITY.md) | Postura di sicurezza: TLS, AES-256-GCM, firewall, limiti | 45 |

**Totale**: ~8.075 righe

## Percorsi di lettura consigliati

### Per un nuovo operatore
1. [README.md](../README.md) — cosa fa il progetto
2. [INSTALLAZIONE_TEST.md](INSTALLAZIONE_TEST.md) §1-7 — installazione
3. [TUNNEL_OPERATIONS_DEBUG.md](TUNNEL_OPERATIONS_DEBUG.md) — uso quotidiano e debug

### Per capire l'architettura
1. [ARCHITETTURA.md](ARCHITETTURA.md) — modello 3 livelli + stripe engine
2. [DATAPLANE_ORCHESTRATOR.md](DATAPLANE_ORCHESTRATOR.md) — QoS e classificazione

### Per configurare
- Parametri YAML: [INSTALLAZIONE_TEST.md](INSTALLAZIONE_TEST.md) §11
- Multipath/stripe: [INSTALLAZIONE_TEST.md](INSTALLAZIONE_TEST.md) §11.8-11.12
- Monitoring: [INSTALLAZIONE_TEST.md](INSTALLAZIONE_TEST.md) §21-22
- VLAN multi-tunnel: [INSTALLAZIONE_TEST.md](INSTALLAZIONE_TEST.md) §23

### Per un decisore / commerciale
1. [NOTA_COMMERCIALE.md](NOTA_COMMERCIALE.md) — sintesi non tecnica
2. [NOTA_TECNICA_MPQUIC.md](NOTA_TECNICA_MPQUIC.md) §19 — vantaggi per il cliente

## Riferimenti canonici (single source of truth)

| Argomento | Documento canonico |
|-----------|-------------------|
| Parametri config YAML | INSTALLAZIONE_TEST §11 |
| Troubleshooting / debug | TUNNEL_OPERATIONS_DEBUG |
| Metriche Prometheus | METRICS.md |
| QoS / dataplane policy | DATAPLANE_ORCHESTRATOR |
| Cronologia implementazione | ROADMAP_IMPLEMENTAZIONE |
| Benchmark / test report | NOTA_TECNICA_MPQUIC §1-17 |
