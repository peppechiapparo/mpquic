#!/usr/bin/env bash
# wan-watchdog.sh — Auto-detect stale DHCP lease and force re-discover
#
# Problema: le NIC VirtIO in Proxmox non perdono carrier quando il cavo
# fisico viene scambiato sull'host. Il DHCP client non sa che deve fare
# un nuovo DISCOVER e mantiene il lease vecchio (rete sbagliata).
#
# Soluzione: ogni CHECK_INTERVAL secondi, per ogni WAN con lease DHCP attivo,
# pinga il gateway. Se il gateway è irraggiungibile per FAIL_THRESHOLD
# check consecutivi, forza networkctl reconfigure sull'interfaccia.
# Questo triggera un nuovo DHCP DISCOVER che prende l'IP corretto
# dal modem attualmente collegato.
#
# Installazione:
#   sudo cp scripts/wan-watchdog.sh /usr/local/bin/
#   sudo chmod +x /usr/local/bin/wan-watchdog.sh
#   sudo cp deploy/systemd/wan-watchdog.service /etc/systemd/system/
#   sudo systemctl daemon-reload
#   sudo systemctl enable --now wan-watchdog.service
#
# Config: variabili d'ambiente o default
#   WAN_INTERFACES   — lista interfacce (default: enp7s3 enp7s4 enp7s5 enp7s6 enp7s7 enp7s8)
#   CHECK_INTERVAL   — secondi tra check (default: 15)
#   FAIL_THRESHOLD   — check falliti consecutivi prima di reconfigure (default: 4 = 60s)
#   PING_TIMEOUT     — timeout singolo ping in secondi (default: 3)
#   PING_COUNT       — numero ping per check (default: 2)
#   COOLDOWN         — secondi minimo tra due reconfigure sulla stessa iface (default: 120)

set -euo pipefail

WAN_INTERFACES="${WAN_INTERFACES:-enp7s3 enp7s4 enp7s5 enp7s6 enp7s7 enp7s8}"
CHECK_INTERVAL="${CHECK_INTERVAL:-15}"
FAIL_THRESHOLD="${FAIL_THRESHOLD:-4}"
PING_TIMEOUT="${PING_TIMEOUT:-3}"
PING_COUNT="${PING_COUNT:-2}"
COOLDOWN="${COOLDOWN:-120}"

# Associative arrays per stato per-interfaccia
declare -A FAIL_COUNT         # check falliti consecutivi
declare -A LAST_RECONFIGURE   # epoch ultimo reconfigure
declare -A LAST_GATEWAY       # ultimo gateway noto

for iface in $WAN_INTERFACES; do
    FAIL_COUNT[$iface]=0
    LAST_RECONFIGURE[$iface]=0
    LAST_GATEWAY[$iface]=""
done

log() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') wan-watchdog: $*"
}

# Estrae il gateway DHCP corrente dall'interfaccia via networkctl
get_dhcp_gateway() {
    local iface="$1"
    # networkctl status mostra "Gateway: x.x.x.x" per DHCP
    networkctl status "$iface" 2>/dev/null \
        | grep -oP 'Gateway:\s+\K[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' \
        | head -1
}

# Estrae l'IP DHCP corrente dell'interfaccia
get_dhcp_address() {
    local iface="$1"
    networkctl status "$iface" 2>/dev/null \
        | grep -oP 'Address:\s+\K[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' \
        | head -1
}

# Controlla se l'interfaccia ha carrier (link fisico up)
has_carrier() {
    local iface="$1"
    local carrier
    carrier=$(cat "/sys/class/net/$iface/carrier" 2>/dev/null || echo "0")
    [[ "$carrier" == "1" ]]
}

# Controlla se l'interfaccia esiste
iface_exists() {
    [[ -d "/sys/class/net/$1" ]]
}

do_reconfigure() {
    local iface="$1"
    local reason="$2"
    local now
    now=$(date +%s)
    local last=${LAST_RECONFIGURE[$iface]}
    local elapsed=$((now - last))

    if (( elapsed < COOLDOWN )); then
        log "$iface: reconfigure skipped (cooldown ${elapsed}s/${COOLDOWN}s) — $reason"
        return
    fi

    log "$iface: *** RECONFIGURE *** reason: $reason"
    log "$iface: old gateway=${LAST_GATEWAY[$iface]:-none}, old addr=$(get_dhcp_address "$iface")"

    # Flush vecchi IP e forza re-discover
    ip addr flush dev "$iface" scope global 2>/dev/null || true
    networkctl reconfigure "$iface"

    LAST_RECONFIGURE[$iface]=$now
    FAIL_COUNT[$iface]=0

    # Attendi che il DHCP prenda il nuovo lease
    sleep 8

    local new_gw new_addr
    new_gw=$(get_dhcp_gateway "$iface")
    new_addr=$(get_dhcp_address "$iface")
    log "$iface: new gateway=${new_gw:-none}, new addr=${new_addr:-none}"
    LAST_GATEWAY[$iface]="${new_gw:-}"
}

log "Starting — interfaces: $WAN_INTERFACES"
log "Config: interval=${CHECK_INTERVAL}s, threshold=${FAIL_THRESHOLD}, ping_timeout=${PING_TIMEOUT}s, cooldown=${COOLDOWN}s"

while true; do
    for iface in $WAN_INTERFACES; do
        # Skip interfacce che non esistono (WAN1-3 senza modem)
        if ! iface_exists "$iface"; then
            continue
        fi

        # Skip interfacce senza carrier
        if ! has_carrier "$iface"; then
            FAIL_COUNT[$iface]=0
            continue
        fi

        # Ottieni gateway DHCP corrente
        gw=$(get_dhcp_gateway "$iface")

        if [[ -z "$gw" ]]; then
            # Nessun gateway — interfaccia senza lease DHCP
            # Se aveva un gateway prima, il lease è stato perso
            if [[ -n "${LAST_GATEWAY[$iface]}" ]]; then
                log "$iface: DHCP lease lost (was gw=${LAST_GATEWAY[$iface]})"
                do_reconfigure "$iface" "DHCP lease lost"
            fi
            continue
        fi

        # Salva gateway per confronto futuro
        LAST_GATEWAY[$iface]="$gw"

        # Ping gateway via interfaccia specifica
        if ping -I "$iface" -c "$PING_COUNT" -W "$PING_TIMEOUT" -q "$gw" &>/dev/null; then
            # Gateway raggiungibile — reset contatore
            if (( FAIL_COUNT[$iface] > 0 )); then
                log "$iface: gateway $gw reachable again (was ${FAIL_COUNT[$iface]} fails)"
            fi
            FAIL_COUNT[$iface]=0
        else
            # Gateway irraggiungibile
            FAIL_COUNT[$iface]=$(( FAIL_COUNT[$iface] + 1 ))
            log "$iface: gateway $gw UNREACHABLE (${FAIL_COUNT[$iface]}/${FAIL_THRESHOLD})"

            if (( FAIL_COUNT[$iface] >= FAIL_THRESHOLD )); then
                do_reconfigure "$iface" "gateway $gw unreachable for $(( FAIL_COUNT[$iface] * CHECK_INTERVAL ))s"
            fi
        fi
    done

    sleep "$CHECK_INTERVAL"
done
