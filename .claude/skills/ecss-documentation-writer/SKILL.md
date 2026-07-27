# SKILL: ecss-documentation-writer

## Descrizione

Questa skill guida la **generazione e il mantenimento di documentazione tecnica conforme agli standard ECSS** (European Cooperation for Space Standardization) per il progetto tbox/NOVA EDGE di Telespazio.

Attivare questa skill ogni volta che si deve:
- Creare un nuovo documento tecnico di progetto
- Aggiornare documentazione esistente a seguito di modifiche software/sistemistiche
- Revisionare la conformità ECSS di un documento esistente
- Creare o aggiornare la Compliance Matrix
- Aggiornare Justification Files o Requirements Traceability

---

## Contesto del progetto

Il progetto tbox/NOVA EDGE è classificato come **infrastruttura critica SATCOM** ai sensi della Direttiva NIS2. Opera in contesto aerospaziale/navale e richiede documentazione ingegneristica strutturata secondo ECSS.

Progetti documentati:

| Progetto | Cartella docs | Tipo documento principale |
|----------|--------------|--------------------------|
| NOVA EDGE portal | `docs/edge/` | EDGE-TDD-001, EDGE-UM-001 |
| tmon + pumbaa + luci-app-tbox | `docs/tmon-pumbaa/` | TMON-TDD-001, TMON-UM-001 |
| data_usage | `docs/data_usage/` | DAUS-TDD-001 |

---

## Regole tassative di drafting ECSS

### R1 — Sintassi dei requisiti

Ogni requisito **DEVE** rispettare le seguenti regole:

1. **Verbo "shall"** per requisiti vincolanti (non negoziabili):
   > `[REQ-SW-001] The system shall log all configuration changes with timestamp, user identifier, and affected parameter.`

2. **Verbo "should"** per raccomandazioni (fortemente consigliati ma non obbligatori):
   > `[REQ-SW-002] The system should complete a data collection cycle within 60 seconds for fleets up to 100 nodes.`

3. **Verbo "may"** per opzioni permesse (implementazione discrezionale):
   > `[REQ-SW-003] The system may display historical traffic charts with configurable time ranges.`

4. **Termini BANDITI** nei requisiti (ambiguità non accettabile in contesto ECSS):
   - ❌ adeguato, sufficiente, appropriato, ottimizzato, rapido, veloce
   - ❌ normalmente, generalmente, tipicamente
   - ❌ e/o, ecc., etc.
   - ❌ minimo / massimo senza valore numerico esplicito
   - ✅ Sostituire con valori misurabili: `within 500 ms`, `at least 99.5% uptime`, `no more than 5 concurrent sessions`

### R2 — Identificazione univoca dei requisiti

Ogni requisito deve avere un **ID univoco e stabile** secondo il pattern:

```
[REQ-<CATEGORIA>-<NNN>]
```

Categorie standardizzate per questo progetto:

| Categoria | Significato | Esempio |
|-----------|-------------|---------|
| `SYS` | Requisiti di sistema (cross-component) | `[REQ-SYS-001]` |
| `SW` | Software (backend, algoritmi) | `[REQ-SW-010]` |
| `NET` | Networking (mwan3, WAN, routing) | `[REQ-NET-005]` |
| `SEC` | Sicurezza (NIS2, autenticazione) | `[REQ-SEC-001]` |
| `UI` | Interfaccia utente (LuCI, EDGE portal) | `[REQ-UI-003]` |
| `INT` | Interfacce tra componenti (ICD) | `[REQ-INT-002]` |
| `OPS` | Operativi / deployment | `[REQ-OPS-007]` |
| `PERF` | Performance e timing | `[REQ-PERF-001]` |
| `CONF` | Configurazione e parametri | `[REQ-CONF-004]` |

### R3 — Atomicità dei requisiti

Ogni requisito deve essere **atomico**: verificabile con un singolo test, non ambiguo, indipendente da altri.

**Sbagliato** (non atomico):
> `[REQ-SW-005] The system shall log all operations and send alerts when quotas are exceeded and restart services automatically.`

**Corretto** (3 requisiti atomici):
> `[REQ-SW-005] The system shall log all configuration operations to the audit trail.`
> `[REQ-SW-006] The system shall generate an alert when a subnet quota consumption exceeds 90% of the configured limit.`
> `[REQ-SW-007] The system shall automatically restart the tmon and pumbaa daemons upon abnormal termination.`

### R4 — Tracciabilità bidirezionale

Ogni requisito deve essere tracciabile secondo la catena:

```
[REQ-XXX-NNN] → Design Component → Implementation (file:line) → Test Case → Validation
```

### R5 — Struttura documento obbligatoria

```markdown
| Campo | Valore |
|-------|--------|
| Document ID | TPZ-[PROJ]-[TYPE]-[NNN] |
| Issue | N.N |
| Revision | N |
| Status | Draft / Released / Obsolete |
| Author | [Nome Cognome] |
| Date | YYYY-MM-DD |
| Applicable standards | ECSS-E-ST-40C, ECSS-Q-ST-80 |
```

**Document ID pattern:**

| Progetto | Codice | Tipo documento | Esempio |
|----------|--------|----------------|---------|
| NOVA EDGE portal | EDGE | TDD, UM, ICD, SRD | TPZ-EDGE-TDD-001 |
| tmon + pumbaa | TMON | TDD, UM, ICD | TPZ-TMON-TDD-001 |
| data_usage | DAUS | TDD, UM | TPZ-DAUS-TDD-001 |
| Cross-project | SYS | SRD, SEC, NET | TPZ-SYS-SEC-001 |

### R6 — Sezioni obbligatorie per TDD

1. **Document Information** (header ECSS)
2. **Table of Contents**
3. **Introduction** (Scope, Applicable Documents, Acronyms)
4. **System Overview** (Context, System Boundaries, Operational Scenarios)
5. **Requirements** (`[REQ-XXX-NNN] ... shall ...`)
6. **Architecture Design** (Component Overview, Data Flow)
7. **Interface Design**
8. **Verification & Validation** (Test Cases con `[TC-XXX-NNN]`)
9. **Requirements Traceability Matrix (RTM)**
10. **Change Log** (ECSS change record)

---

## Glossario ECSS standard

| Termine ECSS | Definizione |
|-------------|-------------|
| **shall** | Requisito vincolante; la non-conformità è un difetto |
| **should** | Raccomandazione; la non-conformità deve essere giustificata |
| **may** | Permesso opzionale; implementazione discrezionale |
| **ICD** | Interface Control Document |
| **TDD** | Technical Design Document |
| **RTM** | Requirements Traceability Matrix |

---

## Workflow di aggiornamento documentale

```
1. Identificare il/i requisito/i impattato/i dalla modifica
2. Aggiornare la sezione Requirements se la modifica introduce un nuovo comportamento
3. Aggiornare la sezione Architecture Design
4. Aggiornare o aggiungere test case nella sezione V&V
5. Aggiornare la RTM
6. Aggiornare il Change Log in fondo al documento
7. Incrementare il numero di Revision del documento
```

---

## Note di compliance ECSS per questo progetto

**Livello di compliance adottato: ECSS-inspired (Level 2)**

Il progetto tbox/NOVA EDGE adotta uno stile documentale **ECSS-inspired** — non formal ECSS compliance.

**Standard di riferimento applicati:**
- ECSS-E-ST-40C (Software Engineering) — per struttura TDD, SRD, test
- ECSS-Q-ST-80 (Software Product Assurance) — per qualità e RTM
- ECSS-M-ST-40 (Configuration Management) — per document control e versioning
