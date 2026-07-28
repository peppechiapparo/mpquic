# Proxmox VE Security Audit Reference

Guida ai controlli di sicurezza specifici per Proxmox VE nel contesto TBOX Telespazio.

## Versioni in uso

| Host | Proxmox VE | Kernel | Subscription |
|------|-----------|--------|-------------|
| TBOX-EVO (10.10.11.254) | da verificare | da verificare | da verificare |

## Checklist di Audit

### 1. Versione e Patch Level

```bash
# Versione PVE
pveversion --verbose

# Aggiornamenti disponibili
apt update && apt list --upgradable 2>/dev/null

# Security advisories Proxmox
# https://www.proxmox.com/en/downloads/item/proxmox-security-advisories
```

**Note critiche:**
- Proxmox senza subscription usa repo `pve-no-subscription` — aggiornamenti disponibili ma senza SLA
- Verificare [Proxmox Security Advisories](https://forum.proxmox.com/forums/proxmox-ve-8-x-security-advisories.51/)

---

### 2. Gestione Utenti e Accessi

```bash
# Utenti Proxmox
pveum user list

# Gruppi
pveum group list

# Roles e permessi
pveum role list
pveum acl list

# Dettaglio utente root
pveum user config root@pam

# Token API esistenti
pveum user token list root@pam 2>/dev/null
```

**Finding CRITICO** se:
- `root@pam` ha token API senza scadenza (`--expire 0`)
- Utenti non riconosciuti nel sistema
- Permessi `PVEAdmin` o `Administrator` su utenti non autorizzati

**Finding ALTO** se:
- Nessuna 2FA configurata per utenti admin

#### Verifica 2FA
```bash
pveum user config root@pam | grep tfa
# Atteso: tfa: type=totp (o yubico/webauthn)
# Se assente: nessuna 2FA configurata
```

---

### 3. SSH sul nodo Proxmox

```bash
# Configurazione SSH
cat /etc/ssh/sshd_config | grep -v "^#" | grep -v "^$"

# Check critici
grep PermitRootLogin /etc/ssh/sshd_config      # ideale: prohibit-password
grep PasswordAuthentication /etc/ssh/sshd_config # ideale: no
grep PubkeyAuthentication /etc/ssh/sshd_config   # ideale: yes

# Chiavi autorizzate
cat /root/.ssh/authorized_keys
```

---

### 4. Proxmox Web API (porta 8006)

```bash
# Versione API
curl -sk https://localhost:8006/api2/json/version | python3 -m json.tool

# Certificato SSL
openssl s_client -connect localhost:8006 2>/dev/null | openssl x509 -noout -text | \
  grep -E "Subject|Issuer|Not After|Signature Algorithm"

# CORS e headers di sicurezza
curl -skI https://localhost:8006 | grep -iE "strict-transport|x-frame|x-content|content-security"
```

**Finding ALTO** se:
- API su HTTP (non HTTPS)
- Certificato auto-firmato senza HSTS
- API accessibile da WAN senza ACL

---

### 5. Firewall Proxmox

```bash
# Status firewall
pve-firewall status

# Regole datacenter
cat /etc/pve/firewall/cluster.fw

# Regole nodo
cat /etc/pve/nodes/$(hostname)/host.fw
```

**Finding CRITICO** se:
- Proxmox firewall disabilitato con porta 8006 accessibile da internet
- Nessuna regola che limiti accesso SSH alla rete management

---

### 6. Container LXC — Sicurezza

```bash
# Lista container con config
for vmid in $(pct list | awk 'NR>1{print $1}'); do
  echo "=== CT $vmid ==="
  pct config $vmid | grep -E "hostname|ostype|net|privileged|nesting|features"
  echo ""
done
```

**Finding CRITICO** se:
- Container `privileged: 1` senza necessità — può breakout verso host
- Container con `root` senza password

**Finding ALTO** se:
- Container condividono la stessa rete dell'hypervisor senza segmentazione

---

### 7. Storage e Backup

```bash
# Storage overview
pvesm status

# Verifica crittografia backup
grep "encrypt\|crypt" /etc/vzdump.conf 2>/dev/null
```

**Finding MEDIO** se:
- Backup non crittografati
- Nessun backup configurato

---

### 8. NIS2 Gap Analysis per Proxmox

| Art. NIS2 | Requisito | Verifica Proxmox |
|----------|----------|-----------------|
| 21(2)(a) | Gestione rischio | Firewall abilitato, patch level aggiornato |
| 21(2)(b) | Incident management | Log Proxmox (`/var/log/pve/`) → syslog remoto? |
| 21(2)(c) | Business continuity | Backup configurati, DR plan |
| 21(2)(g) | Igiene informatica | Patch, password policy, no servizi inutili |
| 21(2)(h) | Crittografia | TLS API, backup encrypted |
| 21(2)(i) | Controllo accessi | RBAC configurato, MFA, ACL per utenti |
| 21(2)(j) | MFA | TOTP/WebAuthn per utenti admin Proxmox |
