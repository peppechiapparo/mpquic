# SKILL: Code Quality Metrics — ECSS / SonarQube

> **Skill owner**: Tech Lead tbox  
> **Versione**: 1.0.0  
> **Ultima revisione**: 2026-05-14

---

## Scopo

Questa skill obbliga ogni agente che scrive codice (Python, TypeScript, JavaScript)
a rispettare le metriche di qualità ECSS e a verificarle tramite SonarLint (VS Code)
e SonarQube (full scan) prima di considerare un task completato.

---

## Infrastruttura SonarQube locale

| Elemento | Valore |
|----------|--------|
| Server URL (via IP container) | `http://172.30.0.3:9000` |
| Admin user | `admin` |
| Admin password | (in secrets — non nel codice) |
| Scanner token name | `tbox-scanner` |
| sonar-scanner binary | `/opt/TPZ/tools/sonar-scanner/bin/sonar-scanner` |
| Quality Gate attivo | `ECSS-Gate` |

**SonarLint in VS Code**: connesso automaticamente in Connected Mode al server locale.
Il pannello **Problems** (Ctrl+Shift+M) mostra le issue in tempo reale mentre scrivi.

---

## Metriche ECSS obbligatorie

### Python (tmon, pumbaa, collector)

| Metrica | Target | Limite assoluto |
|---------|--------|-----------------|
| Complessità ciclomatica McCabe per funzione | ≤ 10 | 20 |
| Cognitive Complexity (SonarQube) per funzione | ≤ 10 | 15 |
| Max nesting level | ≤ 3 | 4 |
| LOC per funzione (SLOC) | ≤ 50 | 100 |
| Rapporto commenti / codice | 20% – 30% | — |
| Maintainability Index | ≥ 65 (ottimo) | < 20 = critico |
| CBO (Coupling Between Objects) | basso | — |
| DIT (Depth of Inheritance Tree) | ≤ 4 | — |

### TypeScript / JavaScript (EDGE portal, data_usage)

| Metrica | Target | Limite assoluto |
|---------|--------|-----------------|
| Cognitive Complexity per funzione | ≤ 10 | 15 |
| Max nesting level | ≤ 3 | 4 |
| LOC per funzione | ≤ 50 | 100 |
| Duplicazioni | < 3% per file | — |

---

## Costrutti vietati

### Python
```python
# VIETATO: goto (non esiste in Python nativo ma non usare librerie che lo implementano)
# VIETATO: multiple return points (usa singolo return alla fine)
# VIETATO: deep nesting > 3 livelli
# VIETATO: funzioni > 100 LOC
# VIETATO: magic numbers senza costanti nominate

# CORRETTO: singolo return
def get_status(code: int) -> str:
    result = "unknown"
    if code == 0:
        result = "ok"
    elif code == 1:
        result = "error"
    return result  # singolo punto di uscita
```

### TypeScript
```typescript
// VIETATO: any implicito, eval(), Function()
// VIETATO: callback nesting > 3 livelli — usare async/await
// VIETATO: var — usare const/let
```

---

## Workflow obbligatorio per ogni agente

### Passo 1 — Scrivi il codice

Implementa la feature seguendo le metriche ECSS sopra.

### Passo 2 — Controlla SonarLint (VS Code)

1. Apri il pannello **Problems** (`Ctrl+Shift+M`)
2. Filtra per `SonarLint`
3. Risolvi **tutti** i problemi `Critical` e `Blocker` prima di continuare
4. Per `Major` e `Minor`: documenta nella PR se non risolvi

### Passo 3 — Full scan con sonar-scanner

```bash
# Da root del sotto-progetto (es: tmon/, pumbaa/, edge/, data_usage/)
/opt/TPZ/tools/sonar-scanner/bin/sonar-scanner

# Oppure con override esplicito:
/opt/TPZ/tools/sonar-scanner/bin/sonar-scanner \
  -Dsonar.host.url=http://172.30.0.3:9000 \
  -Dsonar.token=<TOKEN>
```

### Passo 4 — Verifica Quality Gate

Vai su `http://172.30.0.3:9000` → progetto → verifica che ECSS-Gate sia **Passed**.

Condizioni ECSS-Gate:
- `new_reliability_rating` ≤ A (nessun bug su nuovo codice)
- `new_security_rating` ≤ A (nessuna vulnerability su nuovo codice)
- `new_maintainability_rating` ≤ A
- `new_duplicated_lines_density` ≤ 3%
- `new_coverage` ≥ 80% (dove esistono test)
- `new_security_hotspots_reviewed` = 100%
- `new_violations` = 0

### Passo 5 — Report nel task summary

Nel riepilogo del task includere:
```
🔍 SonarQube: Gate [PASSED/FAILED]
   - Bugs: X | Vulnerabilities: X | Code Smells: X
   - Complexity: max XX (target ≤10)
   - Coverage: XX%
```

---

## Interpretare i risultati SonarQube

### Severity levels
| Livello | Azione richiesta |
|---------|-----------------|
| 🔴 Blocker | **Blocca il deploy** — fix obbligatorio |
| 🔴 Critical | Fix obbligatorio prima della review |
| 🟠 Major | Fix fortemente raccomandato |
| 🟡 Minor | Fix se nel contesto della modifica |
| ℹ️ Info | Nota informativa |

---

## Strumenti di supporto locali

### Verifica complessità ciclomatica Python (radon)
```bash
cd /opt/TPZ/src/tbox
source .venv/bin/activate

# Cyclomatic complexity (A=1-5, B=6-10, C=11-15, D=16-20, E=21-25, F=25+)
radon cc tmon/usr/libexec/tmon_core.py -a -s

# Maintainability Index
radon mi tmon/usr/libexec/tmon_core.py -s

# Raw metrics (LOC, SLOC, comments)
radon raw tmon/usr/libexec/tmon_core.py
```

---

## File di configurazione scanner

| Progetto | File | Chiave |
|----------|------|--------|
| tmon + pumbaa | `tmon/sonar-project.properties` | `tbox-tmon` |
| pumbaa standalone | `pumbaa/sonar-project.properties` | `tbox-pumbaa` |
| EDGE portal | `edge/sonar-project.properties` | `tbox-edge` |
| data_usage | `data_usage/sonar-project.properties` | `tbox-data-usage` |
