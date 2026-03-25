'use strict';
'require view';
'require rpc';
'require poll';
'require dom';
'require ui';

/*
 * MPQUIC Dashboard — main view
 *
 * Shows system health overview and a live-updating table of all tunnel
 * instances with status, metrics and quick action buttons.
 *
 * Data is fetched via rpcd/ubus proxy → TBOX Management API.
 */

var callHealth  = rpc.declare({ object: 'mpquic', method: 'health' });
var callTunnels = rpc.declare({ object: 'mpquic', method: 'tunnels' });
var callMetrics = rpc.declare({ object: 'mpquic', method: 'metrics' });

var callStart   = rpc.declare({ object: 'mpquic', method: 'tunnel_start',   params: ['name'] });
var callStop    = rpc.declare({ object: 'mpquic', method: 'tunnel_stop',    params: ['name'] });
var callRestart = rpc.declare({ object: 'mpquic', method: 'tunnel_restart', params: ['name'] });

/* ── Helpers ───────────────────────────────────────────────────────────── */

function badge(text, color) {
	var span = E('span', { 'class': 'badge', 'style':
		'display:inline-block;padding:2px 8px;border-radius:4px;font-size:0.85em;font-weight:bold;' +
		'color:#fff;background-color:' + color }, text);
	return span;
}

function statusBadge(state) {
	switch ((state || '').toLowerCase()) {
	case 'running': return badge('RUNNING', '#22a722');
	case 'stopped': return badge('STOPPED', '#d32f2f');
	case 'failed':  return badge('FAILED', '#e65100');
	default:        return badge(state || 'unknown', '#757575');
	}
}

function formatBytes(b) {
	if (b == null || isNaN(b)) return '-';
	if (b < 1024)             return b + ' B';
	if (b < 1048576)          return (b / 1024).toFixed(1) + ' KB';
	if (b < 1073741824)       return (b / 1048576).toFixed(1) + ' MB';
	return (b / 1073741824).toFixed(2) + ' GB';
}

function formatUptime(seconds) {
	if (!seconds || seconds <= 0) return '-';
	var d = Math.floor(seconds / 86400),
	    h = Math.floor((seconds % 86400) / 3600),
	    m = Math.floor((seconds % 3600) / 60);
	if (d > 0) return d + 'd ' + h + 'h ' + m + 'm';
	if (h > 0) return h + 'h ' + m + 'm';
	return m + 'm';
}

function safeNum(v, decimals) {
	if (v == null || isNaN(v)) return '-';
	return decimals != null ? Number(v).toFixed(decimals) : String(v);
}

/* ── View ──────────────────────────────────────────────────────────────── */

return view.extend({
	title: _('MPQUIC Tunnels — Dashboard'),

	load: function() {
		return Promise.all([
			callHealth(),
			callTunnels(),
			callMetrics()
		]);
	},

	render: function(data) {
		var health  = data[0] || {};
		var tunnels = data[1] || {};
		var metrics = data[2] || {};

		var tunnelList = tunnels.tunnels || tunnels.instances || [];
		if (!Array.isArray(tunnelList) && typeof tunnelList === 'object') {
			tunnelList = Object.values(tunnelList);
		}

		/* ── Health cards ─────────────────────────────────────────── */
		var healthCards = E('div', { 'class': 'cbi-section', 'style': 'display:flex;flex-wrap:wrap;gap:16px;margin-bottom:24px' }, [
			this.renderCard('Total', health.tunnels_total || tunnelList.length, '#1565c0'),
			this.renderCard('Running', health.tunnels_running || tunnelList.filter(function(t) { return t.state === 'running'; }).length, '#22a722'),
			this.renderCard('Stopped', health.tunnels_stopped || tunnelList.filter(function(t) { return t.state === 'stopped'; }).length, '#d32f2f'),
			this.renderCard('Failed', health.tunnels_failed || 0, '#e65100'),
			this.renderCard('Version', health.version || '-', '#455a64'),
		]);

		/* ── Tunnel table ─────────────────────────────────────────── */
		var tableHead = E('tr', {}, [
			E('th', {}, 'Name'),
			E('th', {}, 'State'),
			E('th', {}, 'WAN'),
			E('th', {}, 'Mode'),
			E('th', {}, 'Uptime'),
			E('th', {}, 'TX'),
			E('th', {}, 'RX'),
			E('th', { 'style': 'text-align:right' }, 'Loss %'),
			E('th', { 'style': 'text-align:right' }, 'RTT ms'),
			E('th', { 'style': 'text-align:right' }, 'FEC Recv'),
			E('th', {}, 'Actions'),
		]);

		var metricsMap = {};
		if (metrics && typeof metrics === 'object') {
			var ml = metrics.tunnels || metrics;
			if (Array.isArray(ml)) {
				ml.forEach(function(m) { if (m.name) metricsMap[m.name] = m; });
			}
		}

		var rows = tunnelList.map(L.bind(function(t) {
			var name = t.name || t.instance || '';
			var m = metricsMap[name] || t.metrics || {};

			return E('tr', {}, [
				E('td', {}, E('strong', {}, name)),
				E('td', {}, statusBadge(t.state)),
				E('td', {}, t.wan || t.bind_ip || '-'),
				E('td', {}, t.transport_mode || t.mode || '-'),
				E('td', {}, formatUptime(t.uptime_seconds || t.uptime)),
				E('td', {}, formatBytes(m.tx_bytes)),
				E('td', {}, formatBytes(m.rx_bytes)),
				E('td', { 'style': 'text-align:right' }, safeNum(m.loss_rate || m.loss_percent, 2)),
				E('td', { 'style': 'text-align:right' }, safeNum(m.rtt_ms || m.srtt_ms, 1)),
				E('td', { 'style': 'text-align:right' }, safeNum(m.fec_recovered || m.fec_recoveries)),
				E('td', {}, this.renderActions(name, t.state)),
			]);
		}, this));

		var table = E('table', { 'class': 'table cbi-section-table', 'style': 'width:100%' }, [
			E('thead', {}, tableHead)
		].concat(rows.length > 0 ? rows : [
			E('tr', {}, E('td', { 'colspan': '11', 'style': 'text-align:center;color:#888' }, 'No tunnels found — check TBOX connection'))
		]));

		/* ── Connection status ────────────────────────────────────── */
		var connStatus = '';
		if (health.error) {
			connStatus = E('div', { 'class': 'alert-message warning', 'style': 'margin-bottom:16px' },
				E('p', {}, '⚠ Connection to TBOX failed: ' + health.error +
					'. Verify /etc/config/mpquic settings.'));
		}

		/* ── Assemble ─────────────────────────────────────────────── */
		var body = E('div', {}, [
			E('h2', {}, _('MPQUIC Tunnels — Dashboard')),
			connStatus,
			healthCards,
			E('div', { 'class': 'cbi-section' }, [
				E('h3', {}, _('Tunnel Instances')),
				table,
			]),
		]);

		/* ── Auto-refresh ─────────────────────────────────────────── */
		poll.add(L.bind(function() {
			return Promise.all([
				callHealth(),
				callTunnels(),
				callMetrics()
			]).then(L.bind(function(fresh) {
				var newBody = this.render(fresh);
				dom.content(document.querySelector('#view'), newBody);
			}, this));
		}, this), 10);

		return body;
	},

	renderCard: function(label, value, color) {
		return E('div', {
			'style': 'flex:1;min-width:120px;max-width:200px;padding:16px;border-radius:8px;' +
			          'background:' + color + ';color:#fff;text-align:center;'
		}, [
			E('div', { 'style': 'font-size:2em;font-weight:bold' }, String(value)),
			E('div', { 'style': 'margin-top:4px;font-size:0.85em;opacity:0.9' }, label),
		]);
	},

	renderActions: function(name, state) {
		var isRunning = (state === 'running');
		var btns = [];

		if (isRunning) {
			btns.push(E('button', {
				'class': 'cbi-button cbi-button-neutral',
				'style': 'margin:0 2px;font-size:0.8em;padding:2px 8px',
				'click': ui.createHandlerFn(this, function(ev) {
					return callRestart(name).then(function() {
						ui.addNotification(null, E('p', {}, 'Restarting ' + name + '…'), 'info');
					});
				})
			}, '⟳ Restart'));

			btns.push(E('button', {
				'class': 'cbi-button cbi-button-negative',
				'style': 'margin:0 2px;font-size:0.8em;padding:2px 8px',
				'click': ui.createHandlerFn(this, function(ev) {
					return callStop(name).then(function() {
						ui.addNotification(null, E('p', {}, 'Stopped ' + name), 'info');
					});
				})
			}, '■ Stop'));
		} else {
			btns.push(E('button', {
				'class': 'cbi-button cbi-button-positive',
				'style': 'margin:0 2px;font-size:0.8em;padding:2px 8px',
				'click': ui.createHandlerFn(this, function(ev) {
					return callStart(name).then(function() {
						ui.addNotification(null, E('p', {}, 'Starting ' + name + '…'), 'info');
					});
				})
			}, '▶ Start'));
		}

		return E('span', {}, btns);
	},

	handleSaveApply: null,
	handleSave: null,
	handleReset: null,
});
