# Istruzioni Operative — Tech Lead tbox

**LEGGERE QUESTO DOCUMENTO PRIMA DI INIZIARE QUALSIASI SESSIONE DI LAVORO.**

---

## Regola #1: Aggiornare SEMPRE la documentazione

Dopo ogni task completato (fix, feature, deploy, analisi), aggiornare **immediatamente** i seguenti documenti:

| Documento | Cosa aggiornare |
|-----------|----------------|
| `docs/CHANGELOG.md` | Nuova entry con: modifiche, file toccati, test eseguiti, risultati |
| `docs/ROADMAP_TMON_PUMBAA.md` | Stato delle fasi (✅ COMPLETATO), timeline, nuovi item se emersi |
| `docs/ARCHITECTURE_TMON_PUMBAA.md` | Sezioni impattate dalle modifiche (pseudocodice, componenti, limiti architetturali) |
| `docs/BUG_REPORT_TMON_PUMBAA.md` | Stato dei bug (fixato/aperto), nuovi bug se scoperti |
| `docs/DEPLOYMENT_GUIDE.md` | Storico deployment, nuove procedure, note operative |
| `docs/SECURITY_NIS2.md` | Misure di sicurezza NIS2: nuove implementazioni, stato, raccomandazioni |

**Non aspettare che l'utente lo chieda.** È responsabilità del tech-lead mantenere la documentazione aggiornata.

---

## Regola #2: Versioning

Quando si modifica codice funzionale (non solo docs):
- Aggiornare `__version__` nei file modificati seguendo SemVer:
  - **PATCH** (x.y.Z): bug fix, correzioni minori
  - **MINOR** (x.Y.0): nuove funzionalità retrocompatibili
  - **MAJOR** (X.0.0): breaking changes, refactoring architetturale
- Le versioni di pumbaa e tmon sono **indipendenti**
- La versione viene scritta automaticamente in UCI all'avvio del daemon (`sw_version`)

### Versioni correnti

| Componente | Versione | File sorgente della versione |
|------------|----------|------------------------------|
| pumbaa | definita in `pumbaa/usr/libexec/pumbaa_core.py` | `__version__` (importata da daemon e CLI) |
| tmon | definita in `tmon/usr/libexec/tmon_core.py` | `__version__` (importata da tmond e tmonctl) |

---

## Regola #3: Procedura di deploy

**MAI deployare senza backup.** Seguire SEMPRE la procedura completa in `docs/DEPLOYMENT_GUIDE.md`:

1. Pre-flight check (connessione, servizi running)
2. Backup timestampato (tutti i file + config UCI)
3. Deploy file (solo quelli modificati)
4. Verifica versione (`--version`, UCI `sw_version`)
5. Restart servizi (tmon prima, pumbaa dopo)
6. Verifica post-deploy (status, doctor, log)
7. Aggiornare storico deployment nel DEPLOYMENT_GUIDE

### Navi / Router noti

| Nome Nave | IP tbox | Note |
|-----------|---------|------|
| GRANDE CALIFORNIA | 10.202.1.162 | Accesso: `ssh root@10.202.1.162` |

---

## Regola #4: Compilazione e test

Prima di deployare qualsiasi modifica Python:
```bash
cd /opt/SATCOMVAS/src/tbox
python3 -m py_compile pumbaa/usr/libexec/pumbaa_core.py
python3 -m py_compile pumbaa/usr/sbin/pumbaa
python3 -m py_compile pumbaa/usr/bin/pumbaactl
python3 -m py_compile tmon/usr/libexec/tmon_core.py
python3 -m py_compile tmon/usr/sbin/tmond
python3 -m py_compile tmon/usr/bin/tmonctl
```

Test unitari (38 test per pumbaa_core):
```bash
python3 tests/test_pumbaa_core.py -v
```

Test `--version` locale:
```bash
python3 pumbaa/usr/sbin/pumbaa --version
python3 pumbaa/usr/bin/pumbaactl --version
PYTHONPATH=tmon/usr/libexec python3 tmon/usr/sbin/tmond --version
PYTHONPATH=tmon/usr/libexec python3 tmon/usr/bin/tmonctl --version
```

---

## Regola #5: Vincoli tecnici

- **Solo stdlib Python 3** — niente pip, niente pacchetti esterni
- **Nessun `shell=True`** in subprocess — mai più (vedi BUG-006)
- **Timeout su tutti i subprocess** — `timeout=60` default
- **`except Exception:`** — mai bare `except:`
- **Config UCI sono specifiche per nave** — non sovrascriverle mai durante deploy
- **`sw_version`** è l'unico campo UCI scritto dal software

---

## Regola #6: Stato corrente del progetto

### Fasi completate
- ✅ Fase 1 — Hotfix critici (BUG-001, BUG-006, BUG-008)
- ✅ Fase 2 — Stabilizzazione (BUG-002, BUG-003, BUG-004, BUG-009, BUG-010, BUG-012, BUG-014)
- ✅ Versioning strutturato (`__version__`, `--version`, UCI `sw_version`, subcomando `version`)
- ✅ Fase 3 — Refactoring: `pumbaa_core.py`, persistenza stato (BUG-005), test suite (BUG-011)
- ✅ Fase 10.4 — Hotfix produzione (3 bug fix) + deploy automatizzato (`deploy_to_prod.sh`)
- ✅ Fase 10.6 — WAN normalization + model routing agenti
- ✅ Fase 10.7 — Fix mwan3 routing rules UI (src_port/dest_ip) + SSH muxclient retry

### Prossime fasi (in ordine)
- 📋 **Fase 5** — Analisi byte counting tmon vs Starlink (PRIORITÀ ALTA — ma solo dopo aver fixato tutti i bug)
- 📋 **Fase 4** — Evoluzione funzionale: LuCI, notifiche, NIS2 audit

### Bug aperti

Nessun bug aperto. BUG-007 e BUG-013 risolti.

---

## Regola #7: Struttura repository

### Documentazione ECSS e struttura per progetto

I documenti tecnici ECSS-E-ST-40C sono **versionati insieme al codice sorgente** in ogni cartella di progetto:

| Cartella | Documento ECSS | Descrizione |
|----------|----------------|-------------|
| `tmon/docs/` | TPZ-TMON-TDD-001 (Issue 1 Rev 0) | TDD per tmon, pumbaa, luci-app-tbox |
| `edge/docs/` | TPZ-EDGE-TDD-001 (Issue 1 Rev 0) | TDD per NOVA EDGE portal |
| `data_usage/docs/` | TPZ-DAUS-TDD-001 (Issue 1 Rev 0) | TDD per data_usage dashboard |

**Schema Document ID:** `TPZ-[PROJ]-[TYPE]-[NNN]`  
- PROJ: `TMON` | `EDGE` | `DAUS` | `SYS`  
- TYPE: `TDD` | `SRD` | `UM` | `ICD` | `SEC` | `VVP` | `RTM`

**Regola:** Dopo ogni modifica software, invocare `@ecss-guardian` per aggiornare il documento ECSS di competenza.

```
tbox/
├── docs/                             ← Solo documenti cross-progetto
│   ├── CHANGELOG.md                  ← log modifiche (tutti i progetti)
│   ├── SECURITY_NIS2.md              ← sicurezza NIS2 (cross-project)
│   ├── MODEL_ROUTING_AGENTI.md       ← model routing agenti (team ops)
│   ├── TRAINING_OPERATIVO_EDGE.md    ← corso training operativo NOVA EDGE
│   └── WORKING_INSTRUCTIONS.md       ← QUESTO FILE
│
├── edge/docs/                        ← NOVA EDGE portal — tutta la documentazione
│   ├── TPZ-EDGE-TDD-001.md           ← TDD ECSS (versionato col codice)
│   ├── ARCHITECTURE.md               ← Architettura (incl. Fase8, Fleet Analysis)
│   ├── ROADMAP.md                    ← Roadmap (incl. storica, fleet onboarding)
│   ├── INSTALL_DEPLOYMENT_GUIDE.md   ← Installazione, deploy prod, onboarding TBOX
│   ├── MANUALE_OPERATIVO.md          ← Manuale operativo L1/L2/Admin
│   ├── AUTH_KEYCLOAK_2FA.md          ← Autenticazione e 2FA
│   ├── INTERFACES.md                 ← Interfacce API
│   └── FAMILY-FEELING.md             ← Design system Telespazio
│
├── tmon/docs/                        ← tmon + pumbaa — tutta la documentazione
│   ├── TPZ-TMON-TDD-001.md           ← TDD ECSS (copre tmon + pumbaa + luci-app-tbox)
│   ├── ARCHITECTURE.md               ← Architettura (incl. bug history come appendice)
│   ├── ROADMAP.md                    ← Roadmap evolutiva
│   └── DEPLOYMENT.md                 ← Guida deployment TBOX fleet
│
├── pumbaa/docs/
│   └── README.md                     ← Puntatori a tmon/docs/ (TDD condiviso)
│
├── luci-app-tbox/docs/
│   └── MANUALE_OPERATIVO.md          ← Manuale operativo interfaccia LuCI
│
├── data_usage/docs/                  ← data_usage dashboard
│   ├── TPZ-DAUS-TDD-001.md           ← TDD ECSS (versionato col codice)
│   └── ARCHITECTURE.md               ← Analisi byte counting e architettura
│
├── tmon/usr/libexec/tmon_core.py     ← v1.3.1
├── pumbaa/usr/libexec/pumbaa_core.py ← v2.2.1
├── edge/src/                         ← Next.js v1.7.0
└── .github/agents/                   ← definizioni agenti (incl. ecss-guardian)
```

---

## Checklist pre-sessione

- [ ] Leggere questo documento
- [ ] Verificare lo stato corrente della roadmap (`tmon/docs/ROADMAP.md` o `edge/docs/ROADMAP.md`)
- [ ] Controllare l'ultimo deploy (`tmon/docs/DEPLOYMENT.md` — storico TBOX; `edge/docs/INSTALL_DEPLOYMENT_GUIDE.md` — storico EDGE)
- [ ] Verificare il CHANGELOG per l'ultimo lavoro svolto (`docs/CHANGELOG.md`)
