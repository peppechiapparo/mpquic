# SKILL: vapt

## Descrizione

Questa skill fornisce il **framework completo di VAPT** (Vulnerability Assessment &
Penetration Testing) per l'infrastruttura TBOX di Telespazio (Proxmox + OpenWrt +
containers + ZeroTier).

Attivare questa skill **ogni volta** che si vuole:
- Avviare un assessment di sicurezza su una TBOX o target di rete
- Verificare la postura di sicurezza di un dispositivo della fleet
- Ottenere una roadmap di remediation per hardening
- Verificare la conformità NIS2 di un'infrastruttura
- Portare il team VAPT su un nuovo workspace/progetto

---

## Struttura del Team VAPT

```
vapt-coordinator            ← entry point, orchestratore OWASP VMG TriCycle
├── vapt-threat-modeler     ← STRIDE + attack surface mapping (PRIMA di tutto)
├── vapt-recon              ← port scan, service enum, OS fingerprint
├── vapt-scanner            ← 5-Phase: Nuclei/Nikto/Trivy + OWASP Top10 + ASVS
├── vapt-infra-auditor      ← SSH audit manuale OpenWrt/Proxmox/container
└── vapt-reporter           ← OWASP VMG report + lifecycle + verdetto formale
```

## Framework Metodologico

| Framework | Utilizzo |
|----------|---------|
| **OWASP VMG TriCycle** | Struttura del workflow: Detection → Reporting → Remediation |
| **STRIDE** | Threat modeling pre-scan (vapt-threat-modeler) |
| **OWASP Top 10** | Classificazione finding web/config (vapt-scanner Phase 2) |
| **OWASP ASVS L1** | Compliance check auth/session/crypto (vapt-scanner Phase 5) |
| **CVSS 3.1** | Severity scoring per tutti i finding |
| **NIS2 Art. 21** | Compliance mapping infrastruttura critica SATCOM |

---

## Container Docker VAPT

| Container | Image | Tool |
|----------|-------|------|
| `vapt-tools` | `tpz/vapt-tools` (custom) | nmap, nikto, sslscan, ssh-audit, openssl |
| `nuclei` | `projectdiscovery/nuclei` | CVE/misconfig template scan |
| `trivy` | `aquasec/trivy` | Container/config/fs scan |
| `nmap` | `instrumentisto/nmap` | Port scanning dedicato |

## Script disponibili (in .claude/skills/vapt/scripts/)

| Script | Descrizione |
|--------|-------------|
| `00-start-tools.sh` | Build e avvio container, update template Nuclei |
| `01-recon.sh` | Reconnaissance completa (TCP/UDP scan, SSH, SSL) |
| `02-vuln-scan.sh` | Automated scan (Nuclei + Nikto + NVD CVE lookup) |
| `03-container-scan.sh` | Trivy scan su container/configs via SSH |
| `99-stop-tools.sh` | Stop container Docker |

## Reference (in .claude/skills/vapt/references/)

| File | Contenuto |
|------|---------|
| `OPENWRT-AUDIT.md` | Checklist audit manuale OpenWrt (Dropbear, LuCI, firewall, ZT) |
| `PROXMOX-AUDIT.md` | Checklist audit manuale Proxmox VE (utenti, API, LXC, backup) |

---

## Quick Start — Come avviare un VAPT

### Step 1 — Setup ambiente

```bash
# Imposta il target
export VAPT_TARGET="10.10.11.254"
export VAPT_HOST="TBOX-EVO"
export VAPT_RESULTS="/tmp/vapt-results/$(date +%Y%m%d_%H%M%S)-$VAPT_HOST"

# Avvia i tool (build + nuclei template update)
.claude/skills/vapt/scripts/00-start-tools.sh
```

### Step 2 — Threat Modeling (STRIDE)

Attivare `vapt-threat-modeler` — nessun accesso di rete richiesto, solo analisi architetturale.
Output: `$VAPT_RESULTS/threat-model.md`

### Step 3 — Reconnaissance

```bash
.claude/skills/vapt/scripts/01-recon.sh
```

Output: `$VAPT_RESULTS/nmap-*.txt`, `ssh-audit.txt`, `ssl-audit.txt`, `recon-report.md`

### Step 4 — 5-Phase Vulnerability Scan

```bash
.claude/skills/vapt/scripts/02-vuln-scan.sh
```

Output: `nuclei-*.json`, `nikto-*.txt`, `phase5-compliance.md`, `vuln-scan-report.md`

### Step 5 — Container & Config Scan

```bash
.claude/skills/vapt/scripts/03-container-scan.sh
```

Output: `trivy-*.json`, `configs-staging/`

### Step 6 — Infrastructure Audit (manuale via agent)

Attivare l'agente `vapt-infra-auditor` con i parametri del target.
Output: `infra-audit-report.md`

### Step 7 — Report Finale + Verdetto

Attivare l'agente `vapt-reporter` con tutti i file in `$VAPT_RESULTS/`.
Output: `VAPT-FINAL-REPORT.md` con verdetto formale (APPROVED/BLOCKED/REJECTED)

### Step 8 — Cleanup

```bash
.claude/skills/vapt/scripts/99-stop-tools.sh
```

---

## Target della fleet Telespazio

| Host | IP MGMT | IP ZeroTier | Layer | Note |
|------|---------|------------|-------|------|
| TBOX-EVO | 10.10.11.254 | 10.202.x.x | OpenWrt + Proxmox | POC VAPT |
| GIULIOVERNE | 10.10.11.250 | 10.202.16.23 | OpenWrt | Fleet PRYSMIAN |
| CABLEENTERPRISE | 192.168.150.3 | 10.202.16.25 | OpenWrt | VSAT — usare `NMAP_TIMING=T2` |

**VSAT targets**: aggiungere `export NMAP_TIMING=T2` prima degli script di recon.

---

## Portabilità — Usare in un altro workspace

Questo team VAPT è **completamente auto-contenuto** in `.claude/skills/vapt/`
e `.claude/agents/vapt-*.md`.

Per portarlo su un altro workspace:

```bash
# Copia gli agenti VAPT
cp .claude/agents/vapt-*.md /path/to/other-workspace/.claude/agents/

# Copia la skill
cp -r .claude/skills/vapt/ /path/to/other-workspace/.claude/skills/

# Build dei container nel nuovo workspace
cd /path/to/other-workspace
docker compose -f .claude/skills/vapt/docker/docker-compose.vapt.yml build
```

---

## Note di sicurezza

- I tool VAPT richiedono **autorizzazione esplicita** del proprietario del target
- Non eseguire **exploit reali** — solo assessment passivo e identificazione
- I risultati in `$VAPT_RESULTS/` contengono informazioni **riservate** — non committare in git
- Aggiungere `/tmp/vapt-results/` al `.gitignore`
