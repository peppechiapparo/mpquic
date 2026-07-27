# Prompt: Configura Agent Team Claude da .github personalizzato

Usa questo prompt su qualsiasi progetto che ha già una cartella `.github/agents/` e `.github/skills/` personalizzata per GitHub Copilot, per generare la versione equivalente in `.claude/` ottimizzata per Claude Code.

---

## Prompt da usare (copia e incolla a Claude)

```
Ho un progetto con una cartella `.github/agents/` e `.github/skills/` configurata per GitHub Copilot.
Voglio creare una cartella `.claude/` separata con agenti e skills identici ma ottimizzati per Claude Code,
in modo da poter lavorare con GitHub usando gli agenti in `.github/` e con Claude Code usando quelli in `.claude/`.

Per favore:

1. **Leggi tutti i file** in `.github/agents/*.agent.md` e `.github/skills/*/SKILL.md`
   (e tutti i file di supporto in `.github/skills/vapt/` se presenti)

2. **Crea la struttura `.claude/`** con:
   - `.claude/agents/` — un file `.md` per ogni agente
   - `.claude/skills/` — copie delle skill directories con tutti i file di supporto

3. **Per ogni agente, adatta il frontmatter YAML**:
   - Rimuovi il campo `model:` esistente (che usa nomi GitHub Copilot) e sostituiscilo con il modello Claude appropriato:
     - Task leggeri/procedurali (git, deploy, operazioni semplici) → `model: claude-haiku-4-5`
     - Task di implementazione/sviluppo (coding, sysadmin, testing) → `model: claude-sonnet-4-6`
     - Task di alta qualità (planning, review, security audit, orchestrazione, reporting) → `model: claude-opus-4-8`
   - Aggiorna il campo `tools:` usando i tool di Claude Code: `[Bash, Read, Edit, Write, WebFetch, WebSearch, Agent, TodoWrite]`
     - Ogni agente dovrebbe avere solo i tool che effettivamente usa
     - Agenti leggeri (git-ops, deploy-ops): solo `[Bash, Read]`
     - Agenti di implementazione: `[Bash, Read, Edit, Write, WebFetch, Agent, TodoWrite]`
     - Agenti di analisi (planner, reviewer, security): `[Bash, Read, WebFetch, WebSearch, Agent]`
     - Orchestratori (tech-lead, vapt-coordinator): tutti i tool

4. **Aggiorna i path** nei body degli agenti:
   - `.github/skills/` → `.claude/skills/`
   - `.github/agents/` → `.claude/agents/`
   - Riferimenti a `runSubagent` tool → usa il tool `Agent` di Claude Code

5. **Correggi eventuali artefatti** (testo spurio prima del frontmatter, typo, ecc.)

6. **Per le skills**: copia i file di contenuto identici ma aggiorna i riferimenti a path interni.

7. **Crea il file `.claude/AGENT-TEAM-SETUP-PROMPT.md`** con questo stesso prompt, per poterlo riutilizzare in futuro su altri progetti.

8. **Leggi `.vscode/settings.json`** (se esiste) e replica in `.claude/settings.json` le autorizzazioni
   già concesse agli agenti GitHub Copilot tramite `chat.tools.terminal.autoApprove`.
   - Per ogni comando in `chat.tools.terminal.autoApprove` con valore `true`, aggiungi la regola
     `"Bash(<comando> *)"` nell'array `permissions.allow` di `.claude/settings.json`.
   - Se `.claude/settings.json` non esiste, crealo con la struttura minima:
     ```json
     {
       "permissions": {
         "allow": [
           "Bash(<comando1> *)",
           "Bash(<comando2> *)"
         ]
       }
     }
     ```
   - Dopo la scrittura, verifica la sintassi con: `jq -e '.permissions.allow[]' .claude/settings.json`

Mantieni tutto il contenuto degli agenti in italiano (o nella lingua originale) e preserva
tutta la conoscenza di dominio specifica del progetto nei body degli agenti.
Non modificare i runbook operativi, le convenzioni di codice, le checklist o le istruzioni specifiche del progetto.
```

---

## Criteri di selezione modello

Usa questa tabella come riferimento quando assegni i modelli:

| Tipo di agente | Modello | Razionale |
|---------------|---------|-----------|
| Operatori leggeri (git, deploy, routine ops) | `claude-haiku-4-5` | Task procedurali ben definiti, non richiedono ragionamento avanzato |
| Sviluppatori (full-stack, Python, LuCI, sysadmin) | `claude-sonnet-4-6` | Implementazione codice, buon bilanciamento qualità/costo |
| Tester, reviewer, ecss-guardian | `claude-sonnet-4-6` | Review e testing, qualità media-alta sufficiente |
| Planner, security expert, orchestratori VAPT | `claude-opus-4-8` | Ragionamento complesso, analisi di rischio, pianificazione architetturale |
| Tech lead (coordinamento dell'intero team) | `claude-opus-4-8` | Orchestrazione, decisioni architetturali, supervisione qualità |

---

## Tool mapping GitHub Copilot → Claude Code

| Tool GitHub Copilot | Tool Claude Code equivalente |
|---------------------|------------------------------|
| `codebase`, `search/codebase` | `Bash` (grep/find) + `Agent` (Explore) |
| `editFiles`, `edit/editFiles` | `Edit` |
| `runCommands`, `execute/runInTerminal` | `Bash` |
| `fetch`, `web/fetch` | `WebFetch` |
| `readFile`, `read/readFile` | `Read` |
| `edit/createFile`, `edit/createDirectory` | `Write`, `Bash` |
| `runSubagent`, `agent/runSubagent` | `Agent` |
| `web/githubTextSearch` | `WebSearch` |
| `findTestFiles`, `usages`, `problems` | `Bash` |
| VS Code-specific tools (vscode/*, browser/*, pylance-*) | Non applicabili — omettere |

---

## Configurazione `.claude/settings.json` — Permessi auto-approvati

Claude Code usa `.claude/settings.json` (a livello di progetto) per definire quali comandi Bash
vengono approvati automaticamente senza prompt interattivo — equivalente di `chat.tools.terminal.autoApprove`
in VS Code per GitHub Copilot.

### Mapping VS Code → Claude Code

| VS Code (`chat.tools.terminal.autoApprove`) | Claude Code (`.claude/settings.json`) |
|---------------------------------------------|---------------------------------------|
| `"ssh": true` | `"Bash(ssh *)"` nell'array `permissions.allow` |
| `"scp": true` | `"Bash(scp *)"` nell'array `permissions.allow` |
| `"docker": true` | `"Bash(docker *)"` nell'array `permissions.allow` |
| `"ansible-playbook": true` | `"Bash(ansible-playbook *)"` nell'array `permissions.allow` |

### Struttura `.claude/settings.json` per questo progetto

```json
{
  "permissions": {
    "allow": [
      "Bash(ssh *)",
      "Bash(scp *)"
    ]
  }
}
```

**Sintassi regole Bash:**
- `"Bash(ssh *)"` — approva qualsiasi comando che inizia con `ssh ` (con spazio)
- `"Bash(ssh)"` — approva solo il comando `ssh` esatto senza argomenti (quasi mai utile)
- Il pattern usa glob: `*` matcha qualsiasi cosa dopo il prefisso

**Scope del file:**
- `.claude/settings.json` → va in git, si applica a tutto il team
- `.claude/settings.local.json` → non va in git, solo locale

**Verifica dopo la scrittura:**
```bash
jq -e '.permissions.allow[]' .claude/settings.json
```
Deve stampare le regole senza errori (exit 0).

---

## Note importanti

- La cartella `.github/` rimane **invariata** per continuare a usare gli agenti con GitHub Copilot
- La cartella `.claude/` è **indipendente** e usata solo con Claude Code
- I due team possono evolvere separatamente nel tempo
- Se un agente ha modelli multipli nel frontmatter originale (fallback), scegli il modello principale
- Preserva tutte le istruzioni operative, runbook, convenzioni e knowledge di dominio esattamente come sono
