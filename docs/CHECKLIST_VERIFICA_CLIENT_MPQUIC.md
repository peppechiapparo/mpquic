# Checklist di verifica di un client mpquic

La lista da controllare prima di dire che un client mpquic è configurato bene. Nasce dai troubleshooting TS-013…TS-020: ogni voce corrisponde a un errore che è già costato tempo o un outage. L'ordine conta: si parte dall'identità della macchina e si finisce con il trace del traffico reale.

Regola di fondo: un canale "online" su OpenWrt non vuol dire che il traffico passa dal tunnel giusto. L'unica prova è tracciare il percorso di un ping su un IP fisso non usato per il tracking (`9.9.9.9`) e vedere dove va davvero.

## 0. Identità della macchina

Prima di toccare qualsiasi cosa.

- [ ] Confermato su **quale VMID** si sta lavorando, alla sorgente (host Proxmox: `qm list`, `qm config <id>`, match del MAC), non fidandosi dell'IP. Lo stesso IP può puntare a VM diverse.
- [ ] Il software e le config trovate sono quelle attese per questo client. Se trovo mpquic sparito, interfacce diverse, config assente o di un altro fornitore, **mi fermo e avviso**: può essere la VM sbagliata. Non procedo assumendo "l'hanno rifatta".

## 1. Binario e TLS allineati alla VPS

- [ ] `md5sum /usr/local/bin/mpquic` sul client uguale a quello sulla VPS, e `sudo readlink -f /proc/$(systemctl show mpquic@mp1 -p MainPID --value)/exe` per confermare che sia davvero quello in esecuzione. Il binario in `/opt/mpquic/bin/` è il prodotto della build, non ciò che gira: su IBLEA-M il v5.1 è rimasto lì per un giorno mentre le unit eseguivano ancora il vecchio (TS-027).
- [ ] Nota sull'md5: `go build` timbra la revisione git nel binario, quindi l'uguaglianza vale a parità di commit e con worktree pulito. Se i due lati sono su commit diversi l'md5 differisce anche a codice identico: prima si confronta `git rev-parse --short HEAD`, poi l'md5.
- [ ] `md5sum /opt/mpquic/bin/mpquic` sul client uguale a quello sulla VPS. Un binario in drift dà regressioni silenziose (TS-016). Dalla v5.1 le build sono riproducibili (`-trimpath` + `CGO_ENABLED=0` nel Makefile), quindi lo stesso commit DEVE dare lo stesso md5 su qualunque host: se differisce, o i commit sono diversi (`git rev-parse HEAD` sui due lati) o qualcuno ha compilato a mano fuori dal Makefile.
- [ ] `/etc/mpquic/tls/ca.crt` sul client è la CA della VPS con cui deve parlare (`md5sum` uguale).

## 2. IP della VPS, nessun IP ereditato

- [ ] `grep VPS_PUBLIC_IP /etc/mpquic/global.env` è l'IP **della VPS di questo ambiente**, non quello ereditato da un clone o da un'altra nave.
- [ ] Nessun IP di un'altra VPS in giro per le config:
  ```
  grep -rIl "<IP_VPS_ALTRUI>" /etc/mpquic /run/mpquic /usr/local/sbin
  ```
  deve essere vuoto (i `.bak` non contano). I `/run/mpquic/*.yaml` sono render runtime: se hanno un IP vecchio è perché l'istanza è partita prima del fix del `global.env`, si rigenerano al restart.
- [ ] I template `/etc/mpquic/instances/*.yaml.tpl` usano il placeholder `remote_addr: VPS_PUBLIC_IP`, non un IP hardcoded. Un IP hardcoded salta la sostituzione per-host ed è il modo con cui il config di una nave rompe un'altra (TS-019).

## 3. Addressing dei tunnel

- [ ] Ogni tunnel ha CIDR `/24` con peer `.254` (es. `10.200.6.1/24`, peer `10.200.6.254`), non `/30` con peer `.2`. Il `/30` è la vecchia forma che dava flapping.
- [ ] Il peer risponde: `ping -c3 10.200.N.254` a 0% quando il tunnel è su.
- [ ] Lato VPS l'indirizzo speculare esiste con lo stesso schema (`10.200.N.254/24`).

## 4. Routing per-WAN

Il principio: a ogni subsotto `/30` di transito corrisponde il suo tunnel, e basta. Se il tunnel è giù il traffico va **scartato** (blackhole), non deviato su un'altra WAN fisica. Solo così ogni WAN di OpenWrt è legata al suo canale e non a quello per caso attivo.

- [ ] Le `ip rule` per WAN ci sono tutte: `ip rule | grep -E "^100[1-6]:"` ne conta 6 (una per `172.16.N.0/30 lookup wanN`).
- [ ] Per un tunnel **su**: `ip route show table wanN` ha `default dev mpqN`.
- [ ] Per un tunnel **giù**: `ip route show table wanN` ha `blackhole default`, **non** un default verso `enp7s8` o un'altra WAN.
- [ ] Le regole restano stabili nel tempo. Con il watchdog attivo, ricontrolla il conteggio dopo 30-60s: deve restare 6. Se spariscono, lo script di routing sta cancellando regole statiche a ogni evento (bug TS-020): la wan rule va resa idempotente (aggiunta solo se assente), non cancellata e riaggiunta.

## 5. Il test vero: trace del percorso

Per **ogni** WAN, non solo per quella che sembra attiva. Servono tre terminali (OpenWrt, client, VPS).

1. Su OpenWrt, forza la WAN e manda un ping fisso:
   ```
   mwan3 use <NOME_WAN> ping -c 15 9.9.9.9
   ```
2. Sul client, in parallelo, traccia:
   ```
   tcpdump -ni any host 9.9.9.9 and icmp
   ```
3. Sulla VPS, stessa traccia, e controlla anche il percorso di ritorno.

Cosa deve risultare, con la WAN N mappata sul tunnel mpqN:

- [ ] Sul client i pacchetti **entrano** dall'interfaccia di transito (es. `enp6s20.N`, src `172.16.N.2`, dst `9.9.9.9`) ed **escono** dal **tunnel giusto** (`mpqN Out`), non da `enp7s8`.
- [ ] Le reply rientrano dallo stesso tunnel e tornano verso `172.16.N.2`.
- [ ] Il conteggio dei pacchetti su `mpqN` corrisponde a quelli inviati (es. 11 In e 11 Out per 11 richieste andate a buon fine), e su `enp7s8` non ne finisce nessuno che non gli spetta.
- [ ] Per BOND1 / multipath: ingresso su `enp6s20.17` con src `172.16.17.2`, uscita tutta su `mp1`.

Se il ping "funziona" ma la reply non torna verso `172.16.N.2`, o se i pacchetti escono da un'interfaccia diversa dal tunnel della WAN, il client **non** è configurato bene, anche se OpenWrt lo dà verde.

## 6. Watchdog e stabilità

- [ ] Il tunnel non fa flapping: `systemctl show mpquic@N -p NRestarts` stabile in una finestra di 40-60s.
- [ ] Il watchdog non va in loop su tunnel permanentemente morti. Se le WAN fisiche `enp7s3-7` non hanno IP e i relativi mpqN non partiranno mai, il churn del watchdog amplifica ogni finestra di riconfigurazione del routing. Con lo script di routing idempotente (voce 4) il churn non fa più danni, ma resta rumore da tenere d'occhio.

## 7. Deploy: sempre dallo script, su entrambi i lati

La via ufficiale è `mpquic-update.sh` dal repo, prima sul server poi sul client. Nessuna copia manuale del binario: era una deroga d'emergenza e ha già prodotto un caso di v5.1 "deployato" che in realtà non girava (TS-027).

- [ ] Entrambi gli host sono sullo stesso ramo e sullo stesso commit (`git -C /opt/mpquic rev-parse --short HEAD`), con worktree pulito.
- [ ] Il ramo in produzione porta anche `deploy/` e `scripts/` aggiornati, non solo il codice: l'update installa `mpquic@.service` e gli script di libreria **dal repo**, quindi un ramo vecchio riporta indietro l'operatività.
- [ ] Server: `sudo MPQUIC_UPDATE_SKIP_PULL=1 /opt/mpquic/scripts/mpquic-update.sh /opt/mpquic` (togliere `SKIP_PULL` dove il `git pull` passa).
- [ ] Client di bordo: il `git fetch` dalla nave va in timeout, quindi il commit viaggia come **bundle git**:
  ```bash
  # dal dev box
  git bundle create /tmp/delta.bundle <branch> --not <commit-gia-presente-sul-client>
  scp -J vps-mpquic-it-tpz-<nave> /tmp/delta.bundle satcom@10.200.17.1:/tmp/
  # sul client
  cd /opt/mpquic && sudo git fetch /tmp/delta.bundle "<branch>:refs/remotes/bundle/delta" && sudo git merge --ff-only bundle/delta
  ```
  Sono pochi KB e il percorso resta quello standard: cambia solo come arrivano gli oggetti.
- [ ] L'update sul client va lanciato **staccato dalla sessione** (`systemd-run --unit=... --setenv=HOME=/root --setenv=MPQUIC_UPDATE_SKIP_PULL=1 ...`) con un dead-man armato: lo script ferma tutte le istanze, mp1 compreso, e quella è la via d'accesso.
- [ ] A fine update: `md5sum /usr/local/bin/mpquic` uguale sui due lati, `ExecStartPost` presente nella unit, istanze attive, ping al peer del tunnel.

## Note

- I `.bak*` lasciati dai fix contengono di proposito i valori vecchi: non contano nei grep di verifica, servono per il rollback.
- Le occorrenze dell'IP di produzione IBLEA (`172.238.232.223`) nella documentazione del repo descrivono l'ambiente IBLEA reale e sono corrette. La bonifica riguarda le config operative degli **altri** ambienti (lab, nuove navi), non la doc di produzione.
