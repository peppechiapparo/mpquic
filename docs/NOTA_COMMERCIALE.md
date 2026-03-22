# Nota Tecnica — Architettura MPQUIC per Connettività Resiliente

**Destinatari**: Referenti commerciali e cliente finale  
**Data**: 10 marzo 2026  
**Versione**: 1.0

---

## Obiettivo della Soluzione

La piattaforma MPQUIC fornisce un **collegamento Internet aggregato, cifrato e
resiliente** combinando due connettività indipendenti — **satellitare (Starlink)** e
**cellulare (LTE)** — in un unico tunnel logico ad alte prestazioni.

Il sistema garantisce:

- **Aggregazione di banda**: la capacità delle due connettività viene sommata,
  raggiungendo throughput superiori a quelli ottenibili da ciascun singolo link
- **Continuità di servizio**: se una delle due connettività si degrada o cade,
  il traffico viene automaticamente dirottato sull'altra senza interruzione
- **Sicurezza**: tutto il traffico è cifrato end-to-end con crittografia
  di livello militare (AES-256-GCM)
- **Trasparenza**: i dispositivi collegati alla rete locale non richiedono
  alcuna configurazione — la connettività aggregata è disponibile come
  una normale connessione Internet

---

## Architettura del Sistema

Il sistema è composto da due elementi: un **sito remoto** (dove si trova
l'utente) e un **punto di concentrazione cloud** (che fornisce l'uscita Internet).

```
                           SITO REMOTO (TBOX)
  ┌──────────────────────────────────────────────────────────────────┐
  │                                                                  │
  │   ┌──────────────┐       ┌──────────────────────────────────┐    │
  │   │              │       │       Macchina Virtuale          │    │
  │   │   Router     │       │     (Orchestratore Tunnel)       │    │
  │   │              │       │                                  │    │
  │   │  • Gestione  │       │  • Creazione tunnel MPQUIC       │    │
  │   │    failover  │◄─────►│  • Aggregazione multi-path       │    │
  │   │  • Routing   │       │  • Cifratura AES-256-GCM         │    │
  │   │    traffico  │       │  • Correzione errori automatica  │    │
  │   │              │       │                                  │    │
  │   └──────┬───────┘       └────────┬──────────────┬──────────┘    │
  │          │                        │              │               │
  │          │ LAN                    │              │               │
  │          │                   eth 8│         eth 7│               │
  └──────────┼────────────────────────┼──────────────┼───────────────┘
             │                        │              │
             │                        │              │
     ┌───────┴───────┐       ┌───────┴──────┐ ┌────┴──────────┐
     │  Dispositivi  │       │   Modem      │ │    Modem      │
     │  di rete LAN  │       │  Starlink    │ │     LTE       │
     │  (PC, IP cam, │       │              │ │               │
     │   VoIP, ...)  │       │  (eth 8)     │ │   (eth 7)     │
     └───────────────┘       └───────┬──────┘ └────┬──────────┘
                                     │              │
                                     │   Internet   │
                                     │              │
                              ═══════╧══════════════╧═══════
                                     │              │
                             ┌───────┴──────────────┴───────┐
                             │                              │
                             │    Provider Cloud Pubblico   │
                             │       (Server VPS)           │
                             │                              │
                             │  • Terminazione tunnel       │
                             │  • Uscita Internet unica     │
                             │  • IP pubblico statico       │
                             │                              │
                             └──────────────────────────────┘
```

---

## Componenti

### Sito Remoto (TBOX)

La TBOX è un apparato compatto installato presso la sede del cliente.
Al suo interno operano due componenti software:

| Componente | Funzione |
|------------|----------|
| **Router** | Gestisce il traffico della rete locale, decide quale connettività utilizzare e garantisce il failover automatico |
| **Orchestratore Tunnel (VM)** | Crea e mantiene i tunnel cifrati MPQUIC verso il cloud, aggrega la banda delle due connettività e applica la correzione degli errori |

Le due connettività fisiche sono:

| Porta | Connettività | Caratteristiche |
|-------|-------------|-----------------|
| **eth 8** | Starlink (satellitare) | Alta banda (fino a 200+ Mbps), latenza variabile (~30-50 ms) |
| **eth 7** | LTE (cellulare) | Banda moderata (30-80 Mbps), latenza stabile (~20-40 ms) |

### Server Cloud (VPS)

Il server è ospitato presso un provider cloud pubblico e svolge il ruolo
di **punto di concentrazione**: riceve i tunnel cifrati da entrambe le
connettività, ricompone il traffico e lo inoltra verso Internet attraverso
un **indirizzo IP pubblico statico**.

---

## Come Funziona

Il flusso del traffico si articola in quattro fasi:

**1. Invio (LAN → Internet)**

I dispositivi della rete locale del cliente (PC, telecamere, telefoni VoIP)
inviano traffico normalmente. Il router lo inoltra all'orchestratore, che
lo suddivide tra i due tunnel — Starlink e LTE — in base alla disponibilità
e alle prestazioni di ciascun link.

**2. Trasporto cifrato**

Ogni pacchetto viene cifrato e inviato attraverso i tunnel MPQUIC.
Il protocollo utilizza meccanismi di **correzione degli errori** che
compensano automaticamente le perdite di pacchetti tipiche delle
connessioni satellitari, senza necessità di ritrasmissioni.

**3. Ricomposizione**

Il server cloud riceve i pacchetti da entrambi i tunnel, li ricompone
nell'ordine corretto e li inoltra verso la destinazione su Internet.

**4. Ritorno (Internet → LAN)**

Le risposte seguono il percorso inverso: il server le cifra e le
distribuisce sui tunnel attivi, l'orchestratore le decifra e le consegna
ai dispositivi tramite il router.

---

## Resilienza e Failover

Il sistema gestisce automaticamente tre scenari di degrado:

| Scenario | Comportamento |
|----------|--------------|
| **Starlink si degrada** (perdita pacchetti, riduzione banda) | Il traffico viene progressivamente spostato sul link LTE; la correzione errori compensa le perdite residue |
| **Starlink cade completamente** | Il router attiva il failover su LTE in pochi secondi; il tunnel LTE continua a operare senza interruzione |
| **LTE cade** | Il tunnel Starlink gestisce tutto il traffico; le prestazioni dipendono dalla sola banda satellitare |

In tutti i casi il passaggio è **automatico e trasparente** per i
dispositivi collegati: non si verificano disconnessioni delle sessioni
applicative (navigazione web, videoconferenza, VoIP).

---

## Sicurezza

Tutto il traffico tra il sito remoto e il server cloud è protetto da:

- **Cifratura AES-256-GCM** su ogni singolo pacchetto (standard utilizzato
  in ambito governativo e bancario)
- **Negoziazione chiavi TLS 1.3**: le chiavi crittografiche vengono
  rinnovate automaticamente ad ogni connessione
- **Nessun dato in chiaro** transita sulle connettività Starlink o LTE

---

## Prestazioni Attese

Sulla base dei test condotti in laboratorio con configurazione analoga
(dual Starlink + link aggiuntivo):

| Metrica | Valore tipico |
|---------|---------------|
| **Throughput aggregato** | 350 – 550 Mbps (download, picco 548 misurato) |
| **Latenza (RTT)** | 15 – 35 ms |
| **Tempo di failover** | < 5 secondi |
| **Tempo di ripristino dopo riavvio** | < 3 secondi |
| **Overhead cifratura** | < 3% della banda |

Le prestazioni effettive dipendono dalla copertura Starlink e LTE
nel sito di installazione.

---

## Riepilogo

```
  Dispositivi LAN                      Internet
       │                                  ▲
       ▼                                  │
  ┌─────────┐    ┌──────────────┐    ┌────┴─────┐
  │ Router  │───►│ Orchestratore│═══►│  Server  │
  │ (TBOX)  │    │   (TBOX)     │    │  Cloud   │
  └─────────┘    └──┬────────┬──┘    └──────────┘
                    │        │
              ┌─────┘        └──────┐
              ▼                     ▼
        ┌──────────┐         ┌──────────┐
        │ Starlink │         │   LTE    │
        │ (eth 8)  │         │ (eth 7)  │
        └──────────┘         └──────────┘

        ════════ Tunnel MPQUIC cifrato ════════
```

La soluzione MPQUIC trasforma due connettività indipendenti — ciascuna con
i propri limiti di banda, latenza e affidabilità — in un **unico collegamento
Internet ad alte prestazioni**, cifrato, resiliente e completamente trasparente
per l'utente finale.
