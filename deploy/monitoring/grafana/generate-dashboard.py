#!/usr/bin/env python3
"""
Genera mpquic-dashboard.json per Grafana.
Eseguire: python3 generate-dashboard.py > ../grafana/mpquic-dashboard.json
"""
import json, sys

DS = {"type": "prometheus", "uid": "prometheus"}

def stat_panel(title, expr, legend, grid, pid, **kw):
    unit = kw.get("unit", "")
    mappings = kw.get("mappings", [])
    thresholds = kw.get("thresholds", {"mode": "absolute", "steps": [{"color": "green", "value": None}]})
    color_mode = kw.get("color_mode", "value")
    text_mode = kw.get("text_mode", "auto")
    graph_mode = kw.get("graph_mode", "none")
    p = {
        "datasource": DS,
        "fieldConfig": {
            "defaults": {
                "unit": unit,
                "mappings": mappings,
                "thresholds": thresholds
            },
            "overrides": []
        },
        "gridPos": grid,
        "id": pid,
        "options": {
            "colorMode": color_mode,
            "graphMode": graph_mode,
            "justifyMode": "auto",
            "orientation": "horizontal",
            "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
            "showPercentChange": False,
            "textMode": text_mode,
            "wideLayout": True
        },
        "pluginVersion": "12.4.1",
        "targets": [{"expr": expr, "legendFormat": legend, "refId": "A"}],
        "title": title,
        "type": "stat"
    }
    return p

def ts_panel(title, targets, grid, pid, **kw):
    unit = kw.get("unit", "")
    fillOpacity = kw.get("fill", 15)
    stacking = kw.get("stacking", "none")
    p = {
        "datasource": DS,
        "fieldConfig": {
            "defaults": {
                "custom": {
                    "drawStyle": "line",
                    "lineInterpolation": "smooth",
                    "lineWidth": 2,
                    "fillOpacity": fillOpacity,
                    "gradientMode": "opacity",
                    "showPoints": "never",
                    "stacking": {"mode": stacking}
                },
                "unit": unit,
                "thresholds": {"mode": "absolute", "steps": [{"color": "green", "value": None}]}
            },
            "overrides": []
        },
        "gridPos": grid,
        "id": pid,
        "options": {
            "legend": {"calcs": ["mean", "max", "lastNotNull"], "displayMode": "table", "placement": "bottom"},
            "tooltip": {"mode": "multi", "sort": "desc"}
        },
        "pluginVersion": "12.4.1",
        "targets": [{"expr": t[0], "legendFormat": t[1], "refId": chr(65+i)} for i, t in enumerate(targets)],
        "title": title,
        "type": "timeseries"
    }
    return p

def row_panel(title, y, pid, collapsed=False):
    return {
        "collapsed": collapsed,
        "gridPos": {"h": 1, "w": 24, "x": 0, "y": y},
        "id": pid,
        "title": title,
        "type": "row"
    }

def table_panel(title, targets, grid, pid, **kw):
    p = {
        "datasource": DS,
        "fieldConfig": {
            "defaults": {
                "thresholds": {"mode": "absolute", "steps": [{"color": "green", "value": None}]}
            },
            "overrides": []
        },
        "gridPos": grid,
        "id": pid,
        "options": {
            "showHeader": True,
            "sortBy": [{"displayName": "instance_name", "desc": False}]
        },
        "pluginVersion": "12.4.1",
        "targets": [{"expr": t[0], "legendFormat": t[1], "refId": chr(65+i), "format": "table", "instant": True} for i, t in enumerate(targets)],
        "title": title,
        "type": "table",
        "transformations": kw.get("transformations", [])
    }
    return p

# ============================================================
pid = 1
panels = []
y = 0

# ── ROW 0: Status at a Glance ──────────────────────────────
panels.append(row_panel("Stato Tunnel", y, pid)); pid += 1; y += 1

# Tunnel status (client) — up/down based on scrape target
up_down_mappings = [{"options": {"0": {"color": "red", "text": "DOWN"}, "1": {"color": "green", "text": "UP"}}, "type": "value"}]
up_down_thresholds = {"mode": "absolute", "steps": [{"color": "red", "value": 0}, {"color": "green", "value": 1}]}

panels.append(stat_panel(
    "Client tunnels", 'up{job="mpquic-client"}', "{{instance_name}}",
    {"h": 4, "w": 16, "x": 0, "y": y}, pid,
    mappings=up_down_mappings, thresholds=up_down_thresholds,
    color_mode="background", text_mode="value_and_name"
)); pid += 1

panels.append(stat_panel(
    "Server instances", 'up{job="mpquic-server"}', "{{instance_name}}",
    {"h": 4, "w": 8, "x": 16, "y": y}, pid,
    mappings=up_down_mappings, thresholds=up_down_thresholds,
    color_mode="background", text_mode="value_and_name"
)); pid += 1
y += 4

# ── ROW 1: Overview ────────────────────────────────────────
panels.append(row_panel("Overview", y, pid)); pid += 1; y += 1

panels.append(stat_panel(
    "Uptime", 'max by (instance_name, job) (mpquic_uptime_seconds{job=~"$job"})',
    "{{instance_name}} ({{job}})",
    {"h": 6, "w": 6, "x": 0, "y": y}, pid,
    unit="s"
)); pid += 1

panels.append(stat_panel(
    "Sessioni attive (mp1)", 'count(mpquic_session_pipes{job="mpquic-server", instance_name="mp1"})',
    "",
    {"h": 6, "w": 3, "x": 6, "y": y}, pid,
    thresholds={"mode": "absolute", "steps": [{"color": "red", "value": None}, {"color": "yellow", "value": 1}, {"color": "green", "value": 2}]}
)); pid += 1

panels.append(stat_panel(
    "Path attivi (mp1)", 'sum(mpquic_path_alive{job="mpquic-client", instance_name="mp1"})',
    "",
    {"h": 6, "w": 3, "x": 9, "y": y}, pid,
    thresholds={"mode": "absolute", "steps": [{"color": "red", "value": None}, {"color": "yellow", "value": 1}, {"color": "green", "value": 2}]}
)); pid += 1

panels.append(stat_panel(
    "TX totale", 'max by (instance_name, job) (mpquic_tx_bytes_total{job=~"$job"})',
    "{{instance_name}}",
    {"h": 6, "w": 6, "x": 12, "y": y}, pid,
    unit="decbytes"
)); pid += 1

panels.append(stat_panel(
    "RX totale", 'max by (instance_name, job) (mpquic_rx_bytes_total{job=~"$job"})',
    "{{instance_name}}",
    {"h": 6, "w": 6, "x": 18, "y": y}, pid,
    unit="decbytes"
)); pid += 1
y += 6

# ── ROW 2: Multipath Stripe (mp1) ──────────────────────
panels.append(row_panel("Multipath Stripe — mp1 (WAN5 + WAN6)", y, pid)); pid += 1; y += 1

panels.append(stat_panel(
    "Path status (mp1)", 'mpquic_path_alive{job="mpquic-client", instance_name="mp1"}',
    "{{path}}",
    {"h": 4, "w": 6, "x": 0, "y": y}, pid,
    mappings=up_down_mappings, thresholds=up_down_thresholds,
    color_mode="background", text_mode="value_and_name"
)); pid += 1

panels.append(stat_panel(
    "Pipe per sessione", 'mpquic_session_pipes{job="mpquic-server", instance_name="mp1"}',
    "sess {{session}}",
    {"h": 4, "w": 6, "x": 6, "y": y}, pid,
    thresholds={"mode":"absolute","steps":[{"color":"red","value":None},{"color":"green","value":1}]}
)); pid += 1

panels.append(stat_panel(
    "Session uptime", 'mpquic_session_uptime_seconds{job="mpquic-server", instance_name="mp1"}',
    "sess {{session}}",
    {"h": 4, "w": 6, "x": 12, "y": y}, pid,
    unit="s"
)); pid += 1

panels.append(stat_panel(
    "Adaptive FEC M", 'mpquic_session_adaptive_m{job="mpquic-server", instance_name="mp1"}',
    "sess {{session}}",
    {"h": 4, "w": 6, "x": 18, "y": y}, pid
)); pid += 1
y += 4

# Stripe TX/RX per path (client)
panels.append(ts_panel(
    "TX rate per path (stripe client)", [
        ('irate(mpquic_path_stripe_tx_bytes{job="mpquic-client", instance_name="mp1"}[1m]) * 8', "{{path}}")
    ],
    {"h": 8, "w": 12, "x": 0, "y": y}, pid, unit="bps"
)); pid += 1

panels.append(ts_panel(
    "RX rate per path (stripe client)", [
        ('irate(mpquic_path_stripe_rx_bytes{job="mpquic-client", instance_name="mp1"}[1m]) * 8', "{{path}}")
    ],
    {"h": 8, "w": 12, "x": 12, "y": y}, pid, unit="bps"
)); pid += 1
y += 8

# Server session TX/RX
panels.append(ts_panel(
    "TX rate per session (server mp1)", [
        ('irate(mpquic_session_tx_bytes{job="mpquic-server", instance_name="mp1"}[1m]) * 8', "sess {{session}}")
    ],
    {"h": 8, "w": 12, "x": 0, "y": y}, pid, unit="bps"
)); pid += 1

panels.append(ts_panel(
    "RX rate per session (server mp1)", [
        ('irate(mpquic_session_rx_bytes{job="mpquic-server", instance_name="mp1"}[1m]) * 8', "sess {{session}}")
    ],
    {"h": 8, "w": 12, "x": 12, "y": y}, pid, unit="bps"
)); pid += 1
y += 8

# ── ROW 3: Tunnel Throughput (all VLAN + single-link clients) ──
panels.append(row_panel("Throughput Tunnel Client (VLAN + single-link)", y, pid)); pid += 1; y += 1

# TX/RX for all client tunnels
panels.append(ts_panel(
    "TX rate per tunnel (client)", [
        ('irate(mpquic_tx_bytes_total{job="mpquic-client"}[$__rate_interval]) * 8', "{{instance_name}}")
    ],
    {"h": 8, "w": 12, "x": 0, "y": y}, pid, unit="bps"
)); pid += 1

panels.append(ts_panel(
    "RX rate per tunnel (client)", [
        ('irate(mpquic_rx_bytes_total{job="mpquic-client"}[$__rate_interval]) * 8', "{{instance_name}}")
    ],
    {"h": 8, "w": 12, "x": 12, "y": y}, pid, unit="bps"
)); pid += 1
y += 8

# TX/RX by WAN (aggregated)
panels.append(ts_panel(
    "TX rate per WAN (aggregato)", [
        ('sum by (wan) (irate(mpquic_tx_bytes_total{job="mpquic-client", wan!=""}[$__rate_interval])) * 8', "{{wan}}")
    ],
    {"h": 8, "w": 12, "x": 0, "y": y}, pid, unit="bps"
)); pid += 1

panels.append(ts_panel(
    "RX rate per WAN (aggregato)", [
        ('sum by (wan) (irate(mpquic_rx_bytes_total{job="mpquic-client", wan!=""}[$__rate_interval])) * 8', "{{wan}}")
    ],
    {"h": 8, "w": 12, "x": 12, "y": y}, pid, unit="bps"
)); pid += 1
y += 8

# TX/RX by class (aggregated)
panels.append(ts_panel(
    "TX rate per classe (aggregato)", [
        ('sum by (class) (irate(mpquic_tx_bytes_total{job="mpquic-client", class!=""}[$__rate_interval])) * 8', "{{class}}")
    ],
    {"h": 8, "w": 12, "x": 0, "y": y}, pid, unit="bps"
)); pid += 1

panels.append(ts_panel(
    "RX rate per classe (aggregato)", [
        ('sum by (class) (irate(mpquic_rx_bytes_total{job="mpquic-client", class!=""}[$__rate_interval])) * 8', "{{class}}")
    ],
    {"h": 8, "w": 12, "x": 12, "y": y}, pid, unit="bps"
)); pid += 1
y += 8

# ── ROW 4: Server Throughput (multi-conn) ──────────────────
panels.append(row_panel("Throughput Server (tutte le istanze)", y, pid)); pid += 1; y += 1

panels.append(ts_panel(
    "TX rate per istanza server", [
        ('irate(mpquic_tx_bytes_total{job="mpquic-server"}[$__rate_interval]) * 8', "{{instance_name}}")
    ],
    {"h": 8, "w": 12, "x": 0, "y": y}, pid, unit="bps"
)); pid += 1

panels.append(ts_panel(
    "RX rate per istanza server", [
        ('irate(mpquic_rx_bytes_total{job="mpquic-server"}[$__rate_interval]) * 8', "{{instance_name}}")
    ],
    {"h": 8, "w": 12, "x": 12, "y": y}, pid, unit="bps"
)); pid += 1
y += 8

# Packet rate server
panels.append(ts_panel(
    "Packet rate TX server", [
        ('irate(mpquic_tx_packets_total{job="mpquic-server"}[$__rate_interval])', "{{instance_name}}")
    ],
    {"h": 8, "w": 12, "x": 0, "y": y}, pid, unit="pps"
)); pid += 1

panels.append(ts_panel(
    "Packet rate RX server", [
        ('irate(mpquic_rx_packets_total{job="mpquic-server"}[$__rate_interval])', "{{instance_name}}")
    ],
    {"h": 8, "w": 12, "x": 12, "y": y}, pid, unit="pps"
)); pid += 1
y += 8

# ── ROW 5: Quality — FEC / ARQ / Loss ──────────────────────
panels.append(row_panel("Quality — FEC / ARQ / Loss (mp1 server)", y, pid)); pid += 1; y += 1

panels.append(ts_panel(
    "Loss rate per session (%)", [
        ('mpquic_session_loss_rate_pct{job="mpquic-server"}', "sess {{session}} ({{instance_name}})")
    ],
    {"h": 8, "w": 8, "x": 0, "y": y}, pid, unit="percent"
)); pid += 1

panels.append(ts_panel(
    "FEC recovery rate", [
        ('rate(mpquic_session_fec_recovered{job="mpquic-server"}[$__rate_interval])', "recovered {{session}}")
    ],
    {"h": 8, "w": 8, "x": 8, "y": y}, pid, unit="ops"
)); pid += 1

panels.append(ts_panel(
    "ARQ — NACK / Retransmit / Dup", [
        ('rate(mpquic_session_arq_nack_sent{job="mpquic-server"}[$__rate_interval])', "NACK {{session}}"),
        ('rate(mpquic_session_arq_retx_recv{job="mpquic-server"}[$__rate_interval])', "Retx {{session}}"),
        ('rate(mpquic_session_arq_dup_filtered{job="mpquic-server"}[$__rate_interval])', "Dup {{session}}")
    ],
    {"h": 8, "w": 8, "x": 16, "y": y}, pid, unit="ops"
)); pid += 1
y += 8

panels.append(ts_panel(
    "Adaptive FEC M (shards parità)", [
        ('mpquic_session_adaptive_m{job="mpquic-server"}', "M {{session}} ({{instance_name}})")
    ],
    {"h": 8, "w": 12, "x": 0, "y": y}, pid
)); pid += 1

panels.append(ts_panel(
    "Decrypt failures (sicurezza)", [
        ('increase(mpquic_session_decrypt_fail_total{job="mpquic-server"}[1m])', "{{session}} ({{instance_name}})")
    ],
    {"h": 8, "w": 12, "x": 12, "y": y}, pid,
    thresholds_kw=True
)); pid += 1
y += 8

# ── ROW 6: Stripe FEC client-side ──────────────────────────
panels.append(row_panel("Stripe — dettaglio client (mp1)", y, pid)); pid += 1; y += 1

panels.append(ts_panel(
    "FEC recovery per path (client stripe)", [
        ('rate(mpquic_path_stripe_fec_recovered{job="mpquic-client"}[$__rate_interval])', "{{path}}")
    ],
    {"h": 8, "w": 12, "x": 0, "y": y}, pid, unit="ops"
)); pid += 1

panels.append(ts_panel(
    "Packet rate per path (client)", [
        ('rate(mpquic_path_tx_packets{job="mpquic-client"}[$__rate_interval])', "TX {{path}}"),
        ('rate(mpquic_path_rx_packets{job="mpquic-client"}[$__rate_interval])', "RX {{path}}")
    ],
    {"h": 8, "w": 12, "x": 12, "y": y}, pid, unit="pps"
)); pid += 1
y += 8

# ── Dashboard envelope ─────────────────────────────────────
dashboard = {
    "annotations": {
        "list": [{
            "builtIn": 1,
            "datasource": {"type": "grafana", "uid": "-- Grafana --"},
            "enable": True,
            "hide": True,
            "iconColor": "rgba(0, 211, 255, 1)",
            "name": "Annotations & Alerts",
            "type": "dashboard"
        }]
    },
    "editable": True,
    "fiscalYearStartMonth": 0,
    "graphTooltip": 1,
    "links": [],
    "panels": panels,
    "schemaVersion": 40,
    "tags": ["mpquic", "tunnel", "monitoring"],
    "templating": {
        "list": [{
            "current": {"selected": True, "text": ["All"], "value": ["$__all"]},
            "datasource": {"type": "prometheus", "uid": "prometheus"},
            "definition": "label_values(mpquic_uptime_seconds, job)",
            "includeAll": True,
            "multi": True,
            "name": "job",
            "query": {"qryType": 1, "query": "label_values(mpquic_uptime_seconds, job)"},
            "refresh": 1,
            "type": "query"
        }]
    },
    "time": {"from": "now-1h", "to": "now"},
    "timepicker": {},
    "timezone": "browser",
    "title": "MPQUIC Overview",
    "uid": "adsnpmk",
    "version": 5
}

json.dump(dashboard, sys.stdout, indent=2, ensure_ascii=False)
print()
