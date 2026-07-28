# Report della notte 27→28 luglio: dal bypass nascosto alla flow-affinity validata

Il riassunto per chi si sveglia adesso: **la causa radice del single-stream collassato è stata trovata, corretta con un fix di trasporto scritto, deployato e validato stanotte, e il banco chiude la notte in salute**. Sotto, la catena completa con i numeri; il dettaglio tecnico vive in TS-023 e TS-024 della [TROUBLESHOOTING_HISTORY](TROUBLESHOOTING_HISTORY.md).

## La catena degli eventi

1. **La scoperta della serata** (tua): i collaudi passavano dal canale fisico, non da STRIPES. Tre concause chiuse: bypass TS-017 con default attivo (rimosso, commit `93a8e29`), host route che dirotta i test verso l'IP pubblico del VPS (metodo di test corretto: solo IP di tunnel), policy mwan3 sul tunnel debole mpq6 (riportata su BONDING/mp1).
2. **Pollicino**: su un P1 il VPS trasmette 22.771 pacchetti, il client ne riceve 22.770 (perdita 0.004%) ma **22.767 arrivano fuori ordine**. Nessun filtro: è lo striping per-pacchetto sulle 12 pipe. TCP legge il riordino come perdita, l'ARQ tempesta (217 NACK/s, 76% del ricevuto = ritrasmissioni), lo stimatore di loss segna il 100% finto che vedevi su Grafana.
3. **Il fix della notte: flow-affinity per pipe** (branch `feat/flow-affinity-3c03829`, commit `4c469dc`): ogni flusso interno inchiodato a una pipe via hash del 5-tuple, wire invariato, flag `stripe_flow_affinity` default off. Baseline pre-CAL `3c03829` su entrambi i lati (md5 `72ad66c4`).
4. **La sorpresa che ha dato la forma finale**: affinity attiva anche lato client uccideva l'upload (104 Kbit): un flusso su UNA pipe eredita al 100% la salute di quella pipe (binding CGNAT degradati), dove il round-robin ne assorbiva 1/12. E l'upload col RR era già sano. Configurazione finale **asimmetrica**: affinity SOLO sul server (downstream, la direzione malata), client resta round-robin.

## I numeri (tutti inoltrati da pclab, verso IP di tunnel)

| Misura | Prima (notte, RR) | Dopo (affinity lato server) |
|---|---|---|
| Single-stream download (P1 -R) | 0.3 - 21 Mbit, varianza selvaggia | **43 - 85 Mbit** |
| Single-stream upload (P1) | 13 - 47 Mbit | 13 - 21 Mbit (invariato, RR) |
| Aggregato (P30 -R) | 80 - 122 Mbit | **153 - 187 Mbit** |

Sotto saturazione piena (k6 50 utenti, validazione 30 min): il collo diventa la capacità del link condiviso: le probe si spartiscono la banda con i 50 utenti (1.5-29 Mbit), il ping perde 25-35% per code piene, k6 chiude con pagine 83% / light 94% / mediana 775ms: nel rumore rispetto al pre-affinity, perché sotto saturazione comanda la banda, non più il riordino. Il guadagno strutturale si misura a parità di condizioni a linea libera, ed è quello in tabella.

## Il resto della notte, in breve

- **Soak 2h07 completato** (pre-affinity, sistema validato su BONDING/mp1): k6 pagine 90% ok (contro l'8% del pomeriggio col percorso sbagliato), routing a zero difetti per tutta la corsa: rule 6+2 integre in ogni campione, 0 restart, 0 EPERM.
- **Guasto lungo di mp1 (4 min, watchdog in pausa)**: continuità utente perfetta (ping 0% loss, IP pubblico flippato sul fisico e rientrato pulito sul VPS). Però la continuità veniva da un fall-through della tabella bd1 (BOND1 restava "online" finto): **chiuso con il pavimento blackhole anche su bd1**: ora il fallback fisico passa esplicitamente da `STARLINK_PHY`.
- **TS-023 e TS-024** scritte in history con tutte le evidenze.

## Cose che restano sul tavolo

1. **Modem LTE**: `enp7s7` senza lease da ieri sera (~22:00). Serve un check fisico; al ritorno si rivalida il dual-path (wan5+wan6) del sistema.
2. **ARQ range-retransmit**: 67k ritrasmissioni per 78 NACK in un run: la banda oggi lo assorbe, ma è il prossimo candidato di tuning (insieme a un AQM contro il bufferbloat sotto saturazione).
3. **Decisione di prodotto**: la flow-affinity è su un branch della baseline pre-CAL. Va deciso come riconciliarla col branch CAL/ML-KEM (`debug/anomalia-mpquic-20260727`) — il debug della versione nuova non è ancora iniziato.
4. **Handoff al team tbox**: resta sospeso; quando congeliamo la baseline (affinity inclusa?) si riapre con le istruzioni definitive.
5. Rollback sempre pronti: `.bak-pre-affinity` (binario pre-fix) e `.bak-pre-rollback-20260727` (versione CAL) su entrambi i lati.

Log grezzi in `scratchpad/soak2/` e `scratchpad/val30/`.
