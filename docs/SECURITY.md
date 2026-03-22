# MPQUIC — Postura di sicurezza

## Trasporto QUIC (tunnel 1:1)

- **TLS 1.3** intrinseco nel protocollo QUIC (handshake, cifratura, autenticazione)
- Certificati X.509: CA self-signed + certificato server con CN `mpquic-server`
- Client verifica CA (`tls_ca_file`) e CN (`tls_server_name`)
- `tls_insecure_skip_verify: false` obbligatorio in produzione
- PFS (Perfect Forward Secrecy) per sessione: chiavi effimere ECDHE

## Trasporto UDP Stripe (multipath)

- **AES-256-GCM** per ogni pacchetto UDP (cifratura + autenticazione)
- Chiavi direzionali derivate da TLS 1.3 Exporter Material (PFS per sessione)
- Nonce monotono per anti-replay (rifiuto pacchetti con nonce inferiore)
- Zero configurazione manuale: le chiavi vengono negoziate automaticamente nel handshake QUIC
- Metrica `mpquic_session_decrypt_fail`: contatore tentativi di decifrazione falliti

## Rete

- **SO_BINDTODEVICE**: ogni socket è vincolata all'interfaccia WAN corretta a livello kernel
- Firewall nftables con policy drop su server VPS: solo porte UDP 45001-45006 aperte
- Metriche Prometheus in ascolto solo su interfaccia tunnel (10.200.x.y), non raggiungibili dall'esterno
- pprof (debug) su `127.0.0.1:6060` — solo loopback, mai esposto

## Autenticazione servizi

- Control API orchestrator: token Bearer (`control_api_auth_token`) + bind locale (`127.0.0.1`)
- SSH verso VPS: chiave pubblica, no password

## Limiti noti (fase corrente)

- Certificati self-signed (non CA pubblica) — adeguato per tunnel point-to-point
- Nessun meccanismo di revoca certificati
- Nessuna rotazione automatica certificati (rinnovo manuale, scadenza 825 giorni)

## Riferimenti

- Generazione certificati: `docs/INSTALLAZIONE_TEST.md` §17
- Cifratura stripe: `docs/ARCHITETTURA.md` (sezione AES-256-GCM)
- Firewall nftables: `docs/INSTALLAZIONE_TEST.md` §14
- Analisi sicurezza formale: `docs/NOTA_TECNICA_MPQUIC.md` §16
