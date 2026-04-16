# Zabbix Template — TBOX MPQUIC Satellite

## Contenuto

| File | Descrizione |
|------|-------------|
| `zbx_template_mpquic_tbox.xml` | Template Zabbix 6.0+ per monitoraggio TBOX MPQUIC (VSAT + Starlink) |

## Import

1. Aprire Zabbix Web UI → **Configuration** → **Templates**
2. Click **Import** (in alto a destra)
3. Selezionare `zbx_template_mpquic_tbox.xml`
4. Confermare l'import

## Assegnazione a un Host

1. **Configuration** → **Hosts** → selezionare la TBOX
2. Tab **Templates** → **Link new templates** → cercare _"TBOX MPQUIC Satellite"_
3. Verificare che l'**Agent interface** dell'host punti all'IP tunnel della TBOX (es. `10.200.17.1`)
4. Il template usa la macro `{$MPQUIC.ENDPOINT}` che di default è `http://{HOST.CONN}:9090/metrics`

## Macros Personalizzabili

| Macro | Default | Descrizione |
|---|---|---|
| `{$MPQUIC.ENDPOINT}` | `http://{HOST.CONN}:9090/metrics` | URL endpoint Prometheus |
| `{$MPQUIC.SCRAPE.INTERVAL}` | `30s` | Frequenza scrape |
| `{$MPQUIC.LOSS.WARN}` | `2` | Soglia loss warning (%) |
| `{$MPQUIC.LOSS.HIGH}` | `5` | Soglia loss critica (%) |
| `{$MPQUIC.NODATA.TIMEOUT}` | `120s` | Timeout no-data → allarme |
| `{$MPQUIC.WAN.VSAT}` | `wan5` | Nome path VSAT |
| `{$MPQUIC.WAN.STARLINK}` | `wan6` | Nome path Starlink |

Per override su singolo host: **Host** → **Macros** → sovrascrivere il valore desiderato.

## Requisiti

- Zabbix Server/Proxy ≥ 6.0 (per supporto HTTP Agent + Prometheus preprocessing)
- Raggiungibilità di rete tra Zabbix e l'IP tunnel della TBOX sulla porta 9090
- MPQUIC con `metrics_listen: auto` abilitato nella configurazione YAML

## Struttura Items

```
mpquic.prometheus.raw          ← HTTP Agent master (scrape /metrics)
├── mpquic.uptime              ← Dependent: uptime processo
├── mpquic.tx.bytes.total      ← Dependent: byte TX totali
├── mpquic.rx.bytes.total      ← Dependent: byte RX totali
├── mpquic.tx.pkts.total       ← Dependent: pacchetti TX totali
├── mpquic.rx.pkts.total       ← Dependent: pacchetti RX totali
│
├── mpquic.path.wan5.alive           ← VSAT link state
├── mpquic.path.wan5.stripe.tx.bytes ← VSAT stripe TX
├── mpquic.path.wan5.stripe.rx.bytes ← VSAT stripe RX
├── mpquic.path.wan5.tx.pkts         ← VSAT TX pkts
├── mpquic.path.wan5.rx.pkts         ← VSAT RX pkts
├── mpquic.path.wan5.fec.recovered   ← VSAT FEC recovered
│
├── mpquic.path.wan6.alive           ← Starlink link state
├── mpquic.path.wan6.stripe.tx.bytes ← Starlink stripe TX
├── mpquic.path.wan6.stripe.rx.bytes ← Starlink stripe RX
├── mpquic.path.wan6.tx.pkts         ← Starlink TX pkts
├── mpquic.path.wan6.rx.pkts         ← Starlink RX pkts
├── mpquic.path.wan6.fec.recovered   ← Starlink FEC recovered
│
├── mpquic.session.loss.pct          ← Loss rate %
├── mpquic.session.pipes             ← Pipe attive
├── mpquic.session.fec.encoded       ← FEC encoded
├── mpquic.session.fec.recovered     ← FEC recovered
├── mpquic.session.arq.nack          ← ARQ NACK
├── mpquic.session.arq.retx          ← ARQ retransmissions
├── mpquic.session.arq.dup           ← Duplicates filtered
├── mpquic.session.decrypt.fail      ← Decrypt failures
├── mpquic.session.fec.adaptive_m    ← FEC parity M
└── mpquic.session.uptime            ← Session uptime

mpquic.path.wan5.stripe.tx.rate  ← Calculated: VSAT TX bps
mpquic.path.wan5.stripe.rx.rate  ← Calculated: VSAT RX bps
mpquic.path.wan6.stripe.tx.rate  ← Calculated: Starlink TX bps
mpquic.path.wan6.stripe.rx.rate  ← Calculated: Starlink RX bps
mpquic.agg.tx.rate               ← Calculated: Aggregate TX bps
mpquic.agg.rx.rate               ← Calculated: Aggregate RX bps
```

## Trigger

| Trigger | Severità |
|---|---|
| VSAT link down | High |
| Starlink link down | High |
| ALL WAN links down | Disaster |
| Packet loss warning (>2%) | Warning |
| Packet loss HIGH (>5%) | High |
| Decrypt failure detected | High |
| Process unreachable | Disaster |
| Process restarted | Info |
| FEC high parity (M>5) | Warning |
| VSAT link up but zero throughput | Warning |
| Starlink link up but zero throughput | Warning |

## Documentazione

Vedere [docs/ZABBIX_TBOX_METRICS.md](../docs/ZABBIX_TBOX_METRICS.md) per la specifica completa.
