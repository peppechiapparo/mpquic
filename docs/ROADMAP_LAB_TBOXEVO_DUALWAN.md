# Roadmap: banco di prova TBOX-EVO dual-WAN (pre-validazione fix IBLEA-M)

Data: 2026-07-27. Stato: **APPLICATA nel pomeriggio del 27 luglio** dopo review (fasi 0-4 complete, collaudo rapido superato). Dettaglio tecnico completo, root cause provata e numeri di validazione nella entry **TS-021** di [TROUBLESHOOTING_HISTORY.md](TROUBLESHOOTING_HISTORY.md). Restano: collaudo lungo (Fase 6 formale), accesso VPS lab e freeze della baseline (Fase 5/7).

Esito in una riga: rule di policy routing rese di proprietà networkd (`proto static`, immuni allo sweep `ManageForeignRoutingPolicyRules`), script canonico unico con refresh atomico (commit `8cfa38d`+`be5b903`), pavimento blackhole per tabella (fail-closed, mai bypass), refresh event-driven sui TUN (`d335a64`) + timer di riconciliazione 30s (`5f34f23`), OpenWrt riallineato (LTE→eth12/172.16.5.2, ONEWEB→eth11/172.16.4.2, chilli su eth0). Validato: `mwan3 use` STARLINK/LTE/BOND1 = 0% loss nei tunnel giusti, WAN morte **offline/rosse**, TS-014 rivalidato, 5 minuti di quiete a zero churn.

Obiettivo: portare il banco TBOX-EVO (VM 200 + OpenWrt VM 100 + VPS lab) a riprodurre lo scenario IBLEA-M e validarci sopra tutti i fix degli ultimi giorni. Solo a valle della validazione si interviene in produzione (todolist in [AUDIT_IBLEAM_2026-07-27.md](AUDIT_IBLEAM_2026-07-27.md)).

Scenario target dichiarato:

| Cosa | Valore |
|---|---|
| WAN6 | `enp7s8`, STARLINK |
| WAN5 | `enp7s7`, LTE |
| LAN CORPORATE | 192.168.222.0/29 |
| LAN CREW | 192.168.182.0/24 |
| Tunnel attivi | mpq5, mpq6, mp1 (le VSAT di IBLEA non ci sono, per il software è equivalente) |

## Il sintomo delle WAN "Online" con le porte staccate: spiegato e riprodotto

Due cause che si sommano, entrambe già note come pattern:

1. **Il carrier VirtIO non cade mai** (lezione TS-001): le porte fisiche della TBOX giù non si propagano alle NIC delle VM, né sulla VM mpquic né su OpenWrt. Gli slot vuoti hanno `carrier=1`. Quindi il verde/rosso di LuCI non potrà mai venire dal link: viene solo dal tracking mwan3 (`track_ip` 8.8.8.8/1.1.1.1).
2. **Le ip rule 1001-1006 sul client non ci sono**: i /30 di transito cadono in `main` ed escono NATtati dal default fisico (`enp7s7`, metrica 105). I ping di tracking di TUTTE le WAN passano da lì e mwan3 le dichiara Online. È il bypass di TS-020, di nuovo.

Con i fix in questa roadmap le WAN scollegate diventeranno **Offline (rosse)** perché il loro /30 finirà nella `blackhole` della tabella wanN e il tracking fallirà. Le WAN volutamente inutilizzate conviene invece disabilitarle in mwan3, così appaiono **Disabled (gialle)** come TELESAT, che è lo stato onesto per una porta non cablata.

## Root cause del bypass: confermata sperimentalmente sul banco

Esperimento delle 11:22 (restart di `mpquic-routing.service`, poi osservazione a 30 secondi):

```
11:22:44 rule=6   (script le crea, tutto ok)
11:23:44 rule=6
11:24:14 rule=0   (sparite tutte insieme)
```

Nella stessa finestra il journal di networkd mostra `mpq6: Configuring with 55-mpq6.network` ogni 2-15 secondi. La catena è:

- `ManageForeignRoutingPolicyRules` non impostato in `networkd.conf` = default `yes`: a ogni riconfigurazione networkd spazza le ip rule che non sono sue. Le nostre 1001-1006 muoiono; la 1017 (bd1) sopravvive perché è dichiarata in `27-bd1.network` ed è di proprietà networkd. La prova regge da sola: sopravvive esattamente e solo la rule networkd-owned.
- Qualcosa riconfigura `mpq6` in continuazione (storm). Anche a rule appena ricreate, entro 2-3 minuti il sweep le porta via. Il colpevole dello storm è da identificare (candidati: `mpquic-watchdog`/`ensure_tun.sh` che toccano il TUN, o il TUN che flappa e networkd che risponde).

Questa è quasi certamente anche la spiegazione sistemica della recidiva di TS-020 (rule "sparite" in campo) e va fixata alla radice, non ricreando le rule più spesso.

## Stato rilevato (27 luglio, read-only + esperimento sopra)

**VM 200** (identità confermata: `qm list` 200 running / 300 stopped, MAC `bc:24:11:3d:9b:59`):

- Binario client `4ac2bed3...` = **identico al VPS di produzione IBLEA**. Il fuori-coro è il client IBLEA (`c95df26c...`).
- `VPS_PUBLIC_IP=104.105.82.146` corretto (bonifica TS-020 tiene).
- mpq5, mpq6, mp1 UP con addressing `/24` peer `.254`; mpq1-4 DOWN; attive anche le istanze di classe cr/br/df 5 e 6.
- Nessun `Gateway=` fantasma sulla management (qui il fix TS-018 tiene).
- `mpquic-routing.service` gira e popola bene le tabelle (wan5/wan6 con `default dev mpq5/mpq6` e host route VPS), ma le rule vengono spazzate come sopra.
- wan-watchdog: funziona (ha rilevato lo scambio cavi delle 11:05 e riconfigurato entrambe le WAN in ~60-90s). La riga `WAN_INTERFACES` è commentata: se si decommenta servono le virgolette (insidia scoperta sulla VM 300).

**Script di routing: tre versioni divergenti in circolazione.** È il rischio più subdolo:

| Dove | Versione | Contenuto |
|---|---|---|
| Repo (`8cfa38d`) | canonica proposta | TS-011 (route /30 per-bridge) + TS-014 (host route disaccoppiata) + TS-017 (bypass via env) + TS-020 (rule idempotenti) |
| VM 200 lab (`9720e413`, 24/7 mai committata) | riscrittura a mano | TS-020 sì, ma **regredisce TS-014** (host route di nuovo accoppiata allo stato del TUN) e **manca TS-011** e TS-017 |
| Client IBLEA | pre-TS-020 | TS-011+TS-014+TS-017 sì, rule con del+add |

Nessuna delle tre le ha tutte tranne il repo. La canonicalizzazione è parte della roadmap.

**OpenWrt VM 100**:

- Mappatura WAN→/30 non allineata allo scenario: STARLINK giusta (`eth13`→172.16.6.x→mpq6), BOND1 giusta (`eth8.17`→172.16.17.x→mp1), ma **LTE punta a 172.16.4.x** (wan4/mpq4, morta) mentre il /30 di wan5 (172.16.5.x, il vero LTE su `enp7s7`) è assegnato a ONEWEB, scollegata. La card "LTE Online" di oggi è online solo via bypass.
- WAN1, KVHTS, ONEWEB sono `enabled` in mwan3 pur non essendo cablate (da qui le card verdi fantasma). TELESAT è già Disabled.
- CORPORATE ok: `LAN` su `eth0` = 192.168.222.1/29. CREW invece è ancora `eth0.5` = **192.168.0.1/24**, va portata a 192.168.182.0/24. Attenzione: il DHCP della CREW è CoovaChilli, non dnsmasq (lezione di flotta): il cambio subnet passa dalla config chilli, non solo da `network`.

**VPS lab (104.105.82.146)**: accesso fallito come `satcom` (publickey denied). Serve il metodo di accesso giusto per chiudere il confronto versioni client/server lab. Punto aperto.

## Fasi proposte

### Fase 0. Gate

Riconferma VMID, snapshot Proxmox di VM 200 e VM 100 (`qm snapshot ... pre-lab-dualwan-20260727`), backup `/etc/config` su OpenWrt e `.bak-20260727` per ogni file toccato sulla VM.

### Fase 1. Fix sistemico della persistenza delle rule (il cuore)

Due interventi complementari, entrambi sul banco prima che altrove:

1. **Spostare le 6 wan rule statiche dentro networkd** (`RoutingPolicyRule` nei file `2x-lanN.network`, come già fa `27-bd1.network` per la 1017): diventano di proprietà networkd, presenti dal boot, immuni al sweep e ai riavvii. La dinamica resta allo script (default dev mpqN vs blackhole nelle tabelle, host route, rule DHCP-dipendenti 1101/1201).
2. **`ManageForeignRoutingPolicyRules=no`** (e valutare `ManageForeignRoutes=no`) in `networkd.conf`, come cintura per le rule dinamiche 1101-1106/1201-1206 che restano allo script.

In più: caccia allo **storm di reconfigure di mpq6** (journal + ispezione di `mpquic-tunnel-watchdog.sh`/`ensure_tun.sh`): anche a rule salve, un reconfigure ogni 2-15 secondi è rumore che prima o poi presenta il conto.

### Fase 2. Script canonico unico

Deploy della versione repo (`8cfa38d`) sulla VM 200 con drop-in `MPQUIC_BYPASS_WANS=none` (sul banco il traffico STARLINK deve attraversare mpq6; il bypass TS-017 resta una scelta di produzione da riconsiderare a valle dei test). Test di non regressione TS-014 sul banco: `systemctl stop mpquic@6` per 60s, mp1 deve restare vivo. Poi la versione deployata e quella nel repo devono avere lo stesso md5, per sempre.

### Fase 3. OpenWrt allineato allo scenario

- LTE → device `eth12`, `ipaddr 172.16.5.2`, gw `172.16.5.1` (il /30 di wan5/mpq5); l'attuale ONEWEB su quel /30 si sposta o si disabilita.
- `mwan3`: WAN1, KVHTS, ONEWEB (e TELESAT, già così) → `enabled=0`: card Disabled, non fake-Online.
- CREW → 192.168.182.0/24 su `network` **e** config CoovaChilli (dhcpif/rete), CORPORATE resta 192.168.222.1/29.
- Verifica policy mwan3 per LAN/CREW (FAILOVER su LTE/STARLINK/BOND1 con le sole interfacce reali).

### Fase 4. wan-watchdog

Decommentare `Environment="WAN_INTERFACES=enp7s7 enp7s8"` (con virgolette) sulla VM 200. Il daemon ha già dimostrato di funzionare sullo scambio cavi delle 11:05.

### Fase 5. Versioni software

Chiarito l'accesso al VPS lab: confrontare md5 client/VPS lab. Decidere la baseline binaria (candidata naturale: `4ac2bed3`, già uguale sul client lab e sul VPS di produzione IBLEA; in alternativa rebuild pulito dal branch corrente che contiene i revert TS-016). Allineare client lab + VPS lab alla baseline: quella diventa la versione da portare sul client IBLEA (`c95df26c` è il fuori-coro da sostituire).

### Fase 6. Collaudo completo (checklist + LuCI)

- Trace su `9.9.9.9` per ciascun percorso: STARLINK→mpq6, LTE→mpq5, BOND1→mp1 (tcpdump sul client, conteggi pacchetti, reply verso il `.2` giusto).
- Rule stabili nel tempo: conteggio 6/6 dopo 30-60 minuti (non solo 60 secondi), attraverso rinnovi DHCP e churn watchdog.
- WAN morte: tabella in `blackhole`, card LuCI **Offline/rossa**; WAN disabilitate: **Disabled/gialla**.
- Scambio cavi: recupero wan-watchdog in 60-90s (già osservato oggi, da rifare a config finale).
- Failover mwan3 LAN/CREW tra LTE/STARLINK/BOND1.
- Throughput `-P1` e `-P30` sostenuti ≥30s via mp1 e mpq6, confrontati col tetto fisico misurato nello stesso momento (metodo TS-016).

### Fase 7. Freeze della baseline e passaggio a IBLEA

Congelare md5 di binario, script e unit validati + snapshot finale del banco. Da lì si riapre [AUDIT_IBLEAM_2026-07-27.md](AUDIT_IBLEAM_2026-07-27.md) e si replica in produzione nell'ordine già proposto (fantasma TS-018 → script+addressing → binario → mpq5).

## Punti aperti per la review

1. Accesso al VPS lab 104.105.82.146 (con che utente/chiave? `satcom` viene rifiutato).
2. Rule statiche in networkd (proposta 1 di Fase 1): d'accordo sul cambio di proprietà? È la strada più solida ma cambia dove "vivono" le rule.
3. Bypass TS-017 in produzione: il banco valida STARLINK dentro mpq6; su IBLEA oggi STARLINK bypassa STRIPES. Dopo i test va deciso se IBLEA resta in bypass o torna nel tunnel.
4. CREW/CoovaChilli: il cambio a 192.168.182.0/24 impatta captive portal e utenti crew: va fatto in finestra concordata?
5. Conferma della baseline binaria (promuovere `4ac2bed3` o rebuild dal branch).
