#!/usr/bin/env bash
# create-lxc.sh — Crea CT 201 (Prometheus) e CT 202 (Grafana) su Proxmox
#
# Prerequisiti:
#   - Eseguire dal nodo Proxmox (10.10.11.2) come root
#   - Bridge vmbr1 = rete MGMT1 10.10.11.0/24
#   - VM 200 (10.10.11.100) = client MPQUIC, gateway verso tunnel 10.200.x.0/24
#
# Uso (da Proxmox):
#   bash create-lxc.sh
#
# Uso remoto:
#   ssh root@10.10.11.2 'bash -s' < create-lxc.sh

set -euo pipefail

# ── Parametri ─────────────────────────────────────────────
STORAGE="local-zfs"            # storage Proxmox (ZFS pool)
TEMPLATE_NAME="debian-12-standard_12.12-1_amd64.tar.zst"
TEMPLATE="local:vztmpl/${TEMPLATE_NAME}"
BRIDGE="vmbr1"
GATEWAY="10.10.11.100"         # VM 200 — gateway verso subnet tunnel 10.200.x.0/24
NAMESERVER="10.10.11.2"        # Proxmox host (DNS resolver)

# CT 201 — Prometheus
PROM_CTID=201
PROM_HOSTNAME="prometheus"
PROM_IP="10.10.11.201/24"
PROM_CORES=1
PROM_MEMORY=512
PROM_DISK=8                    # GB — sufficiente per ~30 giorni retention

# CT 202 — Grafana
GRAF_CTID=202
GRAF_HOSTNAME="grafana"
GRAF_IP="10.10.11.202/24"
GRAF_CORES=1
GRAF_MEMORY=512
GRAF_DISK=4                    # GB

# Password iniziale (cambiare dopo il primo login)
ROOT_PASSWORD="mpquic2025!"

# ── Verifica template ─────────────────────────────────────
if ! pveam list local | grep -q "debian-12-standard"; then
    echo "⬇ Download template Debian 12..."
    pveam download local "${TEMPLATE_NAME}"
fi

# ── Funzione di creazione ─────────────────────────────────
create_ct() {
    local ctid=$1 hostname=$2 ip=$3 cores=$4 memory=$5 disk=$6

    if pct status "$ctid" &>/dev/null; then
        echo "⚠ CT $ctid già esistente, skip"
        return
    fi

    echo "➕ Creazione CT $ctid ($hostname) ..."
    pct create "$ctid" "$TEMPLATE" \
        --hostname "$hostname" \
        --cores "$cores" \
        --memory "$memory" \
        --swap 256 \
        --rootfs "${STORAGE}:${disk}" \
        --net0 "name=eth0,bridge=${BRIDGE},ip=${ip},gw=${GATEWAY}" \
        --nameserver "$NAMESERVER" \
        --password "$ROOT_PASSWORD" \
        --unprivileged 1 \
        --features nesting=1 \
        --onboot 1 \
        --start 0

    echo "✅ CT $ctid ($hostname) creato — IP ${ip%%/*}"
}

# ── Creazione container ───────────────────────────────────
create_ct "$PROM_CTID" "$PROM_HOSTNAME" "$PROM_IP" "$PROM_CORES" "$PROM_MEMORY" "$PROM_DISK"
create_ct "$GRAF_CTID" "$GRAF_HOSTNAME" "$GRAF_IP" "$GRAF_CORES" "$GRAF_MEMORY" "$GRAF_DISK"

# ── Nota routing ──────────────────────────────────────────
# Il gateway dei container è VM 200 (10.10.11.100) che ha già le route
# verso tutte le subnet tunnel 10.200.x.0/24. NON servono route statiche
# aggiuntive nei container.

echo ""
echo "━━━ Prossimi step ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "1. Avviare i container:  pct start $PROM_CTID && pct start $GRAF_CTID"
echo "2. Installare Prometheus: pct exec $PROM_CTID -- bash < setup-prometheus.sh"
echo "3. Installare Grafana:    pct exec $GRAF_CTID -- bash < setup-grafana.sh"
echo "4. Copiare dashboard:     pct push $GRAF_CTID mpquic-dashboard.json /var/lib/grafana/dashboards/mpquic-dashboard.json"
echo "5. Verificare connettività:"
echo "   pct exec $PROM_CTID -- curl -s http://10.200.17.1:9090/metrics | head -3"
