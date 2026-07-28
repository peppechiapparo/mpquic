# Audit read-only IBLEA-M, 27 luglio 2026: stato dei fix e todolist di remediation

Contesto: audit in sola lettura sul client mpquic di produzione IBLEA-M (via `ssh -J vps-mpquic-it-tpz-iblea satcom@10.200.17.1`) e sul VPS `172.238.232.223`, per verificare quali dei fix degli ultimi giorni (TS-013...TS-020) sono presenti in produzione. Nessuna correzione applicata. Decisione concordata: prima si validano tutti i fix sul banco locale TBOX-EVO, poi si interviene qui.

Identità verificata dal percorso stesso: accesso via jump dal VPS di produzione sul tunnel mp1 (10.200.17.1), `VPS_PUBLIC_IP=172.238.232.223` nel `global.env`.

## Issue aperte (ordine di gravità)

- [ ] **1. TS-018 regredita: default fantasma tornata.** `01-mgmt1.network` contiene di nuovo `Gateway=10.10.11.1`; la rotta `default via 10.10.11.1 dev enp6s18 proto static` (metrica 0) è viva in main; 10.10.11.1 non risponde, ARP `INCOMPLETE`. È il buco nero del dataloss al 40%. Il fix del 23 luglio era stato applicato in modo persistente con backup `.bak-ts018`: capire cosa lo ha riportato indietro (restore? propagazione config? redeploy?) fa parte del fix, altrimenti regredisce ancora.
- [ ] **2. Script routing pre-TS-020.** `/usr/local/sbin/mpquic-policy-routing.sh` deployato fa ancora `del`+`add` delle rule statiche 1001-1006 (finestra di bypass). Oggi il churn è basso (`NRestarts=0` su tutte le istanze, rule 6/6 stabili in due letture a 45s), quindi rischio latente, non emergenza. TS-014 e TS-017 sono invece presenti nel deployato. Fix: deploy della versione repo con commit `8cfa38d`.
- [ ] **3. Addressing tunnel a metà migrazione.** Istanze 1-4 ancora `/30` peer `.2` (forma flapping-prone); istanza 5 e mp1 già `/24` peer `.254`. Ogni tunnel è coerente col proprio lato VPS (1-4 `/30` anche sul VPS, 5 e mp1 `/24`), quindi funziona, ma il port è incompleto.
- [ ] **4. Binario in drift client/VPS.** Client `c95df26c...` ≠ VPS `4ac2bed3...`. La checklist richiede md5 identici (TS-016: il drift di binario è la via delle regressioni silenziose). Prima di allineare va stabilito quale versione è quella giusta: la baseline validata sul banco e' ora **v5.1** (`feat/flow-affinity-3c03829`, binario md5 `72ad66c4`, flag `stripe_flow_affinity: true` SOLO lato server): e' questa la versione da portare su client e VPS IBLEA, non la 4ac2bed3. **Procedura di deploy validata sul banco (28/07)**: una-tantum `git fetch origin && git checkout feat/flow-affinity-3c03829`, poi il classico `sudo /opt/mpquic/scripts/mpquic-update.sh /opt/mpquic` (server prima, client poi). Due gotcha che si ripresenteranno su IBLEA: (1) il checkout dal branch CAL lascia `internal/` come residuo untracked (con file root-owned dai test) e la guardia dello script si ferma: fare backup tar e `sudo rm -rf internal/` prima dell'update; (2) il criterio md5 e' di nuovo valido: dalla v5.1 le build sono riproducibili (`-trimpath` + `CGO_ENABLED=0`), verificato con md5 identico su client e VPS lab (`4982065b` per il commit `36fd632`). Su IBLEA verificare anche che `/usr/local/go` sia allineato (e' quello che lo script usa, NON `/usr/bin/go`).
- [ ] **5. Anomalia mpq5 (LTE).** TUN `DOWN` in `ip -br link` ma `mpquic@5` attivo con telemetria `wan5 state=up`, rx/tx sbilanciato (3 rx contro 12 tx osservati), e tabella wan5 con DUE default `dev mpq5` (uno `proto static`, duplicato). Da investigare prima di fidarsi del canale LTE.
- [ ] **6. `remote_addr` hardcoded in `mp1.yaml`** (172.238.232.223 invece del placeholder `VPS_PUBLIC_IP`). Corretto per IBLEA, ma è il pattern di propagazione di TS-019. Minore.
- [ ] **7. OpenWrt 10.10.10.254 irraggiungibile dal client** (accesso ballerino di TS-018, install in carico al team tbox, vedi HANDOFF). Non verificabile la mappatura STARLINK→wan1: se OpenWrt manda ancora STARLINK sul /30 172.16.1.x, oggi quel traffico muore nel `blackhole` di wan1. Punto rimasto aperto in TS-018.

## Cosa risulta a posto

- Nessun IP estraneo nelle config (`104.105.82.146` e `172.232.209.51` assenti da `/etc/mpquic`).
- mp1 single-WAN `if:enp7s8`, 12 pipe, `stripe_reseq_disable: true` (TS-015 e TS-016 tengono).
- TS-017 attivo: wan6 con default diretto via 192.168.1.1 su `enp7s8` e host route VPS presente.
- TS-014 presente nello script deployato (host route disaccoppiata dallo stato del TUN).
- Rule remote 1203-1206 corrette per le WAN con IP.
- wan-watchdog attivo con script presente. La riga `WAN_INTERFACES` è commentata, quindi il difetto di quoting scoperto sulla VM 300 (`Environment=` senza virgolette scarta la seconda interfaccia) qui non è innescato; se mai la si decommenta, usare le virgolette.
- VPS: timer `mpquic-server-watchdog` attivo, 16 return route `172.16.x` presenti (TS-012 tiene).

## Ordine proposto per la remediation (dopo la validazione sul banco locale)

1. Issue 1 (fantasma): impatto immediato su tutto il traffico instradato.
2. Issue 2+3 insieme (stesso deploy: script TS-020 + addressing `/24`).
3. Issue 4 (allineamento binario alla baseline validata in locale) e issue 5 (diagnosi mpq5).
4. Issue 6 nel giro di bonifica config.
5. Issue 7 dipende dal team tbox (install/accessi).

Riferimenti: [CHECKLIST_VERIFICA_CLIENT_MPQUIC.md](CHECKLIST_VERIFICA_CLIENT_MPQUIC.md), [TROUBLESHOOTING_HISTORY.md](TROUBLESHOOTING_HISTORY.md) (TS-013...TS-020), [ROADMAP_VM300_ROMARS_ROTTE_WATCHDOG.md](ROADMAP_VM300_ROMARS_ROTTE_WATCHDOG.md) (le due insidie unit/bypass trovate sul deploy VM 300).
