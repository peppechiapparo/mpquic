# SKILL: ecss-documentation-writer

## Descrizione

Questa skill guida la **generazione e il mantenimento di documentazione tecnica conforme agli standard ECSS** (European Cooperation for Space Standardization) per il progetto **MPQUIC/STRIPES** di Telespazio.

Attivare questa skill ogni volta che si deve:
- Creare un nuovo documento tecnico di progetto
- Aggiornare documentazione esistente a seguito di modifiche software/sistemistiche
- Revisionare la conformità ECSS di un documento esistente
- Creare o aggiornare la Compliance Matrix
- Aggiornare Justification Files o Requirements Traceability

---

## Contesto del progetto

Il progetto **MPQUIC/STRIPES** è classificato come **infrastruttura critica SATCOM** ai sensi della Direttiva NIS2. Realizza un sistema di tunneling multipath cifrato (AES-256-GCM) su link satellitari ibridi (LEO/GEO/LTE) con FEC Reed-Solomon adattivo, Hybrid ARQ NACK-based e scheduling multipath. Opera su VM Linux (Debian 12) gestita via systemd, con REST API di management e monitoraggio Prometheus/Grafana.

Documenti ECSS del progetto:

| Componente | Cartella docs | Document ID principale |
|------------|--------------|------------------------|
| MPQUIC dataplane + STRIPES transport | `docs/` | `TPZ-MPQUIC-TDD-001` |
| mpquic-mgmt REST API | incluso in TDD-001 §7 | — |

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

Ogni documento tecnico DEVE includere una **Requirements Traceability Matrix (RTM)** che documenta questa catena per ogni requisito.

### R5 — Struttura documento obbligatoria

Ogni documento ECSS prodotto per questo progetto deve avere il seguente header:

```markdown
| Campo | Valore |
|-------|--------|
| Document ID | TPZ-[PROJ]-[TYPE]-[NNN] |
| Issue | N.N |
| Revision | N |
| Status | Draft / Released / Obsolete |
| Author | [Nome Cognome] |
| Reviewed by | [Nome / Agent] |
| Approved by | [Nome] |
| Date | YYYY-MM-DD |
| Classification | Internal / Confidential |
| Applicable standards | ECSS-E-ST-40C, ECSS-Q-ST-80 |
```

**Document ID pattern:**

| Componente | Codice | Tipo documento | Esempio |
|------------|--------|----------------|---------|
| MPQUIC dataplane + STRIPES | MPQUIC | TDD, SRD, ICD, UM | TPZ-MPQUIC-TDD-001 |
| Cross-project / sistema | SYS | SRD, SEC | TPZ-SYS-SEC-001 |

**Tipi di documento:**

| Tipo | Significato |
|------|-------------|
| `TDD` | Technical Design Document (ADD + SDD) |
| `SRD` | Software Requirements Document |
| `UM` | User Manual / Operational Manual |
| `ICD` | Interface Control Document |
| `SEC` | Security Architecture Document |
| `VVP` | Verification & Validation Plan |
| `RTM` | Requirements Traceability Matrix |

### R6 — Sezioni obbligatorie per TDD

Ogni Technical Design Document deve contenere nell'ordine:

1. **Document Information** (header ECSS)
2. **Table of Contents**
3. **Introduction**
   - 3.1 Scope
   - 3.2 Applicable Documents (AD)
   - 3.3 Reference Documents (RD)
   - 3.4 Acronyms and Abbreviations
4. **System Overview**
   - 4.1 Context
   - 4.2 System Boundaries
   - 4.3 Operational Scenarios
5. **Requirements** (`[REQ-XXX-NNN] ... shall ...`)
   - 5.1 Functional Requirements
   - 5.2 Interface Requirements
   - 5.3 Performance Requirements
   - 5.4 Security Requirements
   - 5.5 Operational Requirements
6. **Architecture Design**
   - 6.1 Component Overview
   - 6.2 Component Description (per componente)
   - 6.3 Data Flow
7. **Interface Design** (ICD inline o riferimento a documento separato)
8. **Verification & Validation**
   - 8.1 Test Approach
   - 8.2 Test Cases (con ID: `[TC-XXX-NNN]`)
9. **Requirements Traceability Matrix (RTM)**
10. **Change Log** (ECSS change record)

---

## Glossario ECSS standard

| Termine ECSS | Definizione |
|-------------|-------------|
| **shall** | Requisito vincolante; la non-conformità è un difetto |
| **should** | Raccomandazione; la non-conformità deve essere giustificata |
| **may** | Permesso opzionale; implementazione discrezionale |
| **ICD** | Interface Control Document: specifica le interfacce tra componenti |
| **TDD** | Technical Design Document: architettura e design del software |
| **SRD** | Software Requirements Document: tutti i requisiti del sistema |
| **RTM** | Requirements Traceability Matrix: mapping requisito → test |
| **DRD** | Document Requirements Definition: quali doc devono essere prodotti |
| **CI** | Configuration Item: unità gestita da configuration management |
| **CCB** | Configuration Control Board: approva le modifiche ai CI |
| **PDR** | Preliminary Design Review |
| **CDR** | Critical Design Review |
| **TRR** | Test Readiness Review |
| **AR** | Acceptance Review |
| **Justification File** | Documento che giustifica una deviazione da un requisito ECSS |

---

## Workflow di aggiornamento documentale

Quando viene modificato del codice:

```
1. Identificare il/i requisito/i impattato/i dalla modifica
   → Cercare nel TDD del progetto interessato

2. Aggiornare la sezione Requirements se la modifica introduce un nuovo comportamento
   → Aggiungere [REQ-XXX-NNN] con shall/should

3. Aggiornare la sezione Architecture Design
   → Aggiornare component description, data flow

4. Aggiornare o aggiungere test case nella sezione V&V
   → [TC-XXX-NNN] corrispondente al nuovo requisito

5. Aggiornare la RTM
   → Aggiungere riga: REQ-ID | Design section | Implementation file | TC-ID | Status

6. Aggiornare il Change Log in fondo al documento
   → Nuova entry: Issue | Rev | Date | Description of Change | Author

7. Incrementare il numero di Revision del documento
   → Issue rimane uguale, Revision incrementa (es. 1.0 → 1.1)
   → Issue incrementa solo dopo una review formale (PDR, CDR)
```

---

## Note di compliance ECSS per questo progetto

**Livello di compliance adottato: ECSS-inspired (Level 2)**

Il progetto **MPQUIC/STRIPES** adotta uno stile documentale **ECSS-inspired** — non formal ECSS compliance (che richiederebbe CCB, audit ESA, review board formali). Questo livello è appropriato per un progetto industriale SATCOM che vuole struttura e tracciabilità senza il costo di un processo ECSS completo.

**Standard di riferimento applicati:**
- ECSS-E-ST-40C (Software Engineering) — per struttura TDD, SRD, test
- ECSS-Q-ST-80 (Software Product Assurance) — per qualità e RTM
- ECSS-M-ST-40 (Configuration Management) — per document control e versioning

**Standard NON applicati (fuori scope):**
- ECSS-M-ST-10 (Project Planning) — gestito via ROADMAP + JIRA/Git
- Formal review boards (PDR, CDR) — sostituiti da peer review via Git PR
