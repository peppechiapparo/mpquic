# Integrazione Zabbix — Metriche TBOX MPQUIC (VSAT / Starlink)

**Data**: 13 aprile 2026  
**Versione**: 1.0  
**Autori**: Team Engineering SATCOMVAS  
**Classificazione**: Interna / Cliente

---

## Indice

1. [Scopo del Documento](#1-scopo-del-documento)
2. [Architettura di Integrazione](#2-architettura-di-integrazione)
3. [Endpoint Prometheus sulla TBOX](#3-endpoint-prometheus-sulla-tbox)
4. [Catalogo Metriche per Zabbix](#4-catalogo-metriche-per-zabbix)
   - [Metriche Globali (Aggregate)](#41-metriche-globali-aggregate)
   - [Metriche per-Path (per link WAN)](#42-metriche-per-path-per-link-wan)
   - [Metriche per-Session (lato server)](#43-metriche-per-session-lato-server)
   - [Metriche Derivate (calcolate da Zabbix)](#44-metriche-derivate-calcolate-da-zabbix)
5. [Mapping Zabbix — Modalità di Acquisizione](#5-mapping-zabbix--modalità-di-acquisizione)
6. [Trigger e Alert](#6-trigger-e-alert)
7. [Template Zabbix](#7-template-zabbix)
8. [Pacchetto Minimo vs Esteso](#8-pacchetto-minimo-vs-esteso)
9. [Best Practice](#9-best-practice)

---

## 1. Scopo del Documento

Questo documento descrive le metriche di qualità e throughput esposte da ciascuna TBOX tramite endpoint Prometheus, e definisce la strategia di integrazione con un sistema di monitoraggio Zabbix centralizzato.

L'obiettivo è consentire al NOC/SOC di monitorare in tempo reale:

- **Throughput** (bitrate TX/RX) per ciascun link WAN (VSAT, Starlink)
- **Qualità** (loss, FEC recovery, ARQ retransmission)
- **Disponibilità** (stato link attivo/inattivo, failover)
- **Sicurezza** (decrypt failures, duplicati anomali)
- **SLA** (conformità basata su soglie configurabili)

---

## 2. Architettura di Integrazione

```
┌──────────────────────────────────────────────────────────────────┐
│  TBOX (Nave / Sito remoto)                                       │
│                                                                  │
│  ┌─────────────────────────────────────────────┐                 │
│  │  mpquic client (dataplane)                  │                 │
│  │                                             │                 │
│  │  wan5 (VSAT)      wan6 (Starlink)           │                 │
│  │    ▲                  ▲                     │                 │
│  │    │  UDP stripe      │  UDP stripe         │                 │
│  │    └──────┬───────────┘                     │                 │
│  │           │                                 │                 │
│  │  ┌────────▼────────────┐                    │                 │
│  │  │  Metrics HTTP       │                    │                 │
│  │  │  <tun_ip>:9090      │                    │                 │
│  │  │                     │                    │                 │
│  │  │  GET /metrics  ←────┼── Prometheus text  │                 │
│  │  │  GET /api/v1/stats ─┼── JSON             │                 │
│  │  └─────────────────────┘                    │                 │
│  └─────────────────────────────────────────────┘                 │
└──────────────────────┬───────────────────────────────────────────┘
                       │  Tunnel IP / ZeroTier / VPN
                       │
┌──────────────────────▼───────────────────────────────────────────┐
│  NOC / Data Center                                               │
│                                                                  │
│  ┌──────────────┐     ┌──────────────┐     ┌──────────────────┐ │
│  │   Zabbix     │────▶│  HTTP Agent  │────▶│ /metrics         │ │
│  │   Server     │     │  (scrape)    │     │ endpoint TBOX    │ │
│  │              │     └──────────────┘     └──────────────────┘ │
│  │              │                                               │
│  │  ┌────────┐  │     ┌──────────────┐                          │
│  │  │ Items  │◀─┼─────│ Preprocessing│                          │
│  │  │Triggers│  │     │ Prometheus   │                          │
│  │  │ Graphs │  │     │ pattern      │                          │
│  │  └────────┘  │     └──────────────┘                          │
│  └──────────────┘                                               │
└─────────────────────────────────────────────────────────────────┘
```

### Flusso dati

1. **TBOX** espone metriche Prometheus su `<tunnel_ip>:9090/metrics` (bind su interfaccia tunnel, non pubblico)
2. **Zabbix HTTP Agent** esegue scrape periodico dell'endpoint (intervallo consigliato: 30s)
3. Un **item master** acquisisce il payload grezzo in formato Prometheus text exposition
4. **Dependent items** con preprocessing Prometheus pattern estraggono le singole metriche
5. **Trigger** valutano soglie e generano alert

---

## 3. Endpoint Prometheus sulla TBOX

| Proprietà | Valore |
|-----------|--------|
| **URL** | `http://<tunnel_ip>:9090/metrics` |
| **Metodo** | `GET` |
| **Content-Type** | `text/plain; version=0.0.4; charset=utf-8` |
| **Autenticazione** | Nessuna (protetto da isolamento rete tunnel) |
| **Prefisso metriche** | `mpquic_` |
| **Frequenza scrape consigliata** | 30s |

**Esempio di output:**

```
# HELP mpquic_uptime_seconds Process uptime in seconds
# TYPE mpquic_uptime_seconds gauge
mpquic_uptime_seconds 14523.45
# HELP mpquic_tx_bytes_total Total bytes transmitted
# TYPE mpquic_tx_bytes_total counter
mpquic_tx_bytes_total 1349134690
# HELP mpquic_path_alive Path is alive (1) or down (0)
# TYPE mpquic_path_alive gauge
mpquic_path_alive{path="wan5",bind="10.150.19.95"} 1
mpquic_path_alive{path="wan6",bind="100.64.86.226"} 1
# HELP mpquic_path_stripe_tx_bytes Stripe bytes transmitted per path
# TYPE mpquic_path_stripe_tx_bytes counter
mpquic_path_stripe_tx_bytes{path="wan5",bind="10.150.19.95"} 892345678
mpquic_path_stripe_tx_bytes{path="wan6",bind="100.64.86.226"} 456789012
```

---

## 4. Catalogo Metriche per Zabbix

### 4.1 Metriche Globali (Aggregate)

Queste metriche rappresentano il servizio TBOX nel suo complesso, indipendentemente dal link in uso.

| Metrica Prometheus | Tipo | Unità | Zabbix Item Key | Descrizione |
|---|---|---|---|---|
| `mpquic_uptime_seconds` | gauge | secondi | `mpquic.uptime` | Uptime processo mpquic |
| `mpquic_tx_bytes_total` | counter | byte | `mpquic.tx.bytes.total` | Byte totali trasmessi (tutte path/sessioni) |
| `mpquic_rx_bytes_total` | counter | byte | `mpquic.rx.bytes.total` | Byte totali ricevuti |
| `mpquic_tx_packets_total` | counter | pacchetti | `mpquic.tx.pkts.total` | Pacchetti totali trasmessi |
| `mpquic_rx_packets_total` | counter | pacchetti | `mpquic.rx.pkts.total` | Pacchetti totali ricevuti |

### 4.2 Metriche per-Path (per link WAN)

Queste metriche sono disponibili **lato client** con label `path` che identifica il link WAN.

Nella configurazione TBOX standard:
- `path="wan5"` → **VSAT** (o LTE/5G nel progetto TRINA)
- `path="wan6"` → **Starlink**

| Metrica Prometheus | Tipo | Unità | Zabbix Item Key (wan5/VSAT) | Zabbix Item Key (wan6/Starlink) | Descrizione |
|---|---|---|---|---|---|
| `mpquic_path_alive` | gauge | 0/1 | `mpquic.path.wan5.alive` | `mpquic.path.wan6.alive` | Link attivo/inattivo |
| `mpquic_path_tx_packets` | counter | pkt | `mpquic.path.wan5.tx.pkts` | `mpquic.path.wan6.tx.pkts` | Pacchetti TX per path |
| `mpquic_path_rx_packets` | counter | pkt | `mpquic.path.wan5.rx.pkts` | `mpquic.path.wan6.rx.pkts` | Pacchetti RX per path |
| `mpquic_path_stripe_tx_bytes` | counter | byte | `mpquic.path.wan5.stripe.tx.bytes` | `mpquic.path.wan6.stripe.tx.bytes` | Byte stripe TX per path |
| `mpquic_path_stripe_rx_bytes` | counter | byte | `mpquic.path.wan5.stripe.rx.bytes` | `mpquic.path.wan6.stripe.rx.bytes` | Byte stripe RX per path |
| `mpquic_path_stripe_tx_packets` | counter | pkt | `mpquic.path.wan5.stripe.tx.pkts` | `mpquic.path.wan6.stripe.tx.pkts` | Pacchetti stripe TX per path |
| `mpquic_path_stripe_rx_packets` | counter | pkt | `mpquic.path.wan5.stripe.rx.pkts` | `mpquic.path.wan6.stripe.rx.pkts` | Pacchetti stripe RX per path |
| `mpquic_path_stripe_fec_recovered` | counter | gruppi | `mpquic.path.wan5.fec.recovered` | `mpquic.path.wan6.fec.recovered` | Gruppi FEC recuperati per path |

### 4.3 Metriche per-Session (lato server)

Queste metriche sono disponibili **lato server** con label `session` e `peer`.

| Metrica Prometheus | Tipo | Unità | Zabbix Item Key | Descrizione |
|---|---|---|---|---|
| `mpquic_session_tx_bytes` | counter | byte | `mpquic.session.tx.bytes[{#SESSION}]` | Byte TX verso il peer |
| `mpquic_session_rx_bytes` | counter | byte | `mpquic.session.rx.bytes[{#SESSION}]` | Byte RX dal peer |
| `mpquic_session_tx_packets` | counter | pkt | `mpquic.session.tx.pkts[{#SESSION}]` | Pacchetti TX |
| `mpquic_session_rx_packets` | counter | pkt | `mpquic.session.rx.pkts[{#SESSION}]` | Pacchetti RX |
| `mpquic_session_pipes` | gauge | count | `mpquic.session.pipes[{#SESSION}]` | Pipe UDP attive |
| `mpquic_session_fec_encoded` | counter | gruppi | `mpquic.session.fec.encoded[{#SESSION}]` | Gruppi FEC codificati |
| `mpquic_session_fec_recovered` | counter | gruppi | `mpquic.session.fec.recovered[{#SESSION}]` | Gruppi FEC recuperati |
| `mpquic_session_arq_nack_sent` | counter | NACK | `mpquic.session.arq.nack[{#SESSION}]` | NACK ARQ inviati |
| `mpquic_session_arq_retx_recv` | counter | pkt | `mpquic.session.arq.retx[{#SESSION}]` | Ritrasmissioni ARQ ricevute |
| `mpquic_session_arq_dup_filtered` | counter | pkt | `mpquic.session.arq.dup[{#SESSION}]` | Duplicati filtrati |
| `mpquic_session_loss_rate_pct` | gauge | % | `mpquic.session.loss.pct[{#SESSION}]` | Tasso di perdita (0–100) |
| `mpquic_session_uptime_seconds` | gauge | sec | `mpquic.session.uptime[{#SESSION}]` | Durata sessione |
| `mpquic_session_decrypt_fail` | counter | errori | `mpquic.session.decrypt.fail[{#SESSION}]` | Fallimenti decifratura |
| `mpquic_session_adaptive_m` | gauge | M | `mpquic.session.fec.adaptive_m[{#SESSION}]` | Parità FEC corrente |

### 4.4 Metriche Derivate (calcolate da Zabbix)

Queste metriche sono calcolate da Zabbix usando **preprocessing** sui counter raw.

| Zabbix Item Key | Formula | Unità | Descrizione |
|---|---|---|---|
| `mpquic.path.wan5.stripe.tx.rate` | `change(mpquic.path.wan5.stripe.tx.bytes) / interval * 8` | bps | Bitrate TX istantaneo VSAT |
| `mpquic.path.wan5.stripe.rx.rate` | `change(mpquic.path.wan5.stripe.rx.bytes) / interval * 8` | bps | Bitrate RX istantaneo VSAT |
| `mpquic.path.wan6.stripe.tx.rate` | `change(mpquic.path.wan6.stripe.tx.bytes) / interval * 8` | bps | Bitrate TX istantaneo Starlink |
| `mpquic.path.wan6.stripe.rx.rate` | `change(mpquic.path.wan6.stripe.rx.bytes) / interval * 8` | bps | Bitrate RX istantaneo Starlink |
| `mpquic.agg.tx.rate` | `change(mpquic.tx.bytes.total) / interval * 8` | bps | Bitrate TX aggregato |
| `mpquic.agg.rx.rate` | `change(mpquic.rx.bytes.total) / interval * 8` | bps | Bitrate RX aggregato |
| `mpquic.session.fec.efficiency[{#S}]` | `fec_recovered / fec_encoded` | ratio | Efficienza FEC |
| `mpquic.session.arq.retx.ratio[{#S}]` | `arq_retx_recv / tx_packets` | ratio | Ratio ritrasmissioni |

---

## 5. Mapping Zabbix — Modalità di Acquisizione

### Opzione A: HTTP Agent + Prometheus Preprocessing (Consigliata)

Questa è la modalità più semplice e nativa in Zabbix 6.0+.

**Configurazione:**

1. **Item master** (tipo: HTTP Agent):
   - URL: `http://<tunnel_ip>:9090/metrics`
   - Tipo informazione: Text
   - Intervallo: 30s
   - Key: `mpquic.prometheus.raw`

2. **Dependent items** (uno per ogni metrica):
   - Tipo: Dependent item
   - Master item: `mpquic.prometheus.raw`
   - Preprocessing: Prometheus pattern
   - Pattern esempio: `mpquic_path_alive{path="wan5"}`

**Esempio preprocessing Prometheus pattern:**

| Item | Prometheus Pattern | Tipo risultato |
|------|-------------------|----------------|
| Path VSAT alive | `mpquic_path_alive{path="wan5"}` | Numeric (unsigned) |
| Path Starlink alive | `mpquic_path_alive{path="wan6"}` | Numeric (unsigned) |
| Stripe TX bytes VSAT | `mpquic_path_stripe_tx_bytes{path="wan5"}` | Numeric (unsigned) |
| Stripe TX bytes Starlink | `mpquic_path_stripe_tx_bytes{path="wan6"}` | Numeric (unsigned) |
| Loss rate sessione | `mpquic_session_loss_rate_pct{session="<id>"}` | Numeric (float) |

### Opzione B: Prometheus → Zabbix Bridge (ambienti grandi)

Se esiste già un Prometheus centrale:
1. Prometheus scrape le TBOX
2. Un adapter/exporter push verso Zabbix trapper items
3. Utile per flotte > 50 TBOX con Prometheus già operativo

### Opzione C: JSON API (alternativa)

Zabbix HTTP Agent su `http://<tunnel_ip>:9090/api/v1/stats` con JSONPath preprocessing. Meno elegante ma utile se il formato Prometheus non è supportato dalla versione Zabbix in uso.

---

## 6. Trigger e Alert

### Trigger per Link (per-path)

| Nome Trigger | Espressione | Severità | Note |
|---|---|---|---|
| VSAT link down | `last(/host/mpquic.path.wan5.alive)=0` | High | Link VSAT non raggiungibile |
| Starlink link down | `last(/host/mpquic.path.wan6.alive)=0` | High | Link Starlink non raggiungibile |
| Entrambi i link down | `last(/host/mpquic.path.wan5.alive)=0 and last(/host/mpquic.path.wan6.alive)=0` | Disaster | Nessuna connettività WAN |
| VSAT throughput zero | `last(/host/mpquic.path.wan5.stripe.tx.rate)=0 and last(/host/mpquic.path.wan5.alive)=1` | Warning | Link attivo ma nessun traffico |
| Starlink throughput zero | `last(/host/mpquic.path.wan6.stripe.tx.rate)=0 and last(/host/mpquic.path.wan6.alive)=1` | Warning | Link attivo ma nessun traffico |

### Trigger per Sessione (server-side)

| Nome Trigger | Espressione | Severità | Note |
|---|---|---|---|
| Loss elevata | `last(/host/mpquic.session.loss.pct[{#SESSION}])>5` | High | Perdita > 5% per sessione |
| Loss warning | `last(/host/mpquic.session.loss.pct[{#SESSION}])>2` | Warning | Perdita > 2% |
| Decrypt failure | `change(/host/mpquic.session.decrypt.fail[{#SESSION}])>0` | High | Fallimento decifratura — possibile anomalia sicurezza |
| ARQ retransmission elevata | `change(/host/mpquic.session.arq.retx[{#SESSION}]) / change(/host/mpquic.session.tx.pkts[{#SESSION}]) > 0.03` | Warning | Ritrasmissioni > 3% TX |
| FEC overload | `last(/host/mpquic.session.fec.adaptive_m[{#SESSION}])>5` | Information | FEC parità alta — canale degradato |
| Duplicati anomali | `change(/host/mpquic.session.arq.dup[{#SESSION}])>100` | Warning | Possibile replay o loop |

### Trigger Globali

| Nome Trigger | Espressione | Severità | Note |
|---|---|---|---|
| Processo MPQUIC down | `nodata(/host/mpquic.uptime,120s)=1` | Disaster | Nessun dato per 2 minuti |
| Uptime reset (riavvio) | `change(/host/mpquic.uptime)<0` | Information | Il processo è stato riavviato |

### Soglie Indicative per Ambiente Satellitare

| Parametro | VSAT (tipico) | Starlink (tipico) | Note |
|---|---|---|---|
| RTT atteso | 550–700 ms | 25–60 ms | Geostationary vs LEO |
| Packet loss fisiologico | 0.1–1% | 0.01–0.5% | Dipendente da condizioni meteo |
| Throughput tipico down | 2–20 Mbps | 50–250 Mbps | Variabile per piano e congestione |
| Throughput tipico up | 1–5 Mbps | 10–40 Mbps | VSAT spesso asimmetrico |
| Jitter atteso | 20–80 ms | 5–30 ms | |

---

## 7. Template Zabbix

Il template XML pronto per l'import è disponibile in:

```
zabbix/zbx_template_mpquic_tbox.xml
```

Il template include:
- 1 item master HTTP Agent per scrape `/metrics`
- Items dependent per tutte le metriche globali, per-path (VSAT/Starlink), e per-session
- Items calcolati per bitrate derivati
- Trigger per link down, loss, decrypt failure, processo down
- Macros configurabili per IP endpoint e soglie

### Macros del Template

| Macro | Default | Descrizione |
|---|---|---|
| `{$MPQUIC.ENDPOINT}` | `http://{HOST.CONN}:9090/metrics` | URL endpoint metriche |
| `{$MPQUIC.SCRAPE.INTERVAL}` | `30s` | Intervallo di scrape |
| `{$MPQUIC.LOSS.WARN}` | `2` | Soglia loss warning (%) |
| `{$MPQUIC.LOSS.HIGH}` | `5` | Soglia loss high (%) |
| `{$MPQUIC.NODATA.TIMEOUT}` | `120s` | Timeout no-data per processo down |
| `{$MPQUIC.WAN.VSAT}` | `wan5` | Nome path VSAT |
| `{$MPQUIC.WAN.STARLINK}` | `wan6` | Nome path Starlink |

---

## 8. Pacchetto Minimo vs Esteso

### Pacchetto Minimo (10 KPI — monitoraggio operativo base)

| # | Metrica | Ambito | Motivo |
|---|---------|--------|--------|
| 1 | `mpquic_path_alive` (wan5) | per-link | Disponibilità VSAT |
| 2 | `mpquic_path_alive` (wan6) | per-link | Disponibilità Starlink |
| 3 | `mpquic_path_stripe_tx_bytes` (wan5) | per-link | Volume TX VSAT |
| 4 | `mpquic_path_stripe_tx_bytes` (wan6) | per-link | Volume TX Starlink |
| 5 | `mpquic_path_stripe_rx_bytes` (wan5) | per-link | Volume RX VSAT |
| 6 | `mpquic_path_stripe_rx_bytes` (wan6) | per-link | Volume RX Starlink |
| 7 | `mpquic_tx_bytes_total` | globale | Throughput aggregato TX |
| 8 | `mpquic_rx_bytes_total` | globale | Throughput aggregato RX |
| 9 | `mpquic_session_loss_rate_pct` | sessione | Qualità servizio |
| 10 | `mpquic_uptime_seconds` | globale | Salute processo |

### Pacchetto Esteso (aggiuntivi per troubleshooting e SLA)

| # | Metrica | Motivo |
|---|---------|--------|
| 11 | `mpquic_session_fec_recovered` | Efficacia FEC — quanti pacchetti recuperati |
| 12 | `mpquic_session_fec_encoded` | Volume codifica FEC |
| 13 | `mpquic_session_arq_nack_sent` | Segnale di perdita pre-recovery |
| 14 | `mpquic_session_arq_retx_recv` | Volume ritrasmissioni |
| 15 | `mpquic_session_arq_dup_filtered` | Duplicati — indicatore anomalia |
| 16 | `mpquic_session_decrypt_fail` | Security — fallimenti crypto |
| 17 | `mpquic_session_pipes` | Numero pipe attive |
| 18 | `mpquic_session_adaptive_m` | Stato FEC adattivo |
| 19 | `mpquic_path_stripe_fec_recovered` | FEC recovery per-path |
| 20 | Bitrate derivati (calculated) | Throughput in bps per dashboard |

---

## 9. Best Practice

### Label e Cardinalità

- Le label `path` e `session` hanno cardinalità bassa e controllata (2 path, poche sessioni)
- **Non** inserire in Zabbix item per ogni combinazione IP sorgente — usare solo le label strutturali
- Per flotte grandi (>50 TBOX), valutare Zabbix Proxy dedicato per evitare saturazione del server centrale

### Retention

| Tipo dato | Retention suggerita |
|-----------|-------------------|
| Raw values | 7 giorni |
| Trend (hourly) | 365 giorni |
| Events/Alert | 180 giorni |

### Sicurezza

- L'endpoint metriche è esposto **solo** sull'IP tunnel (rete privata)
- Zabbix accede via rete tunnel/ZeroTier — **non** esporre su interfacce pubbliche
- In ambienti con requisiti NIS2, valutare TLS mutual auth tra Zabbix e endpoint

### Naming Convention Host Zabbix

```
Template:  TBOX-MPQUIC-<site>-<tbox_id>
Esempio:   TBOX-MPQUIC-NAVE01-001
Gruppi:    TBOX / Satellite / MPQUIC
```

### Intervallo di Scrape

- **30 secondi**: bilanciamento ottimale tra reattività e carico
- **15 secondi**: per ambienti critici con SLA stringenti
- **60 secondi**: per flotte molto grandi (>100 TBOX) per ridurre carico server Zabbix
