# Analisi comparata degli interventi sul banco TBOX-EVO (27-28 luglio)

> **Erratum del 2026-07-29 sullo strato 3.** L'A/B rifatto in produzione su IBLEA-M dice l'opposto del banco: con `stripe_flow_affinity` attiva lato server il download single-stream nel tunnel stava a 0.3-0.7 Mbit contro 73.5 Mbit del fisico misurato nello stesso minuto; spenta la flag, 21-39 Mbit. Su IBLEA la flag è ora **false** e il rollback di config previsto qui sotto è stato eseguito. Il fix vale sul banco e non sul CGNAT di bordo, quindi è una scelta per-installazione, non un default. Dettagli in [TS-027](TROUBLESHOOTING_HISTORY.md).

Scopo: metterti in condizione di **misurare di persona** ogni risultato dichiarato e decidere, strato per strato, se tenere o tornare indietro. Ogni claim ha la sua evidenza, le condizioni di misura (contano: il banco vive su una Starlink condivisa che di notte faceva 294 Mbit e stamattina ~1) e il comando di rollback.

Avvertenza operativa: **al momento della scrittura il fisico Starlink del banco è degradato (~1 Mbit TCP, ICMP ingannevolmente a 0%)**. Qualunque misura fatta prima della ripresa non vale nulla: il guardiano attivo avvisa quando il pavimento risale sopra i 50 Mbit. Regola numero uno imparata (due volte) in queste 24 ore: prima di ogni sessione di misure, misurare il pavimento fisico nello stesso minuto.

## Gli strati toccati, dal più consolidato al più recente

### Strato 1: routing e fail-closed (TS-021/TS-022, validato più a lungo)

| Cosa | Prima | Dopo |
|---|---|---|
| ip rule 1001-1006 | sparivano in 2-3 min (sweep networkd) → bypass silenzioso, WAN tutte "Online" finte | networkd-owned (`proto static`), stabili per ore sotto carico e guasti |
| Tunnel giù | traffico scavalcava sul fisico (bypass TS-017, anche da run manuali dello script) | blackhole sempre (pavimenti metric 9999 su wanN, phy, bd1); bypass RIMOSSO dal codice |
| Fallback fisico | implicito e invisibile | esplicito: VLAN 95/96 + `LTE_PHY`/`STARLINK_PHY` come riserve mwan3 |
| Riconciliazione | solo eventi (che si perdevano) | eventi + timer 30s idempotente |
| LAN CORPORATE | bloccata (chilli su eth0) / DNS mai permesso | in forwarding nei tunnel + regola DNS |

Evidenze: soak 2h07 + validazione 30 min + guasti provocati = rule 6+2 integre in ogni campione, 0 restart spurii, 0 EPERM, guasto auto-riparato in 10-20s (watchdog) e failover esplicito disponibile. Questo strato ha girato più a lungo di tutti ed è quello che considero acquisito.

Rollback: backup `.bak-20260727`/`.bak-ts018-20260727` di ogni file networkd sul client; snapshot Proxmox `pre-lab-dualwan-20260727` (VM 200 e VM 100); su OpenWrt `/root/etc-config-bak-20260727.tar.gz`.

### Strato 2: versione software (rollback pre-CAL, TS-023)

| Cosa | Prima | Dopo |
|---|---|---|
| Binario | CAL/ML-KEM (`4ac2bed3`), comportamento anomalo, mpq5 spariva | baseline pre-CAL `3c03829`, config mp1 dual-path d'epoca, stabile |
| Salvataggio | — | branch `debug/anomalia-mpquic-20260727` (snapshot integrale incluso WIP crypto) |

Evidenza chiave: sul binario CAL le istanze QUIC flappavano; sulla baseline il trasporto è tornato misurabile e stabile. Il debug della versione CAL non è ancora iniziato: è LA decisione di prodotto aperta.

Rollback: `.bak-pre-rollback-20260727` su client e VPS (= binario CAL).

### Strato 3: flow-affinity (TS-024, il fix della notte)

Causa radice provata (Pollicino + metriche): perdita reale 0.004% ma **~100% dei pacchetti fuori ordine** per lo striping round-robin sulle 12 pipe → TCP collassa sul singolo flusso, ARQ tempesta (217 NACK/s, 76% del ricevuto = ritrasmissioni), Grafana segna loss 100% finta.

Fix: hash del 5-tuple → ogni flusso su una pipe. Wire invariato, flag `stripe_flow_affinity` default off. Forma finale **asimmetrica** dopo l'A/B: attiva SOLO lato server (downstream), perché lato client uccideva l'upload (flusso inchiodato su pipe con binding CGNAT degradato = danno al 100%, dove il RR ne assorbe 1/12).

| Misura (pclab, inoltrato, notte, stesso link) | Round-robin | Affinity lato server |
|---|---|---|
| Single-stream download (P1 -R) | 0.3 - 21 Mbit, varianza selvaggia | **43 - 85 Mbit** |
| Single-stream upload (P1) | 13 - 47 Mbit | 13 - 21 Mbit (RR, invariato) |
| Aggregato (P30 -R) | 80 - 122 Mbit | **153 - 187 Mbit** |
| k6 50 utenti | pagine 90% (2h) | pagine 83% (30 min, sotto saturazione con probe in mezzo: nel rumore) |

Nota di onestà: sotto saturazione piena da 50 utenti comanda la capacità del link, non il riordino: lì l'affinity non fa miracoli (e non deve). Il suo valore è il singolo flusso: la navigazione reale.

Rollback: `.bak-pre-affinity` su entrambi i lati (= baseline liscia) oppure solo `stripe_flow_affinity: false` nel `mp1.yaml` del VPS + restart (rollback di config, 10 secondi, reversibile).

## Cosa NON è risolto (lista onesta)

1. **ARQ range-retransmit**: pochi NACK scatenano decine di migliaia di ritrasmissioni (67k per 78). Oggi la banda lo assorbe, ma è spreco e sotto carico morde. Prossimo tuning di trasporto.
2. **Upload single-stream**: sano ma modesto (13-21). Limite storico, non regressione.
3. **Modem LTE senza lease** da ieri ~22:00 (`enp7s7`): serve check fisico; il dual-path (la config validata "vera") non è rivalidabile finché non torna.
4. **Versione CAL**: da debuggare sul branch; l'affinity andrà riconciliata (cherry-pick del commit `4c469dc` o riscrittura pulita sul CAL risanato).
5. **Sensibilità del banco al link condiviso**: la Starlink CGNAT del lab crolla (stanotte 294 → stamattina 1 Mbit): qualunque campagna di misure seria deve loggare il pavimento fisico a ogni sessione.

## Protocollo "misuro io" (quando il guardiano dà link > 50 Mbit)

Da eseguire nell'ordine; se il punto 0 fallisce, fermarsi lì.

```bash
# 0. PAVIMENTO FISICO (dal client, ~30s) - se sotto 50 Mbit, non proseguire
ssh mpquic   # o: ssh satcom@10.10.11.100
iperf3 -c 104.105.82.146 -p 5201 -R -t 10 -P 5 --bind $(ip -4 -o addr show enp7s8 | awk '{print $4}' | cut -d/ -f1)

# 1. IL TRAFFICO E' NEL TUNNEL? (da pclab) - deve rispondere 104.105.82.146
curl -s ifconfig.me; echo

# 2. LE MISURE VERE (da pclab, SEMPRE verso l'IP di tunnel, MAI verso 104.105.82.146)
iperf3 -c 10.200.17.254 -p 5201 -R -t 15 -P 1     # single-stream down: atteso 40-85 Mbit
iperf3 -c 10.200.17.254 -p 5201 -t 15 -P 1        # upload: atteso 13-21
iperf3 -c 10.200.17.254 -p 5201 -R -t 15 -P 30    # aggregato: atteso 150+ (dipende dal cielo)

# 3. A/B PER CREDERCI: spegni l'affinity e guarda il P1 ricrollare (sul VPS)
ssh vps-mpquic-it-tpz-lab
sed -i 's/stripe_flow_affinity: true/stripe_flow_affinity: false/' /etc/mpquic/instances/mp1.yaml && systemctl restart mpquic@mp1
# ... rimisura il P1 da pclab (atteso: di nuovo 0.3-20 ballerino) ... poi riaccendi:
sed -i 's/stripe_flow_affinity: false/stripe_flow_affinity: true/' /etc/mpquic/instances/mp1.yaml && systemctl restart mpquic@mp1

# 4. IL ROUTING NON MENTE PIU' (da OpenWrt): WAN morte rosse, guasto esplicito
mwan3 status | grep interface
# guasto controllato: sul client "sudo systemctl stop mpquic@mp1" per 3+ minuti
#   -> BOND1 deve andare offline, STARLINK_PHY prendere il traffico (curl ifconfig.me da pclab = IP Starlink)
#   -> "start" e tutto rientra (curl = 104.105.82.146)

# Trappole da evitare (ci siamo cascati noi): niente "ping -I ethX" da OpenWrt (falsi loss da fwmark,
# usare "mwan3 use <WAN> ping"); niente iperf verso l'IP pubblico del VPS (host route = fisico);
# se iperf da' "Resource temporarily unavailable", riavviare il server iperf3 sul VPS prima di dedurre alcunche'.
```

## La mia raccomandazione (poi decidi tu)

Tenere lo strato 1 senza esitazioni (è ciò che rende oneste tutte le misure future, sul banco e su IBLEA). Tenere lo strato 2 finché il CAL non è debuggato. Sullo strato 3, fare la tua sessione di misure col protocollo sopra appena il link risale: l'A/B del punto 3 è il test che non ammette suggestioni: se il P1 crolla a flag spento e risale a flag acceso, il fix è reale e resta; se non vedi differenza, il rollback è una riga di config.

Riferimenti: [TS-021...TS-024](TROUBLESHOOTING_HISTORY.md) · [Report della notte](REPORT_NOTTE_20260728_FLOW_AFFINITY.md) · [Report collaudo (con erratum)](REPORT_COLLAUDO_BANCO_20260727.md) · [Audit IBLEA-M](AUDIT_IBLEAM_2026-07-27.md)
