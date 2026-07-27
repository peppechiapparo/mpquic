# Report collaudo banco TBOX-EVO dual-WAN, 27 luglio 2026

> **ERRATUM (27 luglio, sera)** — Due vizi di metodo scoperti a valle, che ridimensionano parte delle conclusioni:
> 1. **I numeri iperf3 NON misurano i tunnel STRIPES.** Il target era l'IP pubblico del VPS (104.105.82.146), che nelle tabelle wanN è servito dalla **host route /32 diretta sul canale fisico** (serve ai socket del tunnel): i 250-303 Mbit sono Starlink nudo verso il VPS, non trasporto mpquic. Il test corretto punta all'IP di tunnel del VPS (10.200.N.254) o verifica l'IP pubblico di uscita.
> 2. **Il bypass TS-017 poteva attivarsi in silenzio**: lo script aveva `MPQUIC_BYPASS_WANS` con default `enp7s8`, quindi ogni esecuzione fuori dalla unit metteva il gateway fisico in wan6. Meccanismo RIMOSSO del tutto (commit `93a8e29`): a tunnel giù ora solo blackhole, il fallback fisico passa a mwan3 su VLAN dedicate.
>
> Restano valide le conclusioni su: stabilità delle rule (networkd-owned), zero EPERM/restart, fail-closed, auto-riparazione dal guasto, e le misure ping/peer-ping (quelle attraversano davvero i tunnel). Il throughput STRIPES reale è DA RIMISURARE col metodo corretto, sulla versione mpquic che verrà validata dal rollback (vedi TS-023 quando aperta).

Soak test di 2h07 (14:38-16:45) sul banco che riproduce IBLEA-M, a valle dei fix TS-021/TS-022. Setup: pclab (192.168.222.2, LAN CORPORATE) → OpenWrt VM 100 → client mpquic VM 200 → tunnel mpq5 (LTE), mpq6 (Starlink), mp1 (STRIPES) → VPS lab 104.105.82.146 → internet. Carico: k6 profilo `office`, 50 VU, think time 1-5s, 2h, via docker su pclab. Orchestrazione automatica: burst iperf3 a T+30 e T+90, guasto provocato a T+60, monitor continui su ping utente, peer ping in-tunnel e contatori di regressione.

## Verdetto in sintesi

Lo stack mpquic e il routing del banco escono **promossi**: due ore di carico senza un solo cedimento dei meccanismi sistemati oggi (rule, blackhole, riconciliazione, watchdog), throughput pieno anche sotto carico, auto-riparazione dal guasto in ~15 secondi. Il degrado osservato lato utente sotto i 50 VU (fallimenti k6 sulle pagine pesanti, RTT gonfiato) ha una firma che punta fuori dal trasporto; sotto la voce "caso k6" i dettagli e le due prove proposte per chiudere l'attribuzione.

## Baseline (fase A, 14:30, senza carico)

| Percorso | Loss | RTT medio |
|---|---|---|
| STARLINK via mpq6 (`mwan3 use`) | 0% | 25 ms |
| LTE via mpq5 | 0% | 74 ms |
| BOND1 via mp1 | 0% | 56 ms |
| Pavimento fisico enp7s8 (Starlink diretto) | 0% | 25 ms |
| Pavimento fisico enp7s7 (LTE diretto) | 0% | 47 ms |
| Peer in-tunnel mpq5 / mpq6 / mp1 | 0% / 0% / 0% | 69 / 31 / 49 ms |

Overhead del tunnel sul percorso Starlink: praticamente nullo a vuoto.

## Stabilità sotto carico (fase B, 2h07 di soak)

I quattro rilevatori di regressione, campionati ogni 60 secondi per tutta la durata:

| Rilevatore | Esito |
|---|---|
| ip rule 1001-1006 | **6/6 in ogni campione**, mai un buco |
| Restart automatici mpq5 / mpq6 / mp1 | **0 / 0 / 0** |
| EPERM sui socket (journal, 130 min) | **0** |
| Riconfigurazioni networkd | 2 (entrambe = ricreazione TUN nel guasto provocato, attese) |
| Peer ping in-tunnel (456 check da 5 ping) | **456 su 456 a 0% loss** |

## Throughput (fase C, burst iperf3 verso il VPS, CON i 50 VU k6 attivi)

| Misura | T+30 (15:08) | T+90 (16:08) |
|---|---|---|
| Aggregato `-P30 -R` | **303 Mbit/s** | 252 Mbit/s |
| Single-stream `-P1 -R` | 51 Mbit/s | 77 Mbit/s |
| Upload `-P1` | 21 Mbit/s | 16 Mbit/s |

Il single-stream in download conferma che il collasso TS-013 non esiste più; l'upload resta sul limite noto del trasporto in salita (tema del piano trasporto, non di questo collaudo).

## Guasto provocato (fase D, 15:38, `mpquic@6` fermato sotto carico)

Esito diverso e migliore del copione: l'outage visto dall'utente è durato **10-20 secondi** (un solo bucket da 10s con 1 risposta su 10, i vicini a 7-8), perché il **tunnel-watchdog ha resuscitato l'istanza entro il suo ciclo di 15s**, prima ancora che mwan3 avesse il tempo di dichiarare STARLINK offline e failoverare. Al rientro: peer ping 5/5, `route get` di nuovo dentro mpq6, mwan3 mai flappato. Il sistema si auto-ripara più in fretta della catena di failover che doveva coprirlo.

Nota onesta: proprio per questo il failover mwan3 STARLINK→LTE sotto carico NON è stato esercitato in questo run (il guasto va tenuto aperto più del ciclo del watchdog, o simulato a livello fisico). Il fail-closed (blackhole, zero bypass) era comunque già stato validato in mattinata a watchdog fermo.

## Il caso k6: 62% di richieste fallite, ma la firma non è del trasporto

Numeri del summary (29.822 richieste, 25 GB ricevuti, 3.4 MB/s sostenuti):

| Indicatore | Valore |
|---|---|
| Richieste "light" riuscite | **99%** (20.924 su 20.928) |
| Richieste "page" (pagine intere) riuscite | **8%** (761 su 8.894), fallimenti quasi tutti timeout a 30s |
| Durata delle richieste riuscite | mediana 239 ms, p90 1.54 s |
| Ping utente 9.9.9.9 durante il soak | ~9% loss distribuita, RTT medio 120-160 ms |
| Ping utente a k6 spento (16:40+) | RTT 28 ms, loss ~0 |

Lettura: se il tunnel perdesse il 62% del traffico non reggerebbe 25 GB in 2 ore, né il 99% sulle light, né i peer ping a 0% per 456 check. Il pattern (solo le pagine pesanti muoiono, le riuscite sono veloci) è la firma classica di **bot-mitigation o rate-limiting dei siti target** verso l'IP del VPS, bombardato da 50 utenti sintetici per 2 ore. L'RTT gonfiato e il ~9% di ICMP loss durante il soak sono coerenti con code piene sotto carico (bufferbloat) e con la tratta internet a valle del VPS, non con perdite del tunnel (i peer ping, che si fermano al VPS, sono rimasti a 0%).

Due prove per chiudere l'attribuzione, entrambe economiche:

1. Rerun k6 breve (15 min) con **10 VU**: se "page ok" risale, è rate-limiting dei target.
2. Stesso profilo contro una **pagina servita dal VPS stesso**: isola il trasporto al 100%, zero variabili esterne.

Se dopo queste il colpevole risultasse il bufferbloat, il rimedio sta nel qdisc (cake/fq_codel sul TUN o sul fisico), non nel data plane.

## Prossimi passi proposti

1. Le due prove di attribuzione k6 (sopra).
2. Test scambio cavi fisico per il wan-watchdog sotto carico (serve una mano in sala).
3. Guasto lungo (watchdog tunnel sospeso per la durata del test) per esercitare davvero il failover mwan3 sotto carico.
4. Freeze della baseline (binario `4ac2bed3`, script e unit già a md5 identico col repo) e avvio remediation IBLEA-M con la todolist di [AUDIT_IBLEAM_2026-07-27.md](AUDIT_IBLEAM_2026-07-27.md).

Log grezzi del run in `scratchpad/soak/` della sessione (ping con timestamp, contatori per minuto, summary k6, iperf, log del guasto).
