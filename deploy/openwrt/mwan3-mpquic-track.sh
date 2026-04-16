#!/bin/sh
# =============================================================================
# mwan3-mpquic-track.sh — Custom mwan3 tracking script for mpquic tunnels
# =============================================================================
#
# Usato come track_method='custom' per mwan3 su interfacce TUN di mpquic.
# Verifica 3 condizioni per dichiarare il tunnel ONLINE:
#   1. Il processo mpquic è in esecuzione
#   2. Il TUN device è UP con carrier
#   3. Il peer remoto risponde a ping via TUN
#
# Installazione:
#   scp mwan3-mpquic-track.sh root@openwrt:/usr/libexec/mwan3-mpquic-track.sh
#   chmod 755 /usr/libexec/mwan3-mpquic-track.sh
#
# Configurazione mwan3 (alternativa al ping diretto):
#   uci set mwan3.BOND1.track_method='custom'
#   uci set mwan3.BOND1.track_ip='10.200.17.254'
#   uci commit mwan3
#
# mwan3 chiama lo script custom con:
#   $0 $INTERFACE $DEVICE $TIMEOUT $TRACK_IP
#
# Exit 0 = UP, Exit 1 = DOWN
# =============================================================================

INTERFACE="$1"
DEVICE="$2"
TIMEOUT="${3:-4}"
TRACK_IP="$4"

# Validate device name: only alphanumeric, underscore, hyphen
case "$DEVICE" in
    ''|*[!a-zA-Z0-9_-]*) logger -t mwan3-track -p daemon.err "[$INTERFACE] invalid device name: $DEVICE"; exit 1 ;;
esac

# Validate timeout is numeric
case "$TIMEOUT" in
    ''|*[!0-9]*) TIMEOUT=4 ;;
esac

# --- 1) Verificare il processo mpquic ---
# Cerchiamo un processo mpquic che usi la config associata a questo tunnel.
# Il PID file è in /var/run/mpquic-<tunnel_name>.pid (se usato con systemd/procd)
TUN_NAME=""
case "$INTERFACE" in
    BOND1) TUN_NAME="mp1" ;;
    *)     TUN_NAME="$DEVICE" ;;
esac

MPQUIC_RUNNING=0
if pgrep -f "mpquic.*${TUN_NAME}" >/dev/null 2>&1; then
    MPQUIC_RUNNING=1
fi

if [ "$MPQUIC_RUNNING" -eq 0 ]; then
    logger -t mwan3-track -p daemon.warning "[$INTERFACE] mpquic process not running for ${TUN_NAME}"
    exit 1
fi

# --- 2) Verificare che il TUN device esista e sia UP ---
if [ ! -d "/sys/class/net/${DEVICE}" ]; then
    logger -t mwan3-track -p daemon.warning "[$INTERFACE] device ${DEVICE} does not exist"
    exit 1
fi

OPERSTATE=$(cat "/sys/class/net/${DEVICE}/operstate" 2>/dev/null)
if [ "$OPERSTATE" != "up" ] && [ "$OPERSTATE" != "unknown" ]; then
    # TUN devices spesso riportano "unknown" come operstate, è normale
    logger -t mwan3-track -p daemon.warning "[$INTERFACE] device ${DEVICE} operstate=${OPERSTATE}"
    exit 1
fi

# --- 3) Ping al peer remoto via il TUN device ---
if [ -n "$TRACK_IP" ]; then
    if ping -I "$DEVICE" -c 1 -W "$TIMEOUT" -q "$TRACK_IP" >/dev/null 2>&1; then
        exit 0
    else
        logger -t mwan3-track -p daemon.warning "[$INTERFACE] ping to ${TRACK_IP} via ${DEVICE} failed"
        exit 1
    fi
fi

# Se non c'è track_ip, basta che processo e device siano ok
exit 0
