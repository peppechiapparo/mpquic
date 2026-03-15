# MPQUIC — Metriche e Osservabilità

> Versione: 4.2 — Fase 5 Layer 1 + Layer 2  
> Data: 2025-07-15

---

## Indice

1. [Panoramica architettura](#panoramica-architettura)
2. [Configurazione](#configurazione)
3. [Endpoint HTTP](#endpoint-http)
   - [JSON API (`/api/v1/stats`)](#json-api-apiv1stats)
   - [Prometheus (`/metrics`)](#prometheus-metrics)
4. [Struttura JSON — Server](#struttura-json--server)
5. [Struttura JSON — Client](#struttura-json--client)
6. [Catalogo metriche Prometheus](#catalogo-metriche-prometheus)
   - [Metriche globali](#metriche-globali)
   - [Metriche per-session (server)](#metriche-per-session-server)
   - [Metriche per-path (client)](#metriche-per-path-client)
7. [Esempi di scraping Prometheus](#esempi-di-scraping-prometheus)
8. [Query PromQL utili](#query-promql-utili)
9. [Dashboard Grafana — Pannelli suggeriti](#dashboard-grafana--pannelli-suggeriti)

---

## Panoramica architettura

```
┌─────────────────────────────────────────────────────────────┐
│  Layer 3: Consumer (Fase 5.2)                               │
│  ┌───────────┐  ┌────────────┐  ┌─────────────────────────┐ │
│  │  Grafana  │  │ Alerting   │  │  AI/ML Engine (Fase 6)  │ │
│  │ Dashboard │  │ (rules)    │  │  Quality on Demand      │ │
│  └─────┬─────┘  └─────┬──────┘  └──────────┬──────────────┘ │
│        │              │                    │                │
│        └──────────────┼────────────────────┘                │
│                       │                                     │
│              ┌────────▼────────┐                            │
│              │   Prometheus    │                            │
│              │  (scrape ogni   │                            │
│              │   15s–30s)      │                            │
│              └────────┬────────┘                            │
├───────────────────────┼─────────────────────────────────────┤
│  Layer 2: Export      │                                     │
│  ┌────────────────────▼─────────────────────────────────┐   │
│  │  HTTP Server (tunnel_ip:9090)                        │   │
│  │                                                      │   │
│  │  GET /metrics       → Prometheus text exposition     │   │
│  │  GET /api/v1/stats  → JSON strutturato               │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│  Layer 1: Collection (nel dataplane hot path)               │
│  ┌──────────────────────────────────────────────────────┐   │
│  │  sync/atomic counters — zero-alloc, zero-lock        │   │
│  │                                                      │   │
│  │  Server (per-session):                               │   │
│  │    tx/rx bytes, tx/rx pkts, FEC encode/recover,      │   │
│  │    ARQ nack/retx/dup, loss rate, decrypt failures    │   │
│  │                                                      │   │
│  │  Client (per-path):                                  │   │
│  │    tx/rx pkts, stripe tx/rx bytes/pkts,              │   │
│  │    stripe FEC recovered, path alive status           │   │
│  └──────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

**Principi di design:**
- **Zero-alloc nel hot path**: tutti i contatori usano `sync/atomic` — nessuna allocazione heap durante TX/RX
- **Zero-lock in lettura**: gli snapshot iterano le sessioni sotto `RLock` solo al momento della richiesta HTTP
- **Isolamento di rete**: l'HTTP server è bound all'IP del tunnel (es. `10.200.17.254:9090`), non esposto su interfacce pubbliche
- **Nessun impatto sul throughput**: operazioni atomiche `Add` costano ~1 ns ciascuna

---

## Configurazione

Nel file YAML dell'istanza (`/etc/mpquic/<instance>.yaml`):

```yaml
# Abilita il server metriche sull'IP del tunnel, porta 9090
metrics_listen: auto
```

| Valore | Comportamento |
|--------|---------------|
| `auto` | Bind su `<tunnel_ip>:9090` automaticamente (raccomandato) |
| `10.200.17.254:9091` | Bind su indirizzo:porta specifici |
| *(omesso)* | Metriche disabilitate |

L'opzione `auto` usa l'IP del tunnel configurato come `tun_local` (client) o il primo IP della subnet `tun_cidr` lato server per calcolare l'indirizzo `.254:9090`.

---

## Endpoint HTTP

### JSON API (`/api/v1/stats`)

| Proprietà | Valore |
|-----------|--------|
| **URL** | `http://<tunnel_ip>:9090/api/v1/stats` |
| **Method** | `GET` |
| **Content-Type** | `application/json` |
| **Uso** | Portali cliente, script di monitoraggio, debug manuale |

```bash
# Esempio
curl -s http://10.200.17.254:9090/api/v1/stats | jq .
```

### Prometheus (`/metrics`)

| Proprietà | Valore |
|-----------|--------|
| **URL** | `http://<tunnel_ip>:9090/metrics` |
| **Method** | `GET` |
| **Content-Type** | `text/plain; version=0.0.4; charset=utf-8` |
| **Uso** | Scraping Prometheus, integrazione Grafana |

```bash
# Esempio
curl -s http://10.200.17.254:9090/metrics
```

---

## Struttura JSON — Server

Il server espone l'array `sessions[]`, uno per ciascun peer (client) connesso.

```json
{
  "role": "server",
  "version": "4.2",
  "uptime_sec": 14523.45,
  "sessions": [
    {
      "session_id": "a1b2c3d4",
      "peer_ip": "10.150.19.95",
      "pipes": 100,
      "tx_bytes": 892345678,
      "tx_pkts": 612345,
      "rx_bytes": 1234567890,
      "rx_pkts": 845678,
      "fec_mode": "adaptive",
      "adaptive_m": 0,
      "fec_encoded": 12345,
      "fec_recovered": 234,
      "arq_nack_sent": 567,
      "arq_retx_recv": 523,
      "arq_dup_filtered": 89,
      "loss_rate_pct": 0,
      "uptime_sec": 14500.12,
      "decrypt_fail": 0
    },
    {
      "session_id": "e5f6a7b8",
      "peer_ip": "100.64.86.226",
      "pipes": 100,
      "tx_bytes": 456789012,
      "tx_pkts": 312456,
      "rx_bytes": 678901234,
      "rx_pkts": 467890,
      "fec_mode": "adaptive",
      "adaptive_m": 0,
      "fec_encoded": 6789,
      "fec_recovered": 123,
      "arq_nack_sent": 234,
      "arq_retx_recv": 210,
      "arq_dup_filtered": 45,
      "loss_rate_pct": 0,
      "uptime_sec": 14500.12,
      "decrypt_fail": 0
    }
  ],
  "total_tx_bytes": 1349134690,
  "total_rx_bytes": 1913469124,
  "total_tx_pkts": 924801,
  "total_rx_pkts": 1313568
}
```

### Campi per-session (server)

| Campo | Tipo | Descrizione |
|-------|------|-------------|
| `session_id` | string | ID sessione hex (8 char), identifica il peer client |
| `peer_ip` | string | IP sorgente del peer (WAN IP del client) |
| `pipes` | int | Numero di pipe UDP attive nella sessione |
| `tx_bytes` | uint64 | Byte trasmessi verso il client (counter) |
| `tx_pkts` | uint64 | Pacchetti trasmessi verso il client (counter) |
| `rx_bytes` | uint64 | Byte ricevuti dal client (counter) |
| `rx_pkts` | uint64 | Pacchetti ricevuti dal client (counter) |
| `fec_mode` | string | Modalità FEC: `"none"`, `"static"`, `"adaptive"` |
| `adaptive_m` | int | Parità FEC corrente (M). `0` = FEC inattivo |
| `fec_encoded` | uint64 | Gruppi FEC codificati in TX (counter) |
| `fec_recovered` | uint64 | Gruppi FEC recuperati in RX (counter) |
| `arq_nack_sent` | uint64 | NACK ARQ inviati (counter) — pacchetti mancanti segnalati |
| `arq_retx_recv` | uint64 | Ritrasmissioni ARQ ricevute (counter) |
| `arq_dup_filtered` | uint64 | Pacchetti duplicati filtrati (counter) |
| `loss_rate_pct` | uint32 | Tasso di perdita riportato dal peer (0–100%) |
| `uptime_sec` | float64 | Durata della sessione in secondi |
| `decrypt_fail` | uint64 | Fallimenti di decifratura (counter) — potenziale security issue |

---

## Struttura JSON — Client

Il client espone l'array `paths[]`, uno per ciascun percorso WAN configurato.

```json
{
  "role": "client",
  "version": "4.2",
  "uptime_sec": 14523.45,
  "paths": [
    {
      "name": "wan5",
      "bind_ip": "10.150.19.95",
      "alive": true,
      "tx_bytes": 0,
      "tx_pkts": 612345,
      "rx_bytes": 0,
      "rx_pkts": 845678,
      "stripe_tx_bytes": 892345678,
      "stripe_tx_pkts": 612345,
      "stripe_rx_bytes": 1234567890,
      "stripe_rx_pkts": 845678,
      "stripe_fec_recovered": 234
    },
    {
      "name": "wan6",
      "bind_ip": "100.64.86.226",
      "alive": true,
      "tx_bytes": 0,
      "tx_pkts": 312456,
      "rx_bytes": 0,
      "rx_pkts": 467890,
      "stripe_tx_bytes": 456789012,
      "stripe_tx_pkts": 312456,
      "stripe_rx_bytes": 678901234,
      "stripe_rx_pkts": 467890,
      "stripe_fec_recovered": 123
    }
  ],
  "total_tx_bytes": 1349134690,
  "total_rx_bytes": 1913469124,
  "total_tx_pkts": 924801,
  "total_rx_pkts": 1313568
}
```

### Campi per-path (client)

| Campo | Tipo | Descrizione |
|-------|------|-------------|
| `name` | string | Nome del path (da YAML, es. `"wan5"`, `"wan6"`) |
| `bind_ip` | string | IP sorgente di bind per questo path |
| `alive` | bool | `true` se il path è attivo e raggiungibile |
| `tx_bytes` | uint64 | Byte trasmessi (livello QUIC tunnel) |
| `tx_pkts` | uint64 | Pacchetti trasmessi (livello QUIC tunnel) |
| `rx_bytes` | uint64 | Byte ricevuti (livello QUIC tunnel) |
| `rx_pkts` | uint64 | Pacchetti ricevuti (livello QUIC tunnel) |
| `stripe_tx_bytes` | uint64 | Byte trasmessi dal motore stripe (omesso se 0) |
| `stripe_tx_pkts` | uint64 | Pacchetti trasmessi dal motore stripe (omesso se 0) |
| `stripe_rx_bytes` | uint64 | Byte ricevuti dal motore stripe (omesso se 0) |
| `stripe_rx_pkts` | uint64 | Pacchetti ricevuti dal motore stripe (omesso se 0) |
| `stripe_fec_recovered` | uint64 | Gruppi FEC recuperati sullo stripe (omesso se 0) |

### Campi globali (comuni client e server)

| Campo | Tipo | Descrizione |
|-------|------|-------------|
| `role` | string | `"server"` o `"client"` |
| `version` | string | Versione del software (es. `"4.2"`) |
| `uptime_sec` | float64 | Uptime del processo in secondi |
| `total_tx_bytes` | uint64 | Somma byte TX di tutte le session/path |
| `total_rx_bytes` | uint64 | Somma byte RX di tutte le session/path |
| `total_tx_pkts` | uint64 | Somma pacchetti TX di tutte le session/path |
| `total_rx_pkts` | uint64 | Somma pacchetti RX di tutte le session/path |

---

## Catalogo metriche Prometheus

Tutte le metriche hanno il prefisso `mpquic_`.

### Metriche globali

| Metrica | Tipo | Descrizione |
|---------|------|-------------|
| `mpquic_uptime_seconds` | gauge | Uptime del processo in secondi |
| `mpquic_tx_bytes_total` | counter | Byte totali trasmessi (tutte le sessioni/path) |
| `mpquic_rx_bytes_total` | counter | Byte totali ricevuti (tutte le sessioni/path) |
| `mpquic_tx_packets_total` | counter | Pacchetti totali trasmessi |
| `mpquic_rx_packets_total` | counter | Pacchetti totali ricevuti |

### Metriche per-session (server)

Labels: `session` (hex ID), `peer` (IP sorgente)

| Metrica | Tipo | Descrizione |
|---------|------|-------------|
| `mpquic_session_tx_bytes` | counter | Byte trasmessi verso il peer |
| `mpquic_session_rx_bytes` | counter | Byte ricevuti dal peer |
| `mpquic_session_tx_packets` | counter | Pacchetti trasmessi verso il peer |
| `mpquic_session_rx_packets` | counter | Pacchetti ricevuti dal peer |
| `mpquic_session_pipes` | gauge | Pipe UDP attive nella sessione |
| `mpquic_session_adaptive_m` | gauge | Parità FEC corrente (M). 0 = FEC disattivato |
| `mpquic_session_fec_encoded` | counter | Gruppi FEC codificati (TX) |
| `mpquic_session_fec_recovered` | counter | Gruppi FEC recuperati (RX) |
| `mpquic_session_arq_nack_sent` | counter | NACK ARQ inviati (pacchetti mancanti) |
| `mpquic_session_arq_retx_recv` | counter | Ritrasmissioni ARQ ricevute |
| `mpquic_session_arq_dup_filtered` | counter | Pacchetti duplicati scartati |
| `mpquic_session_loss_rate_pct` | gauge | Tasso di perdita riportato dal peer (0–100) |
| `mpquic_session_uptime_seconds` | gauge | Durata della sessione in secondi |
| `mpquic_session_decrypt_fail` | counter | Fallimenti di decifratura |

### Metriche per-path (client)

Labels: `path` (nome WAN), `bind` (IP sorgente)

| Metrica | Tipo | Descrizione |
|---------|------|-------------|
| `mpquic_path_alive` | gauge | Path attivo (1) o inattivo (0) |
| `mpquic_path_tx_packets` | counter | Pacchetti trasmessi su questo path |
| `mpquic_path_rx_packets` | counter | Pacchetti ricevuti su questo path |
| `mpquic_path_stripe_tx_bytes` | counter | Byte stripe trasmessi su questo path |
| `mpquic_path_stripe_rx_bytes` | counter | Byte stripe ricevuti su questo path |
| `mpquic_path_stripe_fec_recovered` | counter | Gruppi FEC stripe recuperati |

---

## Esempi di scraping Prometheus

### Target statici nel `prometheus.yml`

In un deploy dove Prometheus ha visibilità sulle reti tunnel (10.200.x.x), i target vengono configurati staticamente:

```yaml
# /etc/prometheus/prometheus.yml

global:
  scrape_interval: 15s
  evaluation_interval: 15s

scrape_configs:
  # ── Server VPS (172.238.232.223) ───────────────────────
  - job_name: "mpquic-server"
    static_configs:
      - targets:
          - "10.200.17.254:9090"   # mp1 server
        labels:
          instance_name: "mp1"
          site: "vps"

  # ── Client (Proxmox VM 200) ────────────────────────────
  - job_name: "mpquic-client"
    static_configs:
      - targets:
          - "10.200.17.1:9090"     # mp1 client
          - "10.200.14.1:9090"     # cr4 client (WAN4)
          - "10.200.15.1:9090"     # cr5 client (WAN5)
          - "10.200.16.1:9090"     # cr6 client (WAN6)
        labels:
          site: "client"
```

### Verifica manuale dal container Prometheus

```bash
# Testa la raggiungibilità dei target
curl -s http://10.200.17.254:9090/metrics | head -5
curl -s http://10.200.17.1:9090/metrics | head -5
```

---

## Query PromQL utili

### Throughput istantaneo (byte/s)

```promql
# TX rate per session (server)
rate(mpquic_session_tx_bytes[1m])

# RX rate totale (client)
rate(mpquic_rx_bytes_total[1m])
```

### Tasso di perdita per sessione

```promql
# Loss rate corrente
mpquic_session_loss_rate_pct

# Sessioni con loss > 5%
mpquic_session_loss_rate_pct > 5
```

### Efficacia FEC

```promql
# Ratio recovered/encoded (FEC efficiency)
rate(mpquic_session_fec_recovered[5m])
  / rate(mpquic_session_fec_encoded[5m])

# FEC recovery rate per session
rate(mpquic_session_fec_recovered[5m])
```

### ARQ health

```promql
# NACK rate → indica packet loss prima del recovery
rate(mpquic_session_arq_nack_sent[5m])

# Retransmission success ratio
rate(mpquic_session_arq_retx_recv[5m])
  / rate(mpquic_session_arq_nack_sent[5m])
```

### Path health (client)

```promql
# Path attivi per istanza
sum by (instance) (mpquic_path_alive)

# Throughput per path
rate(mpquic_path_stripe_tx_bytes[1m])
```

### Anomalie e security

```promql
# Decrypt failures (security alarm)
increase(mpquic_session_decrypt_fail[5m]) > 0

# Duplicati anomali (possibile replay attack)
rate(mpquic_session_arq_dup_filtered[5m]) > 100
```

---

## Dashboard Grafana — Pannelli suggeriti

### Row 1: Overview

| Pannello | Tipo | Query |
|----------|------|-------|
| Uptime | Stat | `mpquic_uptime_seconds` |
| Sessioni attive | Stat | `count(mpquic_session_pipes)` |
| Path attivi | Stat | `sum(mpquic_path_alive)` |
| TX totale | Stat (bytes) | `mpquic_tx_bytes_total` |
| RX totale | Stat (bytes) | `mpquic_rx_bytes_total` |

### Row 2: Throughput

| Pannello | Tipo | Query |
|----------|------|-------|
| TX rate per session | Time series | `rate(mpquic_session_tx_bytes[1m])` |
| RX rate per session | Time series | `rate(mpquic_session_rx_bytes[1m])` |
| TX rate per path | Time series | `rate(mpquic_path_stripe_tx_bytes[1m])` |
| RX rate per path | Time series | `rate(mpquic_path_stripe_rx_bytes[1m])` |

### Row 3: Quality (FEC + ARQ + Loss)

| Pannello | Tipo | Query |
|----------|------|-------|
| Loss rate | Time series | `mpquic_session_loss_rate_pct` |
| FEC recovery rate | Time series | `rate(mpquic_session_fec_recovered[5m])` |
| ARQ NACK rate | Time series | `rate(mpquic_session_arq_nack_sent[5m])` |
| Adaptive M | Time series | `mpquic_session_adaptive_m` |

### Row 4: Infrastructure

| Pannello | Tipo | Query |
|----------|------|-------|
| Pipe per session | Gauge | `mpquic_session_pipes` |
| Path alive map | Table | `mpquic_path_alive` |
| Session uptime | Table | `mpquic_session_uptime_seconds` |
| Decrypt failures | Alert table | `increase(mpquic_session_decrypt_fail[5m])` |

---

## Mappa target attivi

| Istanza | Ruolo | Tunnel IP | Endpoint metriche |
|---------|-------|-----------|-------------------|
| mp1 | server | 10.200.17.254 | `http://10.200.17.254:9090` |
| mp1 | client | 10.200.17.1 | `http://10.200.17.1:9090` |
| cr4 | client | 10.200.14.1 | `http://10.200.14.1:9090` |
| cr5 | client | 10.200.15.1 | `http://10.200.15.1:9090` |
| cr6 | client | 10.200.16.1 | `http://10.200.16.1:9090` |

> **Nota**: I target server per le istanze multi-conn (mt4/mt5/mt6, mpq4-6)
> espongono metriche sugli IP `.254` del rispettivo tunnel ma con `role=server`
> e `sessions[]` invece di `paths[]`.
