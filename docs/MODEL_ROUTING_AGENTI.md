# Model Routing per Agenti — Cost Optimization

Guida per assegnare modelli AI diversi ai vari agenti specializzati di un progetto VS Code Copilot, riducendo il consumo di token (e costo) senza perdere qualità sui task critici.

## TL;DR — Cosa supporta VS Code

| Feature                                  | Supportato? | Come |
|------------------------------------------|-------------|------|
| Modello diverso per agente               | ✅ Sì       | Campo `model:` nel frontmatter di `*.agent.md` |
| Lista di fallback se modello non disponibile | ✅ Sì    | Campo `model:` come array (primo disponibile vince) |
| Modello diverso per task dentro lo stesso agente | ❌ No  | Workaround: subagent dedicati invocati via tool `agent` |
| Routing automatico basato sul tipo di task | ❌ No     | Workaround: istruzione esplicita nel system prompt del parent |

## 1. Frontmatter `model:` per agente

Apri `.github/agents/<agente>.agent.md` e aggiungi nel frontmatter YAML il campo `model:`:

```yaml
---
description: "Descrizione del ruolo"
model: ["Claude Sonnet 4.6 (copilot)", "GPT-5 (copilot)", "Claude Opus 4.7 (copilot)"]
tools: [...]
---
```

**Sintassi**:
- Stringa singola: `model: "Claude Opus 4.7 (copilot)"`
- Array fallback: `model: ["Claude Sonnet 4.6 (copilot)", "GPT-5 (copilot)"]` — VS Code prova in ordine e usa il primo disponibile per l'utente.

**Nomi modelli**: usa esattamente il nome che vedi nel model picker di VS Code Copilot, es:
- `Claude Sonnet 4.6 (copilot)`
- `Claude Opus 4.7 (copilot)`
- `GPT-5 (copilot)`
- `GPT-4.1 (copilot)`
- `GPT-5 mini (copilot)`
- `Claude Haiku 4 (copilot)`

## 2. Strategia di assegnazione consigliata

| Tipo di lavoro            | Modello consigliato       | Perché |
|---------------------------|---------------------------|--------|
| Design / architettura     | Claude Opus 4.7           | Una sola volta per feature, ma critico |
| Security audit            | Claude Opus 4.7           | Non si compromette sulla sicurezza |
| Implementazione codice    | Claude Sonnet 4.6         | Bilanciato qualità/costo |
| Code review               | Claude Sonnet 4.6         | Review logica, basso rischio |
| Scrittura test            | Claude Sonnet 4.6 / GPT-5 | Procedurale |
| Comandi git procedurali   | GPT-4.1 / Haiku           | Banale, ~10 token in/out |
| Deploy, restart, scp      | GPT-4.1 / Haiku           | Banale, output strutturato |
| Conversazione/orchestrazione | Sonnet                 | Coordina ma non scrive codice |

## 3. Delega programmatica con il tool `agent` (runSubagent)

VS Code supporta delega **programmatica** tra agenti tramite il tool `agent/runSubagent`.
Quando un orchestratore lo chiama, VS Code apre una sotto-conversazione isolata con il
subagent scelto, che gira sul **suo** modello (quello del suo frontmatter, non del padre).
Il padre riceve solo il **summary finale** — il contesto operativo del subagent non scala
sul modello costoso dell'orchestratore. Questo è il risparmio token più concreto.

### Prerequisito fondamentale

Il tool `agent/runSubagent` **deve essere presente nella lista `tools:` del padre**.
Senza di esso, la delega programmatica non funziona — il padre può solo menzionare
`@git-ops` nel testo, ma non invocarlo come subagent isolato.

```yaml
# Frontmatter dell'orchestratore — "agent" abilita la delega
tools: ["agent", "codebase", "editFiles", ...]
```

### Chi ha il tool `agent` in questo repo

| Agente              | Ha `agent` in tools? | Perché |
|---------------------|----------------------|--------|
| `tech-lead`         | ✅ Sì               | Orchestratore principale |
| `developer`         | ✅ Sì               | Delega git dopo implementazione |
| `python-developer`  | ✅ Sì               | Delega git/deploy dopo modifiche Python |
| `git-ops`           | ❌ No               | Foglia — non orchestra nessuno |
| `deploy-ops`        | ❌ No               | Foglia — non orchestra nessuno |
| `planner`           | ❌ No               | Foglia — produce piani, non delega |
| `reviewer`          | ❌ No               | Foglia |
| `tester`            | ❌ No               | Foglia |
| `security-nis2`     | ❌ No               | Foglia |
| `openwrt-sysadmin`  | ❌ No               | Foglia |
| `transport-expert`  | ❌ No               | Foglia |

**Regola**: solo gli orchestratori che devono invocare subagent hanno `"agent"` in tools.
I subagent foglie **non** lo devono avere — evita catene di deleghe infinite.

### Subagent disponibili

- `.github/agents/git-ops.agent.md` — GPT-4.1, solo operazioni git
- `.github/agents/deploy-ops.agent.md` — GPT-4.1, solo scp/restart/journalctl

## 4. Replicare su altri progetti

```bash
# 1. Copia gli agent template
mkdir -p <nuovo-progetto>/.github/agents/
cp /opt/TPZ/src/mpquic/.github/agents/git-ops.agent.md <nuovo-progetto>/.github/agents/
cp /opt/TPZ/src/mpquic/.github/agents/deploy-ops.agent.md <nuovo-progetto>/.github/agents/

# 2. Aggiungi `model:` ai frontmatter dei tuoi agenti esistenti
#    Edita ogni *.agent.md e aggiungi la riga model dopo description.

# 3. Nel tech-lead/orchestrator del progetto, aggiungi la regola:
#    "Per git → delega a @git-ops. Per deploy → delega a @deploy-ops."
```

## 5. Verifica

1. Apri VS Code, carica il progetto
2. Apri la chat e seleziona un agente custom (es. `@developer`)
3. Sotto al campo prompt vedi il modello attivo: deve essere quello indicato nel frontmatter
4. Se vedi un modello diverso, controlla:
   - Sintassi YAML del frontmatter (no tab, virgolette ok)
   - Nome modello esatto come nel picker
   - Modello disponibile nel tuo plan Copilot

## 6. Limiti noti

- Il model selector dell'UI ha priorità rispetto al frontmatter — se l'utente cambia manualmente, lo override vince.
- Modelli "free" (es. GPT-4o mini) potrebbero non essere accessibili a tutti gli account Copilot.
- L'array di fallback funziona solo se il primo modello non è disponibile (non si aggira il rate-limit).

## 7. Riferimenti

- VS Code Custom Agents: https://code.visualstudio.com/docs/copilot/customization/custom-agents
- Frontmatter completo: skill `agent-customization` interna a VS Code
