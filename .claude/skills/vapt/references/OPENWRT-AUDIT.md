# OpenWrt Security Audit Reference

Questa guida fornisce i controlli specifici per l'audit di sicurezza di router OpenWrt
nel contesto dell'infrastruttura TBOX di Telespazio.

## Versioni TBOX in uso

| Fleet | OpenWrt | Kernel | Stato |
|-------|---------|--------|-------|
| TBOX-EVO | da verificare | da verificare | POC |
| GIULIOVERNE | 21.02.x | 5.4.x | EOL 2023 |
| CABLEENTERPRISE | 21.02.x | 5.4.x | EOL 2023 |

> ⚠️ **OpenWrt 21.02 è EOL** — non riceve più patch di sicurezza dal 2023.
> Questo implica CVE non patchate nel kernel, nei pacchetti di sistema e nei componenti base.

## Checklist di Audit

### 1. Autenticazione e Accessi

#### SSH (Dropbear)
```bash
# Verifica configurazione
uci show system.@system[0]
cat /etc/config/dropbear

# Check critici:
uci get dropbear.@dropbear[0].PasswordAuth  # deve essere 'off'
uci get dropbear.@dropbear[0].RootPasswordAuth  # deve essere 'off'
uci get dropbear.@dropbear[0].Port  # porta default 22

# Algoritmi SSH supportati
ssh-audit <target>

# Chiavi autorizzate
cat /root/.ssh/authorized_keys
```

**Finding CRITICO** se:
- `PasswordAuth=on` — brute force possibile
- `RootPasswordAuth=on` — root direttamente via SSH con password
- Chiavi non conosciute in `authorized_keys`
- Algoritmi deboli: `diffie-hellman-group1-sha1`, `ssh-dss`, `arcfour`

#### LuCI Web Interface
```bash
# Configurazione uhttpd
cat /etc/config/uhttpd
uci show uhttpd

# Check critici:
uci get uhttpd.main.listen_http   # solo redirect se HTTP abilitato
uci get uhttpd.main.listen_https  # deve essere presente
uci get uhttpd.main.redirect_https # deve essere '1'
```

**Finding ALTO** se:
- HTTP abilitato senza redirect HTTPS
- Certificato scaduto o auto-firmato senza HSTS
- LuCI raggiungibile da WAN

---

### 2. Firewall

```bash
# Regole nftables (OpenWrt 22+)
nft list ruleset

# Regole iptables (OpenWrt 21)
iptables -L -n -v

# UCI firewall config
uci show firewall
```

**Finding CRITICO** se:
- Policy INPUT su WAN non è DROP
- Forwarding senza controllo di stato
- Regole che consentono accesso da WAN a servizi interni senza autenticazione

**Finding ALTO** se:
- Regole ZEROTIER_IN/ZEROTIER_OUT assenti (violazione NIS2 isolation)
- UPnP abilitato (`pgrep miniupnpd`)

---

### 3. ZeroTier Security

```bash
# Status e networks
zerotier-cli info
zerotier-cli listnetworks
zerotier-cli listpeers

# Configurazione
cat /etc/zerotier/local.conf
uci show zerotier
```

**Checks NIS2 critici:**

| Check | Comando | Atteso |
|-------|---------|--------|
| Moon privata configurata | `cat /etc/zerotier/moons.d/` | File `*.moon` presente |
| allowManaged | `cat /etc/zerotier/local.conf` | `"allowManaged": false` |
| allowGlobal | `cat /etc/zerotier/local.conf` | `"allowGlobal": false` |
| allowDefault | `cat /etc/zerotier/local.conf` | `"allowDefault": false` |
| Blacklist interfacce | `cat /etc/zerotier/local.conf` | `lo`, `tun`, `eth1`, `eth2` blacklistati |
| Firewall REJECT | `iptables -L` | ZEROTIER_IN e ZEROTIER_OUT REJECT su IP pubblici |
| Network autorizzate | `zerotier-cli listnetworks` | Solo reti Telespazio |

---

### 4. Servizi e Processi

```bash
# Processi attivi
ps aux 2>/dev/null || ps

# Porte in ascolto
netstat -tlnup 2>/dev/null || ss -tlnup

# Servizi abilitati al boot
/etc/init.d/* status 2>/dev/null | grep "enabled"
```

**Servizi da disabilitare se non usati:**
- `telnet` — trasmette credenziali in chiaro
- `rsh` — legacy insicuro
- `snmp` — se community string debole
- `miniupnpd` — UPnP, rischio di port forwarding non autorizzato

---

### 5. File e Permessi Critici

```bash
# File SUID/SGID
find / -perm /4000 -o -perm /2000 2>/dev/null | grep -v proc

# File world-writable in percorsi sensibili
find /etc /bin /sbin /usr -perm -o+w 2>/dev/null

# Permessi file critici
ls -la /etc/shadow /etc/passwd /etc/config/
ls -la /root/.ssh/ 2>/dev/null
```

**Finding CRITICO** se:
- `/etc/shadow` world-readable
- File SUID non attesi
- `/root/.ssh/` con permessi troppo aperti

---

### 6. Aggiornamenti e Patch Level

```bash
# Lista pacchetti installati
opkg list-installed | wc -l

# Versione kernel
uname -r

# Data compilazione firmware
cat /etc/openwrt_release
```

**Finding ALTO** se:
- OpenWrt 21.02 (EOL) — suggerire upgrade a 23.05+
- Kernel < 5.15 con CVE non patchate

---

### 7. NIS2 Gap Analysis specifica OpenWrt

| Art. NIS2 | Requisito | Come verificare su OpenWrt |
|----------|----------|--------------------------|
| 21(2)(a) | Gestione rischio | Review policy firewall, aggiornamenti, password |
| 21(2)(b) | Incident management | Logging abilitato? Log remoto configurato? |
| 21(2)(g) | Igiene informatica | Password forti, SSH keys, no servizi inutili |
| 21(2)(h) | Crittografia | TLS 1.2+, no cipher deboli, no telnet/HTTP raw |
| 21(2)(i) | Controllo accessi | ACL LuCI, SSH chiavi-only, root access limitato |
| 21(2)(j) | MFA | LuCI supporta TOTP? Abilitato? |
