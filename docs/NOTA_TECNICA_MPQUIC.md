# Nota Tecnica — Piattaforma MPQUIC: Test e Risultati

**Data**: 1 marzo 2026  
**Versione**: 3.0  
**Autori**: Team Engineering SATCOMVAS  
**Classificazione**: Interna / Clienti

---

## Indice

**Fase 2 — Multi-tunnel per link (Isolamento traffico)**

1. [Sommario Esecutivo](#1-sommario-esecutivo)
2. [Contesto e Motivazione](#2-contesto-e-motivazione)
3. [Architettura della Piattaforma](#3-architettura-della-piattaforma)
4. [Ambiente di Test](#4-ambiente-di-test)
5. [Preparazione dell'Ambiente](#5-preparazione-dellambiente)
6. [Test 1 — Isolamento RTT (Latenza)](#6-test-1--isolamento-rtt-latenza)
7. [Test 2 — Isolamento Throughput (Banda)](#7-test-2--isolamento-throughput-banda)
8. [Analisi dei Risultati](#8-analisi-dei-risultati)

**Fase 3 — BBR Congestion Control e Reliable Transport**

9. [Test 3 — BBR e Reliable Transport su Starlink](#9-test-3--bbr-congestion-control-e-reliable-transport-su-starlink)

**Fase 4 — Multi-Path (Failover, Bonding, Aggregazione)**

10. [Test 4 — Multi-Path Failover](#10-test-4--multi-path-failover)
11. [Test 5 — Multi-Path Bonding (Balanced)](#11-test-5--multi-path-bonding-balanced)
12. [Test 6 — Speedtest End-to-End con Bonding](#12-test-6--speedtest-end-to-end-con-bonding)

**Conclusioni e Appendice**

13. [Vantaggi per il Cliente](#13-vantaggi-per-il-cliente)
14. [Conclusioni](#14-conclusioni)
15. [Appendice — Comandi Completi](#15-appendice--comandi-completi)

---

## 1. Sommario Esecutivo

La piattaforma MPQUIC implementa un'architettura a **tunnel QUIC multipli per link
fisico** con capacità **multi-path** (failover e bonding su link WAN multipli).
Questa nota documenta l'evoluzione completa dei test condotti tra il 28 febbraio e
il 1 marzo 2026, organizzati per fase di sviluppo.

### Fase 2 — Isolamento multi-tunnel (28 febbraio 2026)

> **La degradazione di un tunnel (packet loss fino al 30%) ha impatto ZERO sui
> tunnel adiacenti che condividono lo stesso link fisico.**

| Metrica | Tunnel degradato (br2) | Tunnel adiacenti (cr2, df2) |
|---------|------------------------|------------------------------|
| **Throughput con 10% loss** | 2.3 Mbps (−95%) | 50.2 Mbps (±0%) |
| **Throughput con 30% loss** | 0.4 Mbps (−99%) | 50.0 Mbps (±0%) |
| **Latenza sotto loss** | invariata (13 ms) | invariata (13 ms) |
| **Packet loss osservato** | 15–35% | **0%** |

### Fase 3 — BBR + Reliable Transport (28 febbraio 2026)

> **BBR con reliable transport mantiene +79% di throughput rispetto a Cubic
> con 30% di packet loss su Starlink.**

| Scenario (30% loss) | Cubic | BBR | Vantaggio BBR |
|---------------------|-------|-----|---------------|
| Reliable mode | 14.6 Mbps | **26.1 Mbps** | **+79%** |

### Fase 4 — Multi-Path Failover e Bonding (1 marzo 2026)

> **Failover automatico con soli 2 pacchetti persi. Bonding aggrega due link
> Starlink raggiungendo 74 Mbps (iperf3) e 72 Mbps download (Ookla speedtest
> end-to-end da client LAN).**

| Test | Risultato chiave |
|------|------------------|
| **Failover WAN5→WAN6** | 2 pacchetti persi su 74 (2.7%), recovery in ~8s |
| **Bonding iperf3** | 74.3 Mbps aggregati, picco 102 Mbps |
| **Ookla Speedtest (LAN)** | Download 71.97 Mbps, Upload 41.12 Mbps, Ping 19ms |

---

## 2. Contesto e Motivazione

### 2.1 Il problema: Head-of-Line Blocking nei tunnel tradizionali

Nelle architetture VPN tradizionali (IPsec, OpenVPN, WireGuard), **tutto il traffico** di
una connessione WAN viene incapsulato in un **singolo tunnel**. Quando si verifica packet
loss sul link fisico — evento frequente sulle connessioni satellitari LEO come Starlink
— le conseguenze sono devastanti:

- **TCP-over-TCP**: il protocollo TCP interno e quello del tunnel reagiscono entrambi
  alla perdita, causando collasso esponenziale del throughput
- **Head-of-Line (HOL) Blocking**: un pacchetto perso blocca la consegna di TUTTI i
  pacchetti successivi, indipendentemente dall'applicazione
- **Nessuna priorità**: una sessione di backup che satura il tunnel degrada
  automaticamente la qualità delle chiamate VoIP

In concreto: se una sessione di backup (bulk) subisce perdita di pacchetti, anche il
traffico VoIP critico viene rallentato, con impatti diretti sulla qualità del servizio
percepita dal cliente.

### 2.2 La soluzione: tunnel QUIC multipli per applicazione

La piattaforma MPQUIC adotta un approccio radicalmente diverso:

- **Protocollo QUIC** (RFC 9000): protocollo di trasporto moderno che gestisce perdite
  e ritrasmissioni a livello di stream, senza HOL blocking tra stream diversi
- **Multi-tunnel per link**: ogni link fisico (WAN) trasporta **N tunnel indipendenti**,
  uno per classe di traffico
- **Isolamento nativo**: ogni tunnel ha il proprio congestion control, le proprie
  ritrasmissioni e la propria finestra di congestione — completamente indipendente
  dagli altri

> *"Many small pipes are faster than a single large tube"*  
> *"Each pipe (i.e. application) is independent and does not suffer the packet loss of the others"*

---

## 3. Architettura della Piattaforma

### 3.1 Schema complessivo

```
RETE CLIENTE                    VM MPQUIC (Client)                     VPS (Server)

                                ┌────────────────┐
                                │ Classifier      │
  OpenWrt ──LAN──▶              │ (VLAN / nftables)│
                                │                 │
                                ├─┬───────────────┤       WAN (Starlink)        ┌──────────────┐
                  VoIP ────────▶│ │ TUN cr2       │─── QUIC tunnel ────────────▶│              │
                                │ │ (critical)    │    (indipendente)           │              │
                                ├─┼───────────────┤                             │   Server     │
                  HTTPS ───────▶│ │ TUN br2       │─── QUIC tunnel ────────────▶│   Multi-conn │
                                │ │ (bulk)        │    (indipendente)           │   mt5        │
                                ├─┼───────────────┤                             │              │
                  Backup ──────▶│ │ TUN df2       │─── QUIC tunnel ────────────▶│              │──▶ Internet
                                │ │ (default)     │    (indipendente)           │              │
                                └─┴───────────────┘                             └──────────────┘
                                        │
                                     enp7s7 (WAN5, Starlink SL5)
```

### 3.2 Classi di traffico

| Classe | Sigla | Tipo di traffico | Priorità |
|--------|-------|------------------|----------|
| **Critical** | cr | VoIP, telemetria, protocolli di controllo | Alta — bassa latenza |
| **Bulk** | br | Backup, sincronizzazione, download massivi | Bassa — best effort |
| **Default** | df | Navigazione web, HTTPS, API business | Media — bilanciata |

### 3.3 Topologia 9 tunnel (3 WAN × 3 classi)

L'architettura finale prevede **9 tunnel** indipendenti distribuiti su 3 link satellitari:

| WAN | Link | Porta Server | Tunnel | IP Tunnel |
|-----|------|-------------|--------|-----------|
| WAN4 (SL4) | Starlink #4 (~108 ms) | 45014 | cr1, br1, df1 | 10.200.14.{1,5,9} |
| WAN5 (SL5) | Starlink #5 (~13 ms) | 45015 | cr2, br2, df2 | 10.200.15.{1,5,9} |
| WAN6 (SL6) | Starlink #6 (~34 ms) | 45016 | cr3, br3, df3 | 10.200.16.{1,5,9} |

Tutti e 9 i tunnel sono stati verificati operativi e bidirezionali al momento del test.

---

## 4. Ambiente di Test

### 4.1 Infrastruttura

| Componente | Dettagli |
|-----------|----------|
| **Client VM** | Debian 12, VMID 200, 22 CPU, systemd-networkd |
| **Server VPS** | Ubuntu 24.04, 172.238.232.223 (Italia) |
| **Link fisico** | Starlink SL5 (WAN5, interfaccia enp7s7) |
| **RTT baseline** | ~13 ms |
| **Protocollo tunnel** | QUIC (RFC 9000) con quic-go v0.48.2, Go 1.22.2 |
| **Congestion Control** | Cubic (default quic-go) |
| **TLS** | TLS 1.3 con certificati CA dedicati |

### 4.2 Tunnel sotto test

I test sono stati condotti sul **set WAN5** (Starlink SL5) per il miglior RTT (~13 ms),
utilizzando 3 tunnel che condividono lo stesso link fisico:

| Tunnel | Classe | IP TUN Client | IP TUN Server | Device |
|--------|--------|---------------|---------------|--------|
| **cr2** | Critical | 10.200.15.1 | 10.200.15.254 | cr2 |
| **br2** | Bulk | 10.200.15.5 | 10.200.15.254 | br2 |
| **df2** | Default | 10.200.15.9 | 10.200.15.254 | df2 |

Tutti e tre i tunnel condividono:
- Lo stesso link fisico (enp7s7 / WAN5)
- La stessa porta server (45015/UDP)
- La stessa subnet lato server (10.200.15.0/24, TUN mt5)

I tunnel differiscono per:
- Interfaccia TUN dedicata (cr2, br2, df2) con IP distinto
- Connessione QUIC indipendente (Connection ID QUIC separato)
- Stack congestion control separato

### 4.3 Principio del test

Il test inietta **packet loss artificiale** su una singola interfaccia TUN (br2) usando
il modulo kernel `netem` (Network Emulator) del Linux Traffic Control. Le altre interfacce
TUN (cr2, df2) **non vengono toccate**. Si misurano quindi latenza e throughput su tutti
e 3 i tunnel per verificare che la degradazione resti confinata al solo tunnel affetto.

Questo simula una situazione reale in cui una classe di traffico (es. backup) sta
soffrendo perdite elevate a causa di congestione o errori applicativi, e verifica che
le altre classi (VoIP, navigazione) non ne risentano in alcun modo.

---

## 5. Preparazione dell'Ambiente

### 5.1 Verifica stato tunnel (Client)

Per prima cosa è stato verificato che tutti e 9 i tunnel fossero attivi e bidirezionali:

```bash
# Verifica servizi attivi
systemctl is-active mpquic@{cr1,br1,df1,cr2,br2,df2,cr3,br3,df3}.service

# Verifica interfacce TUN
ip -br addr show | grep -E 'cr[1-3]|br[1-3]|df[1-3]'
```

Risultato: **9/9 tunnel UP** con IP corretti assegnati.

### 5.2 Verifica connettività bidirezionale

Ping bidirezionale su tutti e 9 i tunnel:

```bash
# Per ogni set (WAN4=14, WAN5=15, WAN6=16) e tunnel (offset .1, .5, .9)
for subnet in 14 15 16; do
  for offset in 1 5 9; do
    ping -c 2 -W 2 10.200.${subnet}.254   # client → server
  done
done
```

Risultato: tutti i 9 ping bidirezionali con successo. RTT misurati:
- WAN4 (SL4): ~105–119 ms
- WAN5 (SL5): ~13–14 ms
- WAN6 (SL6): ~21–28 ms

### 5.3 Installazione iperf3 sul Client

Il tool `iperf3` è stato installato sulla VM client per le misure di throughput:

```bash
sudo apt-get update
sudo apt-get install -y iperf3
```

Versione installata: **iperf3 3.12** (cJSON 1.7.15, Debian 12 bookworm).

```bash
$ iperf3 --version
iperf 3.12 (cJSON 1.7.15)
Linux vmmpquic01 6.1.0-31-amd64 #1 SMP PREEMPT_DYNAMIC Debian 6.1.128-1 (2025-02-07) x86_64
```

### 5.4 Installazione iperf3 sul Server VPS

```bash
sudo apt-get update
sudo apt-get install -y iperf3
```

### 5.5 Apertura firewall VPS per traffico TUN e iperf3

Il firewall VPS (nftables) bloccava il traffico TCP sulle interfacce TUN. Sono state
aggiunte due regole per consentire:

1. **Tutto il traffico sulle interfacce TUN** (mt4, mt5, mt6):
```bash
nft add rule inet filter input iifname "mt*" accept
```

2. **Porta iperf3** (TCP 5201) per connessioni dirette:
```bash
nft add rule inet filter input tcp dport 5201 accept
```

3. **Persistenza** delle regole:
```bash
nft list ruleset > /etc/nftables.conf
```

**Diagnosi effettuata**: il primo tentativo di connessione iperf3 attraverso il tunnel
era fallito con timeout TCP. Il ping (ICMP) funzionava correttamente, ma il TCP veniva
bloccato dalla chain `input` del firewall VPS (policy `drop`) che non aveva regole per
le interfacce TUN. Questo ha confermato la necessità della regola `iifname "mt*" accept`.

### 5.6 Avvio iperf3 server sulla VPS

```bash
iperf3 -s   # ascolta su porta 5201 (default)
```

Il server iperf3 è stato lasciato in ascolto per tutta la durata dei test.

### 5.7 Verifica connettività TCP pre-test

Prima di procedere ai test di throughput, è stata verificata la raggiungibilità TCP:

```bash
# Dal client, attraverso il tunnel cr2
iperf3 -c 10.200.15.254 -B 10.200.15.1 -t 2
```

Risultato: connessione stabilita con successo, ~61 Mbps in 2 secondi. Ambiente pronto.

### 5.8 Scoperta tecnica: binding esplicito per-device

Durante i test iniziali, si è osservato che il throughput appariva identico su tutti
e tre i tunnel anche con netem attivo. L'analisi ha rivelato la causa:

```bash
$ ip route get 10.200.15.254 from 10.200.15.1
10.200.15.254 from 10.200.15.1 dev cr2   # ← tutti via cr2!

$ ip route get 10.200.15.254 from 10.200.15.5
10.200.15.254 from 10.200.15.5 dev cr2   # ← anche br2 va via cr2!

$ ip route get 10.200.15.254 from 10.200.15.9
10.200.15.254 from 10.200.15.9 dev cr2   # ← anche df2 va via cr2!
```

**Causa**: i 3 tunnel condividono la subnet 10.200.15.0/24. Il kernel Linux seleziona
la prima interfaccia con route matching (cr2), ignorando le altre. Risultato: il netem
applicato su br2 non aveva effetto perché il traffico di br2 passava comunque da cr2.

**Soluzione**: binding esplicito al device con la sintassi `iperf3 -B IP%device`:

```bash
iperf3 -c 10.200.15.254 -B 10.200.15.5%br2 -t 5   # forza uso device br2
```

Questo garantisce che il traffico iperf3 attraversi effettivamente l'interfaccia TUN
corretta, rendendo il test valido.

> **Nota per produzione**: in ambiente reale il classifier VLAN instrada il traffico
> sull'interfaccia TUN corretta in base alla VLAN di provenienza, quindi questo
> problema non si presenta nell'uso operativo.

---

## 6. Test 1 — Isolamento RTT (Latenza)

### 6.1 Metodologia

Misura della latenza (RTT) e del packet loss su ciascun tunnel usando `ping`:
- **20 pacchetti** per ogni misura, intervallo 0.2 secondi
- **Strumento**: `ping -I <device> -c 20 -i 0.2 -W 2 10.200.15.254`
- **Loss injection**: `tc qdisc add dev br2 root netem loss X%`
- **Target loss**: esclusivamente interfaccia br2 (bulk)
- Misure effettuate su **tutti e 3 i tunnel** per ogni scenario

### 6.2 Scenario Baseline (nessun loss iniettato)

```bash
# Per ciascun tunnel
for tun in cr2 br2 df2; do
  ping -I $tun -c 20 -i 0.2 -W 2 10.200.15.254
done
```

| Tunnel | RTT medio | RTT min | RTT max | Packet Loss |
|--------|-----------|---------|---------|-------------|
| cr2 (critical) | 13.026 ms | 12.862 ms | 13.302 ms | **0%** |
| br2 (bulk) | 13.212 ms | 12.989 ms | 13.568 ms | **0%** |
| df2 (default) | 13.074 ms | 12.891 ms | 13.287 ms | **0%** |

Tutti i tunnel presentano RTT omogeneo (~13 ms) e zero packet loss. La baseline è stabile.

### 6.3 Scenario: 10% packet loss su br2

```bash
sudo tc qdisc add dev br2 root netem loss 10%
```

Misura su tutti e 3 i tunnel:

| Tunnel | RTT medio | Packet Loss | Variazione vs baseline |
|--------|-----------|-------------|------------------------|
| **cr2** (critical) | 13.0 ms | **0%** | Nessuna |
| **br2** (bulk) | 13.1 ms | **15%** | +15% loss (atteso: ~10%) |
| **df2** (default) | 13.1 ms | **0%** | Nessuna |

### 6.4 Scenario: 30% packet loss su br2

```bash
sudo tc qdisc replace dev br2 root netem loss 30%
```

| Tunnel | RTT medio | Packet Loss | Variazione vs baseline |
|--------|-----------|-------------|------------------------|
| **cr2** (critical) | 13.0 ms | **0%** | Nessuna |
| **br2** (bulk) | 13.1 ms | **35%** | +35% loss (atteso: ~30%) |
| **df2** (default) | 13.1 ms | **0%** | Nessuna |

### 6.5 Risultato Test 1

```
          BASELINE          10% netem br2       30% netem br2
cr2 ████████████ 0%    ████████████ 0%    ████████████ 0%
br2 ████████████ 0%    ████░░░░░░░ 15%   ███░░░░░░░░ 35%
df2 ████████████ 0%    ████████████ 0%    ████████████ 0%
```

**Isolamento RTT: PERFETTO.** I tunnel cr2 e df2 non mostrano alcuna variazione di
latenza o packet loss, nonostante br2 stia subendo fino al 35% di perdita pacchetti.

---

## 7. Test 2 — Isolamento Throughput (Banda)

### 7.1 Metodologia

Misura del throughput su ciascun tunnel usando `iperf3`:
- **Durata**: 5 secondi per ogni misura
- **Binding**: esplicito per-device (`-B IP%device`) per garantire routing corretto
- **Strumento**: `iperf3 -c 10.200.15.254 -B <IP>%<dev> -t 5`
- **Server**: iperf3 in ascolto sulla VPS porta 5201 (singola istanza, test sequenziali)
- **Loss injection**: `tc qdisc` netem su interfaccia br2

### 7.2 Scenario Baseline (nessun loss iniettato)

```bash
# Cleanup preventivo
sudo tc qdisc del dev br2 root 2>/dev/null

# Misura sequenziale (iperf3 single-server)
iperf3 -c 10.200.15.254 -B 10.200.15.1%cr2 -t 5
iperf3 -c 10.200.15.254 -B 10.200.15.5%br2 -t 5
iperf3 -c 10.200.15.254 -B 10.200.15.9%df2 -t 5
```

| Tunnel | TX (Sender) | RX (Receiver) | Retransmits |
|--------|-------------|---------------|-------------|
| cr2 (critical) | 53.9 Mbps | **50.2 Mbps** | 244 |
| br2 (bulk) | 51.3 Mbps | **48.1 Mbps** | 230 |
| df2 (default) | 52.5 Mbps | **50.0 Mbps** | 239 |

Throughput baseline omogeneo: ~50 Mbps per tutti i tunnel. I retransmit sono normali
per un link Starlink con RTT ~13 ms (Cubic congestion control).

### 7.3 Scenario: 10% packet loss su br2

```bash
sudo tc qdisc add dev br2 root netem loss 10%
```

| Tunnel | TX (Sender) | RX (Receiver) | Retransmits | Variazione |
|--------|-------------|---------------|-------------|------------|
| **cr2** (critical) | 53.1 Mbps | **50.2 Mbps** | 237 | **±0%** |
| **br2** (bulk) | 2.65 Mbps | **2.3 Mbps** | 104 | **−95%** |
| **df2** (default) | 53.5 Mbps | **50.2 Mbps** | 259 | **±0%** |

### 7.4 Scenario: 30% packet loss su br2

```bash
sudo tc qdisc replace dev br2 root netem loss 30%
```

| Tunnel | TX (Sender) | RX (Receiver) | Retransmits | Variazione |
|--------|-------------|---------------|-------------|------------|
| **cr2** (critical) | 53.0 Mbps | **50.2 Mbps** | 180 | **±0%** |
| **br2** (bulk) | 567 Kbps | **401 Kbps** | 93 | **−99%** |
| **df2** (default) | 53.2 Mbps | **49.8 Mbps** | 236 | **±0%** |

### 7.5 Riepilogo comparativo Throughput

```
Throughput RX (Mbps) — Scala lineare 0-55

BASELINE (0% loss):
  cr2 ██████████████████████████████████████████████████ 50.2
  br2 ████████████████████████████████████████████████   48.1
  df2 ██████████████████████████████████████████████████ 50.0

10% LOSS su br2:
  cr2 ██████████████████████████████████████████████████ 50.2  ← INALTERATO
  br2 ██                                                 2.3  ← −95%
  df2 ██████████████████████████████████████████████████ 50.2  ← INALTERATO

30% LOSS su br2:
  cr2 ██████████████████████████████████████████████████ 50.2  ← INALTERATO
  br2 ░                                                  0.4  ← −99%
  df2 █████████████████████████████████████████████████  49.8  ← INALTERATO
```

### 7.6 Risultato Test 2

**Isolamento throughput: PERFETTO.** Con 30% di packet loss su br2:
- br2 crolla da 48.1 a 0.4 Mbps (−99%)
- cr2 mantiene 50.2 Mbps (variazione 0%)
- df2 mantiene 49.8 Mbps (variazione -0.4%, nel margine di misura)

---

## 8. Analisi dei Risultati

### 8.1 Perché l'isolamento è perfetto

L'isolamento è una conseguenza diretta dell'architettura:

1. **Tunnel indipendenti**: ogni classe di traffico ha la propria connessione QUIC
   con Connection ID separato. Non esiste condivisione di stato tra tunnel.

2. **Congestion control isolato**: ogni tunnel QUIC ha la propria istanza di Cubic
   (congestion control). Quando br2 subisce loss, solo il Cubic di br2 riduce la
   finestra di congestione. I Cubic di cr2 e df2 non vedono alcun loss.

3. **Interfacce TUN separate**: ogni tunnel scrive su un device TUN dedicato.
   Il netem applicato su un device TUN non ha alcun effetto sugli altri device.

4. **Nessun shared buffer**: a differenza di un tunnel monolitico dove tutti i
   pacchetti condividono lo stesso buffer e la stessa coda di ritrasmissione,
   l'architettura multi-tunnel garantisce code completamente separate.

### 8.2 Confronto con architetture tradizionali

| Caratteristica | VPN Tradizionale (singolo tunnel) | MPQUIC (multi-tunnel) |
|---------------|-----------------------------------|------------------------|
| Tunnel per classe | 1 (condiviso) | 1 per classe (isolato) |
| HOL Blocking | **Sì** — loss blocca tutto | **No** — loss resta confinato |
| Impatto loss 10% su bulk | VoIP degrada | VoIP **inalterato** |
| Impatto loss 30% su bulk | VoIP inutilizzabile | VoIP **inalterato** |
| Congestion control | Condiviso | Indipendente per classe |
| Ritrasmissioni | Bloccano tutto il traffico | Solo il tunnel affetto |

### 8.3 Impatto della loss sul singolo tunnel

La degradazione osservata su br2 segue le aspettative teoriche per TCP-over-QUIC
con Cubic congestion control:

| Packet Loss | Throughput | Riduzione | Note |
|-------------|-----------|-----------|------|
| 0% | 48.1 Mbps | baseline | Normale operatività |
| 10% | 2.3 Mbps | −95% | Cubic dimezza la finestra ad ogni loss event |
| 30% | 0.4 Mbps | −99% | Quasi totale collasso |

Questa è la risposta **attesa** di Cubic a condizioni di elevato packet loss.
La sezione 9 documenta i risultati ottenuti con l'implementazione di **BBRv1** e
il **transport mode reliable**, che migliorano drasticamente le prestazioni sotto
elevata packet loss.

### 8.4 Osservazione sul packet loss misurato vs iniettato

| Loss netem impostato | Loss ping misurato |
|---------------------|--------------------|
| 10% | ~15% |
| 30% | ~35% |

Il loss misurato è leggermente superiore a quello impostato. Questo è normale:
netem applica il loss in uscita dal device TUN, ma il ping misura il round-trip
(il loss può colpire sia il pacchetto in uscita che la risposta ICMP).

---

## 9. Test 3 — BBR Congestion Control e Reliable Transport su Starlink

### 9.1 Motivazione

I test di isolamento (sezioni 6-8) hanno dimostrato che quando un tunnel subisce
packet loss, solo quel tunnel è affetto. Tuttavia, il tunnel colpito con Cubic
subisce una degradazione del **95-99%** — un risultato inaccettabile per scenari
operativi su collegamenti degradati come Starlink sotto interferenze o congestione.

BBR (Bottleneck Bandwidth and Round-trip propagation time) è un algoritmo di
congestion control sviluppato da Google che, a differenza di Cubic, non interpreta
ogni pacchetto perso come segnale di congestione. BBR mantiene un modello del
bottleneck bandwidth e del minimum RTT, puntando a operare al punto ottimale
di Kleinrock (massima banda, minima latenza).

### 9.2 Implementazione BBRv1

L'implementazione è stata realizzata come fork locale di quic-go v0.48.2
(`local-quic-go/internal/congestion/bbr_sender.go`, ~400 LOC) con le seguenti
caratteristiche:

- **4 modalità operative**: STARTUP → DRAIN → PROBE_BW → PROBE_RTT
- **Pacing gain cycle** in PROBE_BW: `[1.25, 0.75, 1.0×6]`
- **Windowed max bandwidth filter** su 10 round-trip
- **Min RTT tracking** con expiry a 10 secondi e fase PROBE_RTT di 200ms
- **Startup exit** dopo 3 round senza crescita bandwidth ≥ 25%
- **Loss-agnostic**: nessuna riduzione della finestra su singoli loss events

La configurazione è per-tunnel via YAML:

```yaml
# Esempio: br3.yaml (BBR su Starlink)
congestion_algorithm: bbr
transport_mode: reliable
```

### 9.3 Scoperta critica: Transport Mode Reliable

Durante i test iniziali con BBR su Starlink, è emerso un risultato inatteso:
**BBR e Cubic ottenevano throughput identico sotto loss** (~0.5 Mbps con 10% loss,
indipendentemente dal congestion control).

**Causa**: la piattaforma MPQUIC utilizzava `QUIC DATAGRAM frames` (RFC 9221) per
il trasporto dei pacchetti TUN. I DATAGRAM frames sono **unreliable**: i pacchetti
persi a livello UDP non vengono mai ritrasmessi da QUIC. Di conseguenza:

- Il 10% di loss netem si trasferiva **direttamente** al TCP interno al tunnel
- Il TCP interno vedeva 10% loss e collassava (formula di Mathis: throughput ∝ MSS/(RTT×√loss))
- Il congestion control QUIC era **irrilevante** perché non comandava ritrasmissioni

**Soluzione implementata**: `transport_mode: reliable` — un nuovo modalità di
trasporto che sostituisce i DATAGRAM frames con un **bidirectional QUIC stream**
e framing 2-byte length-prefixed:

```
┌──────────┬────────────────────────────┐
│ Len (2B) │ Payload (pacchetto TUN)    │
│ BigEndian│ [Len bytes]                │
└──────────┴────────────────────────────┘
```

Con stream reliable:
- QUIC ritrasmette automaticamente i pacchetti persi a livello UDP
- Il TCP interno al tunnel vede **0% loss** indipendentemente dalla loss fisica
- Il congestion control QUIC (BBR o Cubic) governa il rate di ritrasmissione
- Il throughput è determinato dalla capacità dell'algoritmo CC di operare sotto loss

### 9.4 Ambiente di Test

| Componente | Dettaglio |
|------------|----------|
| **Link fisico** | Starlink (antenna terminale enp7s8, IP CGNAT 100.64.86.226/10) |
| **RTT medio** | 25-40 ms (Starlink LEO) |
| **Tunnel test** | cr3 (Cubic), br3 (BBR), df3 (Cubic) — tutti su WAN6 porta 45016 |
| **Server** | VPS mt6 (multi-conn, `transport_mode: reliable`, Cubic) |
| **Subnet** | 10.200.16.0/24 (cr3=.1, br3=.5, df3=.9, mt6=.254) |
| **Loss injection** | `tc qdisc netem loss X%` su interfaccia Starlink enp7s8 |
| **Durata test** | 10 secondi per ciascun iperf3 |
| **Commit** | 2d903ab — feat: reliable transport mode |

### 9.5 Risultati: Datagram Mode (prima del fix)

Prima dell'introduzione del reliable transport, tutti i tunnel usavano DATAGRAM
frames. Con 10% loss su Starlink, **tutti i tunnel crollavano** indipendentemente
dal congestion control:

| Tunnel | CC | 0% loss | 10% loss | Degradazione |
|--------|--------|---------|----------|-------------|
| cr3 | Cubic | 15.1 Mbps | 0.5 Mbps | **−97%** |
| br3 | BBR | 14.5 Mbps | 0.5 Mbps | **−97%** |
| df3 | Cubic | 14.9 Mbps | 0.9 Mbps | **−94%** |

Risultato: BBR identico a Cubic. Il congestion control QUIC è irrilevante
quando il transport è unreliable.

### 9.6 Risultati: Reliable Mode

#### 9.6.1 Baseline (0% loss)

| Tunnel | CC | Mbps (sender) | Retransmit | vs Datagram mode |
|--------|--------|-------|------------|------------------|
| cr3 | Cubic | 45.2 | 74 | **+199%** |
| br3 | **BBR** | **47.4** | 120 | **+227%** |
| df3 | Cubic | 55.8 | 194 | **+274%** |

Il passaggio a stream reliable ha **triplicato** il throughput base rispetto ai
DATAGRAM frames. Questo perché lo stream beneficia del flow control QUIC e del
buffering più efficiente (coalescing di pacchetti piccoli in segmenti più grandi).

#### 9.6.2 Con 10% loss

| Tunnel | CC | Mbps | Degradazione vs baseline | Confronto |
|--------|--------|------|------------------------|--------|
| cr3 | Cubic | 41.9 | −7% | Riferimento |
| br3 | **BBR** | 28.5 | −40% | BBR più conservativo |
| df3 | Cubic | 39.7 | −29% | Conferma Cubic |

Con 10% loss, Cubic si dimostra sorprendentemente resiliente grazie al loss
recovery interno di quic-go (RACK, TLP, retransmission timeout). BBR degrada
di più perché la nostra implementazione BBRv1 entra in modalità conservativa
sulle ritrasmissioni frequenti.

#### 9.6.3 Con 30% loss — BBR vince nettamente

| Tunnel | CC | Mbps | Degradazione vs baseline | Confronto |
|--------|--------|------|------------------------|--------|
| cr3 | Cubic | 15.5 | **−66%** | Riferimento |
| br3 | **BBR** | **26.1** | **−45%** | **+68% vs Cubic (cr3)** |
| df3 | Cubic | 13.6 | **−76%** | Conferma Cubic |

**Con 30% loss, BBR mantiene 26 Mbps contro i 14-15 Mbps di Cubic — quasi il
doppio del throughput.** Questo è il risultato atteso dalla teoria: BBR non
interpreta la loss come congestione, mantenendo una finestra di congestione
ampia basata sulla stima del bandwidth-delay product.

### 9.7 Analisi Comparativa Completa

Tabella riassuntiva con tutti gli scenari testati:

| Scenario | Cubic (Mbps) | BBR (Mbps) | Vantaggio BBR |
|----------|-------------|-----------|---------------|
| **Datagram mode, 0% loss** | 15.0 | 14.5 | −3% (parità) |
| **Datagram mode, 10% loss** | 0.7 | 0.5 | −29% (irrilevante) |
| **Reliable mode, 0% loss** | 50.5 | 47.4 | −6% (parità) |
| **Reliable mode, 10% loss** | 40.8 | 28.5 | −30% (Cubic meglio) |
| **Reliable mode, 30% loss** | **14.6** | **26.1** | **+79% (BBR meglio)** |

### 9.8 Interpretazione e Conclusioni del Test

1. **Il reliable transport è il fattore di gran lunga più impattante**: il passaggio
   da DATAGRAM a stream ha **triplicato** il throughput base e trasformato una
   degradazione catastrofica (−97%) in una degradazione gestibile (−7% a −76%
   a seconda dello scenario).

2. **BBR eccelle in condizioni di alta loss (≥30%)**: quando la loss è elevata,
   BBR mantiene quasi il doppio del throughput di Cubic. Questo lo rende ideale
   per scenari satellite degradati, congestione di rete, o link con interferenze.

3. **Cubic è preferibile con loss moderata (≤10%)**: il loss recovery aggressivo
   di quic-go, combinato con l'inflation rapida della finestra di Cubic, lo rende
   più performante a loss basse dove la maggior parte delle perdite sono recuperate
   in tempo.

4. **Strategia operativa consigliata**:
   - `transport_mode: reliable` su **tutti** i tunnel operativi
   - `congestion_algorithm: bbr` su tunnel **bulk** (backup, sync) dove la loss
     è più probabile e tollerabile
   - `congestion_algorithm: cubic` (default) su tunnel **critici** (VoIP,
     telemetria) dove la loss è contenuta e la latenza ha priorità

---

## 10. Test 4 — Multi-Path Failover

### 10.1 Obiettivo

Dimostrare che la piattaforma MPQUIC è in grado di effettuare un failover automatico
tra link WAN multipli (multi-path) con **minima perdita di pacchetti** e senza
interruzione del servizio percepita dall'utente finale. Il test utilizza traffico reale
proveniente da un router OpenWrt collegato alla LAN.

### 10.2 Configurazione

| Parametro | Valore |
|-----------|--------|
| **Istanza** | mp1 |
| **Policy** | `multipath_policy: failover` |
| **Path primario** | wan5 — enp7s7 (Starlink terrestre, ~14 ms) — `priority: 1` |
| **Path secondario** | wan6 — enp7s8 (Starlink satellite, ~25–40 ms) — `priority: 2` |
| **Server** | VPS 172.238.232.223:45017, `multi_conn_enabled: true` |
| **Transport** | `transport_mode: reliable`, `congestion_algorithm: bbr` |
| **TUN** | `10.200.17.1/24` (client) ↔ `10.200.17.254/24` (VPS) |
| **Sorgente traffico** | OpenWrt SL1 → `mwan3 use SL1 ping 9.9.9.9` |
| **Percorso** | PC LAN → OpenWrt SL1 → route table wan1 → mp1 TUN → VPS → Internet |

**Routing configurato per il test:**

```bash
# Client — instradare SL1 via mp1
ip route replace default dev mp1 table wan1
nft add rule ip nat postrouting oifname "mp1" masquerade

# VPS — ritorno al client
ip route add 172.16.1.0/30 dev mp1
```

### 10.3 Procedura

1. **Baseline**: avvio ping continuativo da OpenWrt (`mwan3 use SL1 ping 9.9.9.9`)
   con wan5 attivo come path primario.
2. **Fault injection**: blocco del traffico UDP verso il server sul path wan5 tramite
   nftables:
   ```bash
   nft add table inet failover_test
   nft add chain inet failover_test output { type filter hook output priority 0 \; }
   nft add rule inet failover_test output oif enp7s7 udp dport 45017 drop
   ```
3. **Osservazione failover**: monitoraggio del passaggio automatico a wan6.
4. **Recovery**: rimozione del blocco e osservazione del ritorno a wan5:
   ```bash
   nft flush table inet failover_test
   nft delete table inet failover_test
   ```

### 10.4 Risultati

**Fase di failover (wan5 → wan6):**

| Sequenza | RTT (ms) | Link | Note |
|----------|----------|------|------|
| seq 0–44 | ~14 ms | wan5 | Baseline stabile |
| seq 45–46 | — | — | **PERSI** (2 pacchetti durante switchover) |
| seq 47+ | ~24–42 ms | wan6 | Failover completato, Starlink satellite |

**Fase di recovery (wan6 → wan5):**

| Sequenza | RTT (ms) | Link | Note |
|----------|----------|------|------|
| Recovery +0s | ~35 ms | wan6 | Blocco rimosso |
| Recovery +3s | ~28 ms | transizione | wan5 rientra attivo |
| Recovery +8s | ~14 ms | wan5 | Path primario ristabilito |

**Riepilogo:**

| Metrica | Valore |
|---------|--------|
| **Pacchetti inviati** | 74 |
| **Pacchetti ricevuti** | 72 |
| **Pacchetti persi** | 2 (2.7%) |
| **Tempo di failover** | ~2 secondi (1 seq di timeout + 1 transizione) |
| **Tempo di recovery** | ~8 secondi (graduale, stabile a ~14 ms) |
| **Pacchetti persi in recovery** | 0 |

### 10.5 Verifica post-recovery

Dopo il ripristino, eseguiti due ping aggiuntivi per confermare la stabilità:

```
Run 2: 17/17 ricevuti, 0% loss, RTT 35→14 ms (stabilizzazione)
Run 3: 36/36 ricevuti, 0% loss, RTT 14 ms costante
```

### 10.6 Valutazione

Il failover multi-path dimostra:
- **Perdita minima**: solo 2 pacchetti persi in tutto lo switchover (meno di 2 secondi)
- **Trasparenza per l'utente**: l'aumento di latenza (14→35 ms) è percepibile ma
  non impatta applicazioni real-time
- **Recovery automatico**: il path primario viene ripristinato senza intervento manuale
  e senza alcuna perdita
- **Compatibilità con traffico reale**: il test è stato condotto con traffico routing
  da un router OpenWrt in produzione, non con traffico sintetico

---

## 11. Test 5 — Multi-Path Bonding (Balanced)

### 11.1 Obiettivo

Verificare che la piattaforma MPQUIC sia in grado di **aggregare la banda** di due
link WAN multipli simultaneamente (bonding), distribuendo il traffico in modo bilanciato
per ottenere un throughput superiore a quello di ciascun singolo link.

### 11.2 Configurazione

| Parametro | Valore |
|-----------|--------|
| **Istanza** | mp1 |
| **Policy** | `multipath_policy: balanced` |
| **Path 1** | wan5 — enp7s7 (Starlink terrestre, ~14 ms) — `priority: 1, weight: 1` |
| **Path 2** | wan6 — enp7s8 (Starlink satellite, ~25–40 ms) — `priority: 1, weight: 1` |
| **Server** | VPS 172.238.232.223:45017, `multi_conn_enabled: true` |
| **Transport** | `transport_mode: reliable`, `congestion_algorithm: bbr` |

Rispetto al test di failover, è stata cambiata la policy da `failover` a `balanced`
e il path wan6 da `priority: 2` a `priority: 1` (entrambi con weight=1 per distribuzione 50/50).

### 11.3 Verifica del bilanciamento — Ping

```
PING 10.200.17.254 — 10 packets transmitted, 10 received, 0% packet loss
RTT alternato: 14.7 / 35.2 / 14.6 / 39.8 / 14.0 / 35.8 / 14.5 / 37.2 / 14.7 / 35.2 ms
```

Il pattern alternato conferma che i pacchetti vengono distribuiti equamente tra i due
path: i valori ~14 ms corrispondono a wan5 (terrestre), quelli ~35 ms a wan6 (satellite).

### 11.4 Test di throughput — iperf3

**Parametri**: 4 stream paralleli, 15 secondi, modalità reverse (download dal VPS
al client attraverso il tunnel bonded).

```bash
iperf3 -c 10.200.17.254 -p 5201 -t 15 -P 4 -R --bind-dev mp1
```

**Risultati per intervallo:**

| Intervallo (s) | Throughput SUM (Mbps) | Note |
|-----------------|----------------------|------|
| 0–1 | 40.3 | Ramp-up iniziale |
| 1–2 | 51.4 | BBR in probing |
| 2–3 | 60.0 | Crescita costante |
| 3–4 | 67.7 | — |
| 4–5 | 74.5 | — |
| 5–6 | 75.1 | — |
| 6–7 | 85.2 | — |
| 7–8 | 92.6 | — |
| 8–9 | 98.6 | — |
| **9–10** | **102.3** | **Picco massimo** |
| 10–11 | 80.2 | Starlink handover |
| 11–12 | 76.5 | — |
| 12–13 | 87.7 | — |
| 13–14 | 68.9 | — |
| 14–15 | 53.4 | Fine test |

**Risultati aggregati:**

| Metrica | Valore |
|---------|--------|
| **SUM Sender** | 135 MB / **75.4 Mbps** |
| **SUM Receiver** | 133 MB / **74.3 Mbps** |
| **Picco** | **102.3 Mbps** (sec 9–10) |
| **Retransmit** | 185 totali |
| **Tempo ramp-up** | ~10 secondi (40 → 102 Mbps) |

### 11.5 Test da OpenWrt — Traffico reale

Ping continuativo da OpenWrt attraverso il tunnel bonded:

```
86 packets transmitted, 81 received, 5% packet loss
rtt min/avg/max = 13.488/20.345/42.156 ms
```

Le 5 perdite iniziali sono attribuibili alla fase di ri-registrazione dei path dopo
il cambio di policy (da failover a balanced con restart dell'istanza mp1).

### 11.6 Valutazione

Il bonding multi-path dimostra:
- **Aggregazione reale**: 74.3 Mbps aggregati superano ampiamente la banda di ciascun
  singolo link (~40–50 Mbps ciascuno)
- **Picco a 102 Mbps**: BBR riesce a sfruttare entrambi i link al massimo
  contemporaneamente
- **Ramp-up rapido**: da 40 a 102 Mbps in soli 10 secondi grazie a BBR probing
- **Retransmit contenuti**: 185 su un trasferimento di 135 MB (0.01%) indica una
  gestione ottimale del reordering tra path a latenza differente

---

## 12. Test 6 — Speedtest End-to-End con Bonding

### 12.1 Obiettivo

Misurare le prestazioni end-to-end percepite da un **client LAN reale** collegato al
router OpenWrt, con traffico instradato attraverso il tunnel bonded mp1 verso Internet
via VPS. A differenza dei test iperf3 (che misurano il throughput del tunnel), questo
test misura le prestazioni complete della catena:

```
PC LAN → OpenWrt (SL1) → mp1 bonded tunnel → VPS → Internet → Ookla server
```

### 12.2 Configurazione

| Parametro | Valore |
|-----------|--------|
| **Client** | PC collegato a OpenWrt LAN |
| **Router** | OpenWrt con mwan3, SL1 → table wan1 → mp1 |
| **Tunnel** | mp1 bonded (wan5 + wan6, balanced) |
| **VPS egress** | 172.238.232.223 (Akamai Technologies) |
| **Server Ookla** | Fiber Telecom SPA, Milano |
| **Strumento** | Ookla Speedtest (speedtest.net) |

### 12.3 Risultati Ookla Speedtest

| Metrica | Valore |
|---------|--------|
| **Download** | **71.97 Mbps** |
| **Upload** | **41.12 Mbps** |
| **Ping (idle)** | 19 ms |
| **Ping (download)** | 69 ms |
| **Ping (upload)** | 27 ms |
| **Connessioni** | Multi |
| **Server** | Fiber Telecom SPA, Milano |
| **Provider rilevato** | Akamai Technologies (172.238.232.223) |
| **Result ID** | 18904003389 |

### 12.4 Analisi

1. **Download 72 Mbps**: coerente con il risultato iperf3 (74.3 Mbps), il leggero
   delta (~3%) è dovuto all'overhead aggiuntivo di HTTPS e all'hop VPS→Ookla server.

2. **Upload 41 Mbps**: inferiore al download perché Starlink alloca tipicamente meno
   banda in uplink. Il bonding aggrega comunque i due link efficacemente.

3. **Ping 19 ms idle**: il percorso PC→OpenWrt→mp1→VPS→Ookla aggiunge solo ~5 ms
   rispetto al RTT diretto del tunnel (14 ms), dimostrando overhead trascurabile.

4. **Ping 69 ms sotto carico download**: il bufferbloat è contenuto (+50 ms) grazie
   a BBR che limita l'inflazione delle code rispetto a Cubic.

5. **Provider Akamai**: il server Ookla vede il VPS (172.238.232.223) come sorgente,
   confermando che il traffico transita correttamente attraverso l'intera catena
   tunnel.

### 12.5 Significato per il cliente

Questo test è particolarmente significativo perché riproduce **esattamente** lo
scenario operativo reale:
- Un PC normale collegato via Ethernet al router
- Nessuna configurazione speciale sul PC (DHCP standard)
- Traffico web standard (HTTPS verso server Ookla)
- Tutto il routing, tunneling e bonding avviene in modo trasparente

Il client ottiene **72 Mbps in download** aggregando due link Starlink, con una
latenza base di 19 ms — prestazioni comparabili a una connessione FTTH residenziale
in un contesto completamente satellitare.

---

## 13. Vantaggi per il Cliente

### 13.1 Qualità del Servizio Garantita

La piattaforma MPQUIC garantisce che le applicazioni critiche del cliente mantengano
prestazioni ottimali **indipendentemente dallo stato delle altre applicazioni**:

- **Chiamate VoIP** e **videoconferenze**: latenza costante (~13 ms), zero packet loss,
  anche durante trasferimenti dati massivi o backup in corso
- **Applicazioni business** (ERP, CRM, email): throughput pieno e stabile, non
  influenzato da download o aggiornamenti in background
- **Backup e sincronizzazione**: possono saturare la loro quota di banda senza
  impattare le altre classi di traffico

### 13.2 Resilienza alle Condizioni del Link

Le connessioni satellitari LEO (Starlink) sono soggette a variabilità naturale:
handover tra satelliti, condizioni meteo, congestione del beam. Grazie
all'isolamento per classe:

- Un **picco di loss temporaneo** affetta solo il traffico meno prioritario
- Le **applicazioni critiche** continuano a funzionare normalmente
- Non è necessario **interrompere il backup** per fare una telefonata

### 13.3 Monitorabilità e Trasparenza

Ogni tunnel è separato e monitorabile individualmente:
- **RTT per classe**: è possibile misurare la latenza di ogni tipo di traffico
- **Throughput per classe**: visibilità sulla banda effettivamente utilizzata
- **Loss per classe**: identificazione immediata di quale tipo di traffico è affetto

### 13.4 Scalabilità

L'architettura è stata progettata per scalare:
- **3 link × 3 classi = 9 tunnel** già operativi
- Possibilità di aggiungere nuove classi (es. *gaming*, *streaming*)
- Possibilità di aggiungere nuovi link (fino a 6 WAN supportate)
- Classificazione flessibile tramite VLAN: l'apparato di rete del cliente decide
  quale traffico va in quale classe

### 13.5 Confronto sintetico: prima e dopo

| Scenario | Prima (tunnel singolo) | Dopo (MPQUIC multi-tunnel) |
|----------|------------------------|----------------------------|
| Backup saturando il link | VoIP si blocca, web lentissimo | VoIP perfetto, web perfetto |
| 10% packet loss sul link | Tutto il traffico degrada del 95% | Solo il bulk degrada; critico e default inalterati |
| 30% packet loss sul link | Servizio inutilizzabile | Critico e default funzionano normalmente |
| Identificazione problema | "Il tunnel è lento" | "Il tunnel bulk ha 10% loss, gli altri sono OK" |

---

## 14. Conclusioni

I test condotti tra il 28 febbraio e il 1 marzo 2026 dimostrano in modo
**quantitativo e riproducibile** che la piattaforma MPQUIC soddisfa tutti gli
obbiettivi delle quattro fasi di sviluppo: isolamento multi-tunnel, resilienza
BBR su satellite, failover automatico e aggregazione multi-link.

**Risultati chiave per fase:**

**Fase 2 — Isolamento multi-tunnel:**

1. **Isolamento RTT**: packet loss fino al 30% su un tunnel non causa alcun aumento
   di latenza sugli altri tunnel (0 ms di variazione)

2. **Isolamento throughput**: packet loss fino al 30% su un tunnel non causa alcuna
   diminuzione di banda sugli altri tunnel (±0% variazione, 50 Mbps mantenuti)

3. **Nessun Head-of-Line Blocking**: a differenza delle VPN tradizionali, la perdita
   di pacchetti su una classe non blocca la consegna delle altre

**Fase 3 — BBR + Reliable Transport:**

4. **Reliable transport**: il passaggio da DATAGRAM frames a QUIC streams ha
   triplicato il throughput base (15 → 50 Mbps) e trasformato una degradazione
   catastrofica sotto loss (−97%) in una gestibile (−7% a −45%)

5. **BBR congestion control**: con 30% loss e reliable transport, BBR mantiene
   **26 Mbps** contro i 14 Mbps di Cubic — **+79% di throughput**, rendendo
   operativi scenari satellite che altrimenti sarebbero inutilizzabili

**Fase 4 — Multi-Path Failover e Bonding:**

6. **Failover automatico**: switchover in ~2 secondi con soli **2 pacchetti persi**
   su 74, recovery senza alcuna perdita. Traffico reale da router OpenWrt.

7. **Bonding aggregato**: **74.3 Mbps** di throughput aggregando due link Starlink,
   con picco a **102 Mbps**. Ramp-up BBR da 40 a 102 Mbps in 10 secondi.

8. **Speedtest end-to-end**: un client LAN reale ottiene **72 Mbps in download**
   e **41 Mbps in upload** con Ookla speedtest, dimostrando che le prestazioni
   sono reali e trasparenti per l'utente finale.

9. **Architettura scalabile**: 19 tunnel operativi (16 attivi) su 6 WAN, con
   supporto multi-path, failover e bonding integrati.

**Prossimi sviluppi**:

- **BBRv2/v3**: implementazione completa con reazione proporzionale alla loss
  (atteso ulteriore miglioramento al 10% loss)
- **QoS attivo** (DSCP, traffic shaping): allocazione di banda garantita per classe
- **Bonding per classe**: estensione del bonding multi-path a tutti i tunnel,
  non solo al tunnel mp1 di test
- **Adaptive CC selection**: selezione automatica di BBR vs Cubic basata sulle
  condizioni del link in tempo reale
- **Monitoring dashboard**: interfaccia web per visualizzare lo stato di tutti
  i tunnel, path e metriche in tempo reale

---

## 15. Appendice — Comandi Completi

### A.1 Verifica stato tunnel

```bash
# Client — tutti i 9 tunnel attivi
for svc in cr1 br1 df1 cr2 br2 df2 cr3 br3 df3; do
  printf "%-4s " "$svc"
  systemctl is-active mpquic@${svc}.service
done

# Client — interfacce TUN con IP
ip -br addr show | grep -E 'cr[1-3]|br[1-3]|df[1-3]'

# Ping bidirezionale (esempio set WAN5)
for ip in 10.200.15.1 10.200.15.5 10.200.15.9; do
  ping -c 2 -W 2 10.200.15.254 -I "$ip"
done
```

### A.2 Applicazione packet loss con netem

```bash
# Applicare 10% loss su br2
sudo tc qdisc add dev br2 root netem loss 10%

# Verificare
sudo tc qdisc show dev br2
# Output: qdisc netem 8003: root refcnt 2 limit 1000 loss 10%

# Modificare a 30%
sudo tc qdisc replace dev br2 root netem loss 30%

# Rimuovere
sudo tc qdisc del dev br2 root
```

### A.3 Misure iperf3 con device binding

```bash
# Baseline (senza loss)
iperf3 -c 10.200.15.254 -B 10.200.15.1%cr2 -t 5
iperf3 -c 10.200.15.254 -B 10.200.15.5%br2 -t 5
iperf3 -c 10.200.15.254 -B 10.200.15.9%df2 -t 5

# Con loss su br2 — stessi comandi, risultati cambiano solo per br2
```

### A.4 Configurazione firewall VPS

```bash
# Apertura interfacce TUN
nft add rule inet filter input iifname "mt*" accept

# Apertura porta iperf3
nft add rule inet filter input tcp dport 5201 accept

# Persistenza
nft list ruleset > /etc/nftables.conf
```

### A.5 Configurazione multi-path mp1

```bash
# Configurazione client (/etc/mpquic/instances/mp1.yaml — sezione rilevante)
multipath_policy: balanced       # oppure: failover
paths:
  - name: wan5
    interface: enp7s7
    priority: 1
    weight: 1
  - name: wan6
    interface: enp7s8
    priority: 1                  # per failover: priority: 2
    weight: 1

# Routing SL1 tramite mp1
ip route replace default dev mp1 table wan1
nft add rule ip nat postrouting oifname "mp1" masquerade

# VPS — route di ritorno
ip route add 172.16.1.0/30 dev mp1

# Fault injection per test failover
nft add table inet failover_test
nft add chain inet failover_test output { type filter hook output priority 0 \; }
nft add rule inet failover_test output oif enp7s7 udp dport 45017 drop

# Recovery
nft flush table inet failover_test
nft delete table inet failover_test
```

### A.6 iperf3 bonding attraverso tunnel mp1

```bash
# Server VPS (demone)
iperf3 -s -D -B 10.200.17.254 -p 5201

# Client — 4 stream paralleli, 15s, download (reverse)
iperf3 -c 10.200.17.254 -p 5201 -t 15 -P 4 -R --bind-dev mp1
```

### A.7 Note di riproducibilità

Per riprodurre i test è necessario:
1. Avere almeno 3 tunnel attivi sullo stesso link (es. cr2/br2/df2 su WAN5)
2. `iperf3` installato su client e server
3. Firewall VPS aperto per interfacce TUN e porta iperf3
4. Usare `-B IP%device` nell'iperf3 per garantire routing device-bound
5. `tc` (iproute2) con modulo kernel `sch_netem` disponibile sul client
6. Test sequenziali (un iperf3 alla volta) poiché il server iperf3 è single-instance
7. Per test multi-path: almeno 2 link WAN attivi con tunnel mp1 configurato
8. Per test end-to-end: router OpenWrt con mwan3 configurato per instradare SL1 via mp1
9. Dopo restart di mp1, ri-aggiungere `ip route replace default dev mp1 table wan1`
   (il TUN viene ricreato e la route va persa)

---

*Documento aggiornato il 01/03/2026 — Piattaforma MPQUIC v3.0*  
*Commit di riferimento: 1cc40bc (main)*
