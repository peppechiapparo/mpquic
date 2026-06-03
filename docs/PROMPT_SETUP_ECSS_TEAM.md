# Prompt — Setup ECSS Documentation per nuovo progetto

> **Uso:** Fornire questo prompt al tech-lead di un nuovo progetto per replicare
> l'infrastruttura di documentazione ECSS già presente in tbox/NOVA EDGE.
> Compilare le variabili tra `[...]` prima di usarlo.

---

## PROMPT DA FORNIRE AL TECH-LEAD

---

Sei il Tech Lead del progetto **[MPQUIC]** di **[TELESPAZIO]**.

Devo che tu mi configuri l'infrastruttura completa di documentazione tecnica ECSS per questo repository.  
Segui le istruzioni esatte sotto. Non improvvisare strutture alternative.

---

### CONTESTO DEL PROGETTO

- **Nome progetto:** `[MPQUIC]` (es. `NOVA RADAR`, `SATLINK`, `GROUNDSEG`)
- **Acronimo progetto (3–5 lettere, usato negli ID ECSS):** `[ACRONIMO]` (es. `EDGE`, `TMON`, `DAUS`)
- **Descrizione sintetica:** `[UNA FRASE — es. "Sistema di monitoring traffico satellitare a bordo di navi commerciali"]`
- **Classificazione NIS2:** `[SI / NO — se l'infrastruttura è critica ai sensi della Direttiva NIS2 (EU) 2022/2555]`
- **Root del repository:** `[PATH_ASSOLUTO]` (es. `/opt/TPZ/src/mioprogetto`)

**Sotto-progetti/componenti** (elenca tutti quelli che hanno codice separato):

| Componente | Descrizione breve | Linguaggio/Framework | Cartella sorgente |
|------------|------------------|----------------------|-------------------|
| `[COMP1]`  | [es. "Portal web fleet management"] | [es. Next.js 16 + PostgreSQL] | `[cartella/]` |
| `[COMP2]`  | [es. "Daemon conteggio traffico"]    | [es. Python 3 stdlib-only]    | `[cartella/]` |
| `[COMP3]`  | [es. "Dashboard locale TBOX"]        | [es. Shell + vanilla JS]      | `[cartella/]` |

---

### COSA DEVI CREARE

#### 1. Skill ECSS — `.github/skills/ecss-documentation-writer/SKILL.md`

Crea il file `.github/skills/ecss-documentation-writer/SKILL.md` con questo contenuto esatto (sostituendo i placeholder):

```markdown
# SKILL: ecss-documentation-writer

## Descrizione

Questa skill guida la **generazione e il mantenimento di documentazione tecnica
conforme agli standard ECSS** per il progetto [MPQUIC] di [TELESPAZIO].

Attivare questa skill ogni volta che si deve:
- Creare un nuovo documento tecnico di progetto
- Aggiornare documentazione esistente a seguito di modifiche software/sistemistiche
- Revisionare la conformità ECSS di un documento esistente
- Creare o aggiornare la Requirements Traceability Matrix (RTM)
- Aggiornare Justification Files

---

## Contesto del progetto

[DESCRIZIONE_1_2_RIGHE_CONTESTO_OPERATIVO]
[Se NIS2: "Opera in contesto critico ai sensi della Direttiva NIS2 (EU) 2022/2555 e richiede documentazione ingegneristica strutturata secondo ECSS."]

| Progetto | Cartella docs | Document ID principale |
|----------|--------------|------------------------|
[INSERISCI RIGA PER OGNI COMPONENTE: | `[COMP]` | `[comp]/docs/` | `TPZ-[ACRONIMO]-TDD-001` |]

---

## Regole tassative di drafting ECSS

### R1 — Sintassi dei requisiti

Ogni requisito **DEVE** rispettare:

1. **Verbo "shall"** per requisiti vincolanti:
   > `[REQ-SW-001] The system shall log all configuration changes with timestamp and user identifier.`

2. **Verbo "should"** per raccomandazioni:
   > `[REQ-SW-002] The system should complete a data cycle within 60 seconds for up to 100 nodes.`

3. **Verbo "may"** per opzioni permesse:
   > `[REQ-SW-003] The system may display historical charts with configurable time ranges.`

4. **Termini BANDITI** (ambiguità non accettabile):
   - ❌ adeguato, sufficiente, appropriato, ottimizzato, rapido, veloce
   - ❌ normalmente, generalmente, tipicamente
   - ❌ e/o, ecc., etc.
   - ❌ minimo/massimo senza valore numerico
   - ✅ Sostituire con: `within 500 ms`, `at least 99.5%`, `no more than 5 sessions`

### R2 — Identificazione requisiti

Pattern obbligatorio:

```
[REQ-<CATEGORIA>-<NNN>]
```

Categorie standard (adattare al progetto):

| Categoria | Descrizione |
|-----------|-------------|
| `SW`      | Requisiti software funzionali |
| `SEC`     | Sicurezza e autenticazione |
| `PERF`    | Performance e tempi di risposta |
| `INT`     | Interfacce esterne e integrazioni |
| `CONF`    | Configurazione e parametri operativi |
| `OPS`     | Operatività e manutenibilità |
| `UI`      | Interfaccia utente |

### R3 — Struttura obbligatoria del TDD (Template)

Ogni Technical Design Document deve avere esattamente queste 9 sezioni:

```
1. Document Information
2. Introduction
3. Applicable Documents & References
4. System Overview
5. Requirements
   5.1 [Categoria 1] Requirements
   5.2 [Categoria 2] Requirements
   ...
6. Architecture Design
   6.1 Component Overview
   6.2 Data Flow
   6.3 Technology Stack
7. Interface Definition
   7.1 Internal Interfaces
   7.2 External Interfaces
8. Verification & Validation
   8.1 Test Cases
   8.2 Acceptance Criteria
9. Requirements Traceability Matrix (RTM)
```

**Sezione 1 — Document Information (template):**

```markdown
| Campo | Valore |
|-------|--------|
| Document ID | TPZ-[ACRONIMO]-TDD-001 |
| Title | [NOME COMPONENTE] — Technical Design Document |
| Issue | 1 |
| Revision | 0 |
| Status | Draft |
| Date | [DATA ISO 8601] |
| Author | [TELESPAZIO] Engineering Team |
| Project | [MPQUIC] |
| Classification | Internal / Restricted |
| Standard ref. | ECSS-E-ST-40C, ECSS-Q-ST-80C |
```

**Sezione 9 — RTM (template):**

```markdown
| REQ-ID | Titolo requisito | Sezione | Implementazione | Test Case | Status |
|--------|-----------------|---------|-----------------|-----------|--------|
| [REQ-SW-001] | [titolo] | 5.1 | `file.py:funzione()` | [TC-NNN] | ✅ Verificato |
```

### R4 — Versionamento documento

- Ogni modifica: incrementare **Revision** (Issue 1 Rev 0 → Issue 1 Rev 1)
- Breaking change architetturale: incrementare **Issue** (Issue 1 → Issue 2, Revision reset a 0)
- Aggiornare sempre il **Change Log** in fondo al documento

### R5 — Dove si trovano i documenti ECSS

I documenti ECSS sono versionati **insieme al codice sorgente del componente**:

```
[COMP1]/docs/TPZ-[ACRONIMO1]-TDD-001.md
[COMP2]/docs/TPZ-[ACRONIMO2]-TDD-001.md
...
```

**Non** creare cartelle `docs/ecss/` o `docs/tdd/` separate: ogni componente possiede la propria documentazione.

### R6 — Regola cross-componente

Se due o più componenti formano un **subsistema strettamente integrato** (IPC diretto, stesso processo procd, stessa configurazione UCI), è accettabile un **unico TDD** che copre entrambi, posizionato nel componente principale (es. `tmon/docs/TPZ-TMON-TDD-001.md` copre sia tmon che pumbaa). Il componente secondario avrà solo un `docs/README.md` con puntatori.

---

## Checklist di conformità (da eseguire dopo ogni aggiornamento)

```
□ Tutti i requisiti hanno formato [REQ-XXX-NNN] ... shall/should/may ...?
□ Nessun termine vago (adeguato, rapido, ottimizzato)?
□ Ogni requisito è atomico e verificabile?
□ La RTM copre tutti i requisiti nuovi/modificati?
□ Il Change Log è aggiornato con data e descrizione?
□ La Revision è stata incrementata?
□ Il campo Status è corretto (Draft / Released)?
```

---

## SEGNALAZIONE VIOLAZIONI

Se una modifica al codice **viola** un requisito ECSS preesistente, segnalare sempre con:

```
⚠️ ECSS COMPLIANCE ALERT
Requisito violato: [REQ-XXX-NNN]
Testo: "The system shall ..."
File modificato: path/to/file
Impatto: [descrizione]
Azione richiesta: aggiornare il requisito / correggere il codice / aprire Justification File
```
```

---

#### 2. Agente ECSS Guardian — `.github/agents/ecss-guardian.agent.md`

Crea il file `.github/agents/ecss-guardian.agent.md` con questo contenuto (sostituendo i placeholder):

```markdown
---
description: "Scrittore e guardiano della documentazione tecnica ECSS per il progetto [MPQUIC]. Mantiene i documenti TDD, SRD, RTM e UM conformi agli standard ECSS-E-ST-40C e ECSS-Q-ST-80 ad ogni modifica software o sistemistica."
model: ["Claude Sonnet 4.6 (copilot)", "GPT-5 (copilot)"]
tools: ["codebase", "editFiles", "fetch", "findTestFiles", "githubRepo", "problems", "readFile", "runCommands", "usages"]
---

# ECSS Guardian — Documentazione Tecnica ECSS

Sei il **guardiano della documentazione tecnica ECSS** per il progetto **[MPQUIC]** di **[TELESPAZIO]**.
Il tuo ruolo è **creare, aggiornare e verificare** che la documentazione tecnica sia sempre
allineata al codice e conforme agli standard ECSS, ogni volta che viene richiesto.

**PRIMA DI INIZIARE** qualsiasi attività documentale, leggi la skill guida:
`[PATH_ASSOLUTO]/.github/skills/ecss-documentation-writer/SKILL.md`

---

## Contesto del progetto

[DESCRIZIONE_ESTESA_2_3_RIGHE — cosa fa il sistema, per chi, in quale dominio operativo]

### Struttura documentale

```
[COMP1]/docs/
├── TPZ-[ACRO1]-TDD-001.md   ← TDD ECSS (Issue 1, Rev 0)
├── ARCHITECTURE.md
├── ROADMAP.md
└── ...

[COMP2]/docs/
├── TPZ-[ACRO2]-TDD-001.md
└── ...

docs/                        ← Solo documenti cross-progetto
├── CHANGELOG.md
├── WORKING_INSTRUCTIONS.md
├── SECURITY_NIS2.md
└── ...
```

### Mapping progetto → documento ECSS

| Progetto | Document ID | File |
|----------|-------------|------|
[UNA RIGA PER OGNI COMPONENTE CON TDD:]
| [COMP1] | `TPZ-[ACRO1]-TDD-001` | `[COMP1]/docs/TPZ-[ACRO1]-TDD-001.md` |

---

## Ciclo di vita operativo

### Trigger di attivazione

Intervieni in questi casi:
1. Il Tech Lead ti delega esplicitamente dopo un commit/feature
2. Viene richiesta la creazione di un nuovo documento ECSS
3. Una feature modifica comportamento documentato in un TDD
4. Vengono aggiunti nuovi requisiti o nuove interfacce
5. Viene richiesta verifica di conformità documentale

### Flusso di aggiornamento

**Passo 1 — Analisi del diff/modifica:**

```
1. Quali file di codice sono stati modificati?
2. Quale componente coinvolgono?
3. Qual è l'impatto funzionale? (nuovo comportamento? interfaccia cambiata?)
4. Esiste già un [REQ-XXX-NNN] che copre questa funzionalità?
5. Serve un nuovo requisito?
```

**Passo 2 — Aggiornamento documento TDD:**

- Identifica sezione Requirements, Architecture, V&V e RTM impattati
- Aggiungi `[REQ-XXX-NNN]` con verbo `shall`/`should` per ogni nuovo comportamento
- Aggiungi `[TC-XXX-NNN]` corrispondenti nella sezione V&V
- Aggiorna la RTM (nuova riga per ogni nuovo requisito)
- Incrementa la Revision
- Aggiungi entry nel Change Log

**Passo 3 — Verifica conformità (checklist dalla skill)**

Esegui la checklist R5 dalla skill prima di terminare.

---

## Regole di scrittura

Tutte le regole sono nella skill. Applica sempre:
- Verbi shall/should/may (mai "deve", "dovrebbe", "può")
- ID univoci [REQ-XXX-NNN]
- Nessun termine vago (adeguato, rapido, sufficiente)
- Ogni requisito atomico e verificabile
- RTM aggiornata ad ogni modifica
- Revision incrementata ad ogni aggiornamento

---

## Gestione nuovi TDD (da zero)

Se richiedi la creazione di un **nuovo TDD** per un componente che non ne ha ancora uno:

1. Leggi tutto il codice sorgente del componente (`readFile`, `codebase`)
2. Identifica funzionalità, interfacce, configurazioni
3. Derivazione bottom-up: estrai i requisiti dal codice esistente (retroattivi)
4. Struttura il documento con le 9 sezioni obbligatorie (dalla skill)
5. Assegna Document ID secondo lo schema `TPZ-[ACRONIMO]-TDD-NNN`
6. Posiziona il file in `[COMP]/docs/TPZ-[ACRONIMO]-TDD-NNN.md`
7. Compila la RTM completa

**Obiettivo minimo per un TDD iniziale:**
- ≥ 15 requisiti per componenti semplici
- ≥ 30 requisiti per componenti complessi (daemon, portal, API)
- ≥ 5 test case per ogni categoria di requisiti presente
- RTM al 100% (ogni requisito tracciato)
```

---

#### 3. File WORKING_INSTRUCTIONS.md — `docs/WORKING_INSTRUCTIONS.md`

Crea (o aggiorna) il file `docs/WORKING_INSTRUCTIONS.md` con questo schema (le sezioni
obbligatorie sono indicate con ★, le opzionali con ○):

```markdown
# Istruzioni Operative — Tech Lead [MPQUIC]

**LEGGERE QUESTO DOCUMENTO PRIMA DI INIZIARE QUALSIASI SESSIONE DI LAVORO.**

---

★ ## Regola #1: Aggiornare SEMPRE la documentazione

Dopo ogni task completato, aggiornare IMMEDIATAMENTE:

| Documento | Cosa aggiornare |
|-----------|----------------|
| `docs/CHANGELOG.md` | Nuova entry: modifiche, file toccati, test eseguiti, risultati |
| `[COMP]/docs/ROADMAP.md` | Stato fasi, nuovi item se emersi |
| `[COMP]/docs/ARCHITECTURE.md` | Sezioni impattate dalle modifiche |
| `[COMP]/docs/[COMP]-TDD-001.md` | Requisiti, test case, RTM, Revision |

Non aspettare che l'utente lo chieda.

---

★ ## Regola #2: Versioning (SemVer)

- **PATCH** (x.y.Z): bug fix
- **MINOR** (x.Y.0): nuove funzionalità retrocompatibili
- **MAJOR** (X.0.0): breaking changes

| Componente | Versione corrente | File `__version__` |
|------------|------------------|--------------------|
| [COMP1] | [x.y.z] | `[path/__init__.py o main.py]` |
| [COMP2] | [x.y.z] | `[path/]` |

---

★ ## Regola #3: Procedura di deploy

[Descrivi la procedura specifica del progetto o rimanda al file DEPLOYMENT.md]

MAI deployare senza backup.

---

★ ## Regola #4: Sicurezza[, NIS2 se applicabile]

[Elenco misure di sicurezza attive e vincoli obbligatori per il progetto]

[Se NIS2:]
Questo progetto è infrastruttura critica NIS2. Ogni modifica deve essere
validata da `@security-nis2` prima del merge.

---

○ ## Regola #5: Testing

[Dove si trovano i test, come eseguirli, soglia minima di coverage]

---

★ ## Regola #[N]: Documentazione ECSS

I documenti tecnici ECSS sono **versionati insieme al codice** in ogni cartella di progetto:

| Cartella | Documento ECSS | Descrizione |
|----------|----------------|-------------|
| `[COMP1]/docs/` | TPZ-[ACRO1]-TDD-001 (Issue 1 Rev 0) | TDD per [COMP1] |
| `[COMP2]/docs/` | TPZ-[ACRO2]-TDD-001 (Issue 1 Rev 0) | TDD per [COMP2] |

**Schema Document ID:** `[TELESPAZIO_SIGLA]-[PROJ]-[TYPE]-[NNN]`
- PROJ: `[ACRO1]` | `[ACRO2]` | `SYS`
- TYPE: `TDD` | `SRD` | `UM` | `ICD` | `SEC` | `VVP` | `RTM`

**Regola:** Dopo ogni modifica software, invocare `@ecss-guardian` per aggiornare il documento ECSS di competenza.

### Struttura documentale per progetto

```
[ROOT]/
├── docs/                         ← Solo documenti cross-progetto
│   ├── CHANGELOG.md
│   ├── WORKING_INSTRUCTIONS.md   ← QUESTO FILE
│   ├── SECURITY_NIS2.md          ← [se applicabile]
│   └── MODEL_ROUTING_AGENTI.md   ← [se usi il team di agenti]
│
├── [COMP1]/docs/
│   ├── TPZ-[ACRO1]-TDD-001.md
│   ├── ARCHITECTURE.md
│   ├── ROADMAP.md
│   └── DEPLOYMENT.md
│
├── [COMP2]/docs/
│   ├── TPZ-[ACRO2]-TDD-001.md
│   └── ARCHITECTURE.md
│
└── .github/
    ├── agents/ecss-guardian.agent.md
    └── skills/ecss-documentation-writer/SKILL.md
```

---

★ ## Checklist pre-sessione

- [ ] Leggere questo documento
- [ ] Verificare roadmap: `[COMP_PRINCIPALE]/docs/ROADMAP.md`
- [ ] Verificare CHANGELOG: `docs/CHANGELOG.md`
- [ ] [Aggiungere checklist specifica del progetto]
```

---

### PASSAGGI FINALI (dopo aver creato i 3 file)

1. **Crea le cartelle `docs/` per ogni componente** che non le ha già:
   ```bash
   mkdir -p [COMP1]/docs [COMP2]/docs [COMP3]/docs
   ```

2. **Crea immediatamente il primo TDD** per il componente principale (quello con più codice):
   - Leggi tutto il codice sorgente del componente
   - Deriva i requisiti bottom-up dal codice esistente (retroattivi)
   - Usa le 9 sezioni obbligatorie della skill
   - Punta alla RTM completa al 100% (ogni requisito tracciato)
   - Salva in `[COMP_PRINCIPALE]/docs/TPZ-[ACRONIMO]-TDD-001.md`

3. **Aggiorna `docs/WORKING_INSTRUCTIONS.md`** con la struttura delle cartelle reale
   (dopo aver creato tutti i file).

4. **Testa l'agente**: chiedi all'agente `@ecss-guardian` di verificare il TDD appena creato.

---

### NOTE IMPORTANTI

- I documenti ECSS usano inglese per i requisiti (`shall`, `should`, `may`) e possono usare
  italiano per le sezioni descrittive e architetturali (approccio ibrido accettato internamente).
- Il Document ID usa il prefisso dell'TELESPAZIO: `TPZ-` per Telespazio. Sostituire con il
  prefisso della propria TELESPAZIO (es. `LEO-`, `SAT-`, `NGS-`).
- Se il progetto NON è NIS2, rimuovere tutti i riferimenti NIS2 dalla skill e dall'agente.
- Il campo `model:` nell'agente può essere omesso se si vuole usare il modello di default
  del proprio ambiente Copilot.

---

*Questo prompt è stato generato a partire dall'implementazione reale del progetto tbox/NOVA EDGE
di Telespazio (workspace: `/opt/TPZ/src/tbox`). I file di riferimento sono:*
- *`.github/skills/ecss-documentation-writer/SKILL.md`*
- *`.github/agents/ecss-guardian.agent.md`*
- *`docs/WORKING_INSTRUCTIONS.md`*
