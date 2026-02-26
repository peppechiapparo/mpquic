# Dataplane QoS e integrazione orchestrator

Questo documento definisce come configurare il dataplane multipath per QoS applicativa e come interfacciarlo con orchestrator esterno via file policy e Control API locale.

## Obiettivo

Separare:
- **config applicativa MPQUIC** (path WAN, endpoint, TLS, TUN)
- **config dataplane QoS** (classi traffico, classifier, policy scheduler)

Per ambienti orchestrati è raccomandato il file dedicato `dataplane_config_file`.

## Modelli di configurazione supportati

### 1) File dataplane dedicato (raccomandato)

Nel file applicativo client multipath:

```yaml
role: client
multipath_enabled: true
dataplane_config_file: ./dataplane.yaml
...
```

Poi in `dataplane.yaml`:

```yaml
default_class: default
classes:
  default:
    scheduler_policy: balanced
    preferred_paths: [wan4, wan5, wan6]
  critical:
    scheduler_policy: failover
    preferred_paths: [wan4, wan5]
    duplicate: true
    duplicate_copies: 2
  bulk:
    scheduler_policy: balanced
    excluded_paths: [wan4]
classifiers:
  - name: voip-sip
    class: critical
    protocol: udp
    dst_ports: ["5060", "10000-20000"]
    dscp: [46]
  - name: telemetry-control
    class: critical
    protocol: tcp
    dst_ports: ["443", "8443"]
  - name: backup-stream
    class: bulk
    protocol: tcp
    dst_ports: ["5001-6000"]
```

### 2) Dataplane inline nello YAML applicativo

Alternativa valida per POC piccoli:

```yaml
dataplane:
  default_class: default
  classes:
    ...
  classifiers:
    ...
```

### Precedenza

Se sono presenti sia `dataplane` inline sia `dataplane_config_file`, il runtime usa il file dedicato (`dataplane_config_file`).

## Schema dataplane

### `default_class`
- classe di fallback quando nessuna regola classifier matcha.

### `classes.<name>`
- `scheduler_policy`: `priority | failover | balanced`
- `preferred_paths`: lista nomi path da favorire (es. `wan4`)
- `excluded_paths`: path da escludere per la classe
- `duplicate`: abilita duplicazione datagrammi per classe
- `duplicate_copies`: copie inviate su path distinti (2..3)

### `classifiers[]`
- `name`: etichetta regola
- `class`: classe target
- `protocol`: `udp | tcp | icmp | icmpv6` (opzionale)
- `src_cidrs`, `dst_cidrs`: CIDR IPv4/IPv6 (opzionali)
- `src_ports`, `dst_ports`: porta singola (`"443"`) o range (`"10000-20000"`)
- `dscp`: lista valori DSCP (0..63)

Le regole sono valutate in ordine; il primo match vince.

## Vincoli di validazione

- `default_class` deve esistere in `classes`
- ogni `classifiers[].class` deve esistere in `classes`
- `preferred_paths` / `excluded_paths` devono riferire path presenti in `multipath_paths`
- `scheduler_policy` valido per ogni classe
- `duplicate_copies` clamp a 2..3 quando `duplicate: true`
- CIDR, range porte e DSCP validati a startup

## Pattern QoS consigliati

### Mission-critical
- classe `critical`
- `scheduler_policy: failover`
- `preferred_paths`: solo WAN più affidabili
- `duplicate: true`, `duplicate_copies: 2`

### Default business traffic
- classe `default`
- `scheduler_policy: balanced`
- uso di tutti i path disponibili

### Bulk
- classe `bulk`
- `scheduler_policy: balanced`
- esclusione path costosi/sensibili con `excluded_paths`

## Pattern per orchestrator esterno

### Stato desiderato (source of truth)
- orchestrator mantiene versioni di `dataplane.yaml` per tenant/sito.

### Flusso consigliato
1. orchestrator genera nuovo `dataplane.yaml`
2. valida schema e riferimenti path lato control-plane
3. distribuisce file sul nodo MPQUIC
4. applica policy via Control API (`/dataplane/reload`) oppure restart controllato istanza
5. verifica log runtime `class telemetry` e `path telemetry`

## Control API locale (implementata)

La Control API è disponibile nel client multipath quando è configurato:

```yaml
control_api_listen: 127.0.0.1:19090
control_api_auth_token: "change-me"
```

Campi consigliati:
- `control_api_listen`: bind locale (`127.0.0.1:<port>`)
- `control_api_auth_token`: token Bearer opzionale ma fortemente consigliato

Endpoint:
- `GET /healthz`: stato processo/API
- `GET /dataplane`: snapshot policy dataplane attiva
- `POST /dataplane/validate`: valida payload dataplane (JSON o YAML) senza applicare
- `POST /dataplane/apply`: valida e applica payload dataplane in runtime
- `POST /dataplane/reload`: ricarica e applica `dataplane_config_file` da disco

Esempio validate:

```bash
curl -sS -X POST \
  -H 'Authorization: Bearer change-me' \
  -H 'Content-Type: application/yaml' \
  --data-binary @/etc/mpquic/instances/dataplane.yaml \
  http://127.0.0.1:19090/dataplane/validate
```

Esempio apply:

```bash
curl -sS -X POST \
  -H 'Authorization: Bearer change-me' \
  -H 'Content-Type: application/yaml' \
  --data-binary @/etc/mpquic/instances/dataplane.yaml \
  http://127.0.0.1:19090/dataplane/apply
```

Esempio reload da file:

```bash
curl -sS -X POST \
  -H 'Authorization: Bearer change-me' \
  http://127.0.0.1:19090/dataplane/reload
```

Sicurezza operativa:
- non esporre la Control API su IP pubblici
- usare sempre token Bearer quando possibile
- limitare accesso con firewall locale/host policy

### Convenzioni operative
- tenere i nomi path stabili (`wan4`, `wan5`, `wan6`)
- usare classi canoniche (`critical`, `default`, `bulk`)
- evitare regole classifier sovrapposte non necessarie
- versionare i file policy (`dataplane.vNN.yaml`) e mantenere rollback rapido

## Esempio operativo su nodo client

```bash
sudo install -m 0644 /opt/SATCOMVAS/src/mpquic/deploy/config/client/dataplane.yaml /etc/mpquic/instances/dataplane.yaml
sudo cp /opt/SATCOMVAS/src/mpquic/deploy/config/client/multipath-dataplane-file.yaml /etc/mpquic/instances/4.yaml.tpl
sudo systemctl restart mpquic@4.service
journalctl -u mpquic@4.service -n 200 --no-pager | egrep 'path telemetry|class telemetry'
```

## Telemetria e osservabilità

- `path telemetry ...`: stato e contatori per path
- `class telemetry ...`: contatori per classe (`tx_pkts`, `tx_err`, `tx_dups`)

Questo permette a un orchestrator di verificare che le policy QoS siano realmente applicate dopo rollout.