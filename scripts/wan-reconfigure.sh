#!/usr/bin/env bash
# wan-reconfigure.sh — Forza DHCP re-discover sulle WAN dopo cable-swap
#
# Uso:
#   wan-reconfigure.sh              # riconfigura tutte le WAN (enp7s3-enp7s8)
#   wan-reconfigure.sh wan5         # riconfigura solo WAN5 (enp7s7)
#   wan-reconfigure.sh enp7s7       # riconfigura solo enp7s7
#
# Perché serve: le NIC VirtIO in Proxmox non riflettono il carrier loss
# fisico nella VM. Dopo aver scambiato un cavo, il DHCP client non sa che
# deve fare un nuovo DISCOVER e mantiene il lease vecchio.
#
# Questo script: rilascia il lease DHCP vecchio → rimuove l'IP →
# forza un nuovo DHCP DISCOVER sulla/e interfaccia/e.

set -euo pipefail

# Mappatura nome logico → interfaccia
declare -A WAN_MAP=(
    [wan1]=enp7s3  [wan2]=enp7s4  [wan3]=enp7s5
    [wan4]=enp7s6  [wan5]=enp7s7  [wan6]=enp7s8
)
ALL_WANS=(enp7s3 enp7s4 enp7s5 enp7s6 enp7s7 enp7s8)

resolve_iface() {
    local arg="$1"
    # Se è un nome logico (wan5), traduci
    if [[ -n "${WAN_MAP[$arg]+x}" ]]; then
        echo "${WAN_MAP[$arg]}"
        return
    fi
    # Se è già un nome interfaccia
    if [[ "$arg" == enp* ]]; then
        echo "$arg"
        return
    fi
    echo "ERRORE: interfaccia sconosciuta: $arg" >&2
    echo "  Valori validi: wan1-wan6, enp7s3-enp7s8" >&2
    return 1
}

reconfigure_wan() {
    local iface="$1"

    # Verifica che l'interfaccia esista
    if [[ ! -d "/sys/class/net/$iface" ]]; then
        echo "  ⚠  $iface: non esiste (slot vuoto?), skip"
        return 0
    fi

    local carrier
    carrier=$(cat "/sys/class/net/$iface/carrier" 2>/dev/null || echo 0)
    local old_ip
    old_ip=$(ip -4 -o addr show "$iface" 2>/dev/null | awk '{print $4}' || true)

    echo "  → $iface: carrier=$carrier  old_ip=${old_ip:-none}"

    # Forza reconfigure (rilascia vecchio DHCP, fa nuovo DISCOVER)
    networkctl reconfigure "$iface"

    # Attendi fino a 15s che il nuovo lease arrivi
    local waited=0
    while (( waited < 15 )); do
        sleep 1
        waited=$((waited + 1))
        local new_ip
        new_ip=$(ip -4 -o addr show "$iface" 2>/dev/null | awk '{print $4}' || true)
        if [[ -n "$new_ip" && "$new_ip" != "$old_ip" ]]; then
            echo "  ✓ $iface: nuovo IP $new_ip (era ${old_ip:-none}) — ${waited}s"
            return 0
        fi
        if [[ -n "$new_ip" && "$new_ip" == "$old_ip" && $waited -ge 5 ]]; then
            # Stesso IP dopo 5s — potrebbe essere lo stesso DHCP server (reboot modem)
            echo "  ✓ $iface: IP confermato $new_ip (lease rinnovato) — ${waited}s"
            return 0
        fi
    done

    local final_ip
    final_ip=$(ip -4 -o addr show "$iface" 2>/dev/null | awk '{print $4}' || true)
    if [[ -n "$final_ip" ]]; then
        echo "  ✓ $iface: IP $final_ip — ${waited}s"
    else
        echo "  ✗ $iface: nessun IP dopo ${waited}s (carrier=$carrier, DHCP server raggiungibile?)"
    fi
}

# --- Main ---
echo "=== WAN Reconfigure (DHCP re-discover) ==="

if [[ $# -eq 0 ]]; then
    echo "Riconfiguro tutte le WAN..."
    for iface in "${ALL_WANS[@]}"; do
        reconfigure_wan "$iface"
    done
else
    for arg in "$@"; do
        iface=$(resolve_iface "$arg")
        reconfigure_wan "$iface"
    done
fi

echo "=== Done ==="
