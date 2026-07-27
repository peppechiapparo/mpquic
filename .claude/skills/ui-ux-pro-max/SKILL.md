# SKILL: ui-ux-pro-max

## Descrizione

Questa skill fornisce il **design system di riferimento** per il progetto NOVA EDGE
e guida l'implementazione di qualsiasi componente UI in modo coerente con l'identità
visiva Telespazio.

Attivare questa skill **ogni volta** che un agente deve:
- Creare o modificare componenti React / Tailwind CSS
- Aggiungere una nuova pagina al portale EDGE
- Revisionare o correggere lo stile visivo di componenti esistenti
- Decidere colori, tipografia, spacing o iconografia

---

## Design System del progetto

Il design system permanente è in:
```
edge/design-system/nova-edge/MASTER.md
```

**REGOLA:** Prima di scrivere qualsiasi CSS o componente, leggere `edge/design-system/nova-edge/MASTER.md`.  
Se esiste `edge/design-system/nova-edge/pages/[nome-pagina].md`, quel file ha priorità sul MASTER per quella pagina specifica.

---

## Come usare la skill (workflow operativo)

### Caso 1 — Implementare un componente UI (uso tipico)

```
1. Leggi edge/design-system/nova-edge/MASTER.md
2. Verifica se esiste edge/design-system/nova-edge/pages/[pagina].md
3. Implementa rispettando palette, tipografia e spacing definiti
4. Non introdurre colori o font non presenti nel MASTER
```

### Caso 2 — Nuova sezione/pagina senza override specifico

Generare un override specifico per quella pagina prima di implementare:

```bash
cd /opt/TPZ/src/tbox/edge
python3 .github/prompts/ui-ux-pro-max/scripts/search.py \
  "fleet management dashboard dark satcom monitoring" \
  --design-system --persist -p "NOVA EDGE" --page "[nome-pagina]"
```

Il comando crea `edge/design-system/nova-edge/pages/[nome-pagina].md`.

---

## Parametri stack del progetto

| Parametro | Valore |
|-----------|--------|
| Stack | Next.js 16 App Router · React 19 · TypeScript |
| CSS | Tailwind CSS v4 |
| Componenti | Radix UI primitives + lucide-react icons |
| Tema default | **Dark** (con toggle light) |
| Mood | Dark dashboard · industrial · monitoraggio critico |
| Industry | SATCOM · fleet management · infrastruttura critica |

---

## Regole di stile obbligatorie (estratto dal MASTER)

> **Fonte autoritativa:** `edge/design-system/nova-edge/MASTER.md`
> Le regole sotto sono un estratto rapido. In caso di conflitto, il MASTER prevale.

### Colori primari (dark mode)
- Background: `#212631` — canvas body
- Card/Surface: `#2a303d` — sidebar, card, table header
- Primary/CTA: `#e60028` — Rosso Telespazio — badge attivi, bottoni primari, link
- Testo primario: `#f0f0ee`
- Testo muted: `#9094a0` — label, caption

### Anti-pattern da evitare (sempre)
- ❌ Sfondo nero puro `#000` o bianco puro `#fff` in dark mode
- ❌ Bordi con contrasto inferiore a 3:1 su background scuro
- ❌ Animazioni CSS su elementi di dati in tempo reale (CPU overhead)
- ❌ Font sans-serif non monospace per valori numerici in tabelle
- ❌ Colori status non standardizzati (usare sempre: verde `#22c55e`, giallo `#f59e0b`, rosso `#e60028`, grigio `#9094a0`)

### Spacing e layout
- Grid: 12 colonne, gap `1rem` (16px)
- Card padding: `p-4` (16px) o `p-6` (24px) per card grandi
- Sidebar width: `w-64` (256px) fixed
- Tabella: `text-sm`, row height `h-10`, header `bg-[#2a303d]`

### Tipografia
- Font UI: **Inter** (headings + body)
- Font dati numerici: **JetBrains Mono** (valori tabelle, metriche)
- Heading principale: `text-2xl font-semibold`
- Label/caption: `text-xs text-muted-foreground`

---

## Dove sono i file della skill

```
edge/
├── .github/prompts/ui-ux-pro-max/   ← tool uipro-cli (script Python + dataset)
│   ├── PROMPT.md                    ← istruzioni complete del tool
│   ├── scripts/
│   │   ├── search.py                ← entry point CLI
│   │   ├── design_system.py
│   │   └── core.py
│   └── data/                        ← dataset CSV (colori, stili, UX, stacks...)
│
└── design-system/nova-edge/         ← output del tool (source of truth)
    ├── MASTER.md                    ← Design System globale NOVA EDGE ★
    └── pages/                       ← Override specifici per pagina
```

---

## Istruzioni per gli agenti sviluppatori

Ogni agente (`developer`, `luci-developer`) che implementa componenti UI deve includere nel proprio workflow:

```
PRIMA di implementare qualsiasi componente UI:
1. Leggi /opt/TPZ/src/tbox/edge/design-system/nova-edge/MASTER.md
2. Verifica override in edge/design-system/nova-edge/pages/[pagina].md
3. Rispetta palette, spacing e anti-pattern definiti
4. Non introdurre colori, font o stili non presenti nel design system
```
