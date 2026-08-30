# Runbook — ricostruzione di una TBOX mpquic da un clone vecchio

Sequenza eseguita e collaudata il 2026-08-30 sulla nuova TBOX-EVO (TS-034 in
`TROUBLESHOOTING_HISTORY.md`), dopo il guasto hardware della precedente. Vale per
qualunque TBOX il cui clone/backup sia indietro rispetto al repo: ogni passo dice
da dove viene il file canonico, cosa verificare e come tornare indietro.

Regole fisse (da `verify-vmid-before-mpquic-work` e dalla wiki):
- prima di scrivere: `qm list` sul Proxmox e `hostname` + MAC delle NIC sulla VM,
  per essere sicuri di quale VM si sta toccando;
- il riferimento di produzione (IBLEA-M) si legge, non si scrive;
- ogni file sostituito ha una copia in `/root/bak-<motivo>-<data>/`;
- diagnosi con numeri prima di ogni fix.

## 0. Ricognizione (tutto read-only)

| Dove | Cosa guardare | Comando |
|------|---------------|---------|
| Proxmox | VMID, NIC fisiche con carrier, bridge → VM | `qm list`; `for n in /sys/class/net/enp*; do echo $n $(cat $n/carrier); done`; `brctl show`; `qm config 200 \| grep ^net` |
| VM | identità, NIC guest, networkd, tabelle, script, unit, binario, VPS | `hostname; ip -br link; ls /etc/systemd/network; cat /etc/iproute2/rt_tables; md5sum /usr/local/sbin/mpquic-policy-routing.sh /usr/local/sbin/mpquic-vps-routes.sh /usr/local/bin/mpquic; cat /etc/mpquic/global.env; ls /etc/systemd/network/*.d` |
| VM | commit del binario | `go version -m /usr/local/bin/mpquic \| grep vcs` |
| VPS | stesso binario? stessa sezione crypto? | `md5sum /usr/local/bin/mpquic`; `grep -A8 stripe_crypto_enabled /etc/mpquic/instances/mp1.yaml` |
| OpenWrt | interfacce PHY, policy in uso, zona wan | `uci show network \| grep _PHY`; `uci show mwan3 \| grep use_member`; `uci show firewall \| grep zone.*network` |

Cose che un clone vecchio nasconde e che qui sono state trovate:
- residui di test (drop-in networkd che rendono statica una WAN, NIC della VM
  agganciata al bridge sbagliato);
- istanze a `/30` con peer `.2` mentre il server è `.254/24` (commit `c14ac81`,
  TS-020): il watchdog riavvia l'istanza ogni ~17 s, `/run/mpquic/watchdog-peer-fail`;
- `mpquic-vps-routes.sh` nella versione hard-coded del VPS (md5 `ea851b31`):
  lanciato sul client dall'`ExecStartPost` della unit canonica mette
  `172.16.N.0/30 dev mpqN` in `main` e le risposte verso l'OpenWrt tornano nel
  tunnel. Va sostituito col canonico (`scripts/mpquic-vps-routes.sh`).

## 1. Binario

Il client deve girare lo stesso commit del VPS a cui si collega. Build
riproducibile dal repo (`Makefile`: `-trimpath`, `CGO_ENABLED=0`), da un worktree
pulito del commit del VPS, e stesso artefatto sui due lati:

```bash
git worktree add /tmp/build-<commit> <commit> && cd /tmp/build-<commit> && make build
md5sum bin/mpquic
scp bin/mpquic <vps>:/tmp/ ; scp bin/mpquic <client>:/tmp/
# su ciascun lato:
sudo cp -a /usr/local/bin/mpquic /usr/local/bin/mpquic.bak-<md5vecchio>-<data>
sudo install -m 0755 /tmp/mpquic /usr/local/bin/mpquic && md5sum /usr/local/bin/mpquic
sudo systemctl restart 'mpquic@*'
```

Se il VPS ha una build "dirty" (`vcs.modified=true`) non si può riprodurre a md5
identico: si ricostruisce pulito e si installa su entrambi.

## 2. Proxmox

- NIC della VM sul bridge giusto (Starlink e LTE): `qm set 200 -net13 virtio=<MAC>,bridge=vmbr7`
  funziona a caldo (breve perdita di carrier sulla sola NIC toccata).
- Il cavo dell'uplink deve stare sulla porta fisica di quel bridge: `carrier=1` e
  `bridge fdb show br vmbrN | grep enpXs0` con MAC imparati. Se il lease arriva su
  un'altra NIC della VM, il cavo è su un altro bridge.

## 3. VM — file canonici (fonte: il repo, identici a IBLEA-M)

```
deploy/networkd/wan/14-wan5.network 15-wan6.network      # UseGateway=false (TS-028)
deploy/networkd/vlan/20..25-lan*.network                 # rule proto static + pavimento blackhole + VLAN= 95/96
deploy/networkd/bd1/27-bd1.network
deploy/networkd/phy/28-*.{network,netdev} 29-*.{network,netdev}
deploy/networkd/networkd.conf.d/10-mpquic-foreign-rules.conf
deploy/networkd/rt_tables                                # righe phy1-6 (191-196) se mancano
scripts/mpquic-policy-routing.sh                         # blocco phy_table
scripts/mpquic-vps-routes.sh                             # versione RETURN_SUBNETS (no-op sul client)
deploy/systemd/mpquic@.service mpquic-routing.service mpquic-routing.timer
deploy/config/client/{1..6}.env {1..6}.yaml              # addressing /24 peer .254 (-> N.env e N.yaml.tpl)
```

Applicazione, nell'ordine:

```bash
sudo mkdir -p /root/bak-<motivo>-<data> && sudo cp -a /etc/systemd/network /etc/systemd/system/mpquic*.service /etc/mpquic /usr/local/sbin/mpquic-*.sh /etc/iproute2/rt_tables /root/bak-<motivo>-<data>/
# rimuovere eventuali drop-in di test in /etc/systemd/network/*.d (spostarli nel backup)
sudo install -m 0644 <networkd files> /etc/systemd/network/
sudo install -m 0644 10-mpquic-foreign-rules.conf /etc/systemd/networkd.conf.d/
grep -q phy6 /etc/iproute2/rt_tables || printf '191 phy1\n192 phy2\n193 phy3\n194 phy4\n195 phy5\n196 phy6\n' | sudo tee -a /etc/iproute2/rt_tables
sudo install -m 0755 mpquic-policy-routing.sh mpquic-vps-routes.sh /usr/local/sbin/
sudo install -m 0644 mpquic@.service mpquic-routing.service mpquic-routing.timer /etc/systemd/system/
printf 'VPS_PUBLIC_IP=<vps>\n' | sudo tee /etc/mpquic/global.env
# mp1.yaml: vedi deploy/config/client/mp1.tbox-evo-lab.yaml (banco) o mp1.yaml (canonico); IP hardcoded anche in mp1_RS_ADAPTIVE.yaml
sudo systemctl daemon-reload
sudo networkctl reload && sudo networkctl reconfigure enp7s7 enp7s8 enp7s1 enp7s2   # solo i link toccati
sudo systemctl restart mpquic-routing.service && sudo systemctl enable --now mpquic-routing.timer
sudo systemctl restart mpquic@mp1 mpquic@6 mpquic@cr6 mpquic@br6 mpquic@df6
```

Verifiche (tutte con numeri):

```bash
ip -br addr | grep -E 'enp7s[78] |\.95|\.96'        # lease WAN, VLAN 95/96 con .1
ip rule | grep -c 'proto static'                     # rule 100x/1017/1095/1096 networkd-owned
ip route show table phy6                             # default via <gw DHCP> sopra il blackhole 9999
ip route show table main | grep -E 'dev mpq[0-9] scope link$'   # deve essere vuoto
ip route get 172.16.6.2 from 8.8.8.8 iif mpq6        # deve dire dev enp7s2, NON dev mpq6
ping -c5 -I 172.16.96.1 8.8.8.8                      # canale fisico via VLAN 96
ping -c5 10.200.17.254 ; ping -c5 10.200.6.254       # peer dei tunnel
ls /run/mpquic/watchdog-peer-fail 2>/dev/null        # deve NON esistere
```

## 4. OpenWrt — canali fisici PHY

`deploy/openwrt/06-phy-interfaces.sh` (idempotente, fa backup in `/root/bak-phy/`).
Variabili: `TRUNK_LTE`/`TRUNK_STARLINK` (device verso LAN5/LAN6 della VM),
`WAN_ZONE` (zona con masq), `POLICIES` (dove mettere i membri in coda: sul banco
`BONDING FAILOVER`, su IBLEA-M anche `ZT_CLEAN`).

Collaudo corretto: `mwan3 use STARLINK_PHY ping -c 5 8.8.8.8`. Un `ping -I eth13.96`
"a mano" NON riproduce il tracker: con la marcatura mwan3 in OUTPUT esce da
un'altra WAN. Il tracker usa il wrapper `LD_PRELOAD` (device + sorgente +
fwmark 0x3f00), che è esattamente ciò che fa `mwan3 use`.

## 5. Chiusura

- entry in `docs/TROUBLESHOOTING_HISTORY.md` con le evidenze;
- accessi nuovi (IP Proxmox, porte fisiche, jump) nelle note di accesso;
- `mwan3 status`: ogni membro online o offline per una ragione vera (niente rossi strutturali, TS-029).
