'use strict';
'require view';
'require rpc';
'require ui';
'require dom';

/*
 * MPQUIC Configuration Editor
 *
 * Per-tunnel configuration form that enforces parameter classification:
 *   Category A (hot-reload) — editable, no restart required
 *   Category B (restart)    — editable, restart required after save
 *   Category C (blocked)    — read-only, server-coupled
 *
 * Data flows: LuCI → rpcd (ubus) → TBOX mpquic-mgmt API
 */

var callTunnels       = rpc.declare({ object: 'mpquic', method: 'tunnels' });
var callTunnelConfig  = rpc.declare({ object: 'mpquic', method: 'tunnel_config',          params: ['name'] });
var callConfigSet     = rpc.declare({ object: 'mpquic', method: 'tunnel_config_set',      params: ['name', 'config', 'auto_restart'] });
var callConfigValidate = rpc.declare({ object: 'mpquic', method: 'tunnel_config_validate', params: ['name', 'config'] });

/* ── Parameter metadata ───────────────────────────────────────────────── */

var PARAM_META = {
	/* Category A — hot-reload, no restart */
	log_level:             { cat: 'A', label: 'Log Level',             type: 'select', choices: ['error', 'warning', 'info', 'debug'] },
	stripe_pacing_rate:    { cat: 'A', label: 'Pacing Rate (Mbps)',    type: 'number', min: 0, max: 10000 },
	stripe_fec_mode:       { cat: 'A', label: 'FEC Mode',             type: 'select', choices: ['off', 'adaptive', 'always'] },
	multipath_policy:      { cat: 'A', label: 'Multipath Policy',     type: 'select', choices: ['aggregate', 'redundant', 'failover'] },

	/* Category B — requires restart */
	tun_mtu:               { cat: 'B', label: 'TUN MTU',              type: 'number', min: 1280, max: 9000 },
	congestion_algorithm:  { cat: 'B', label: 'Congestion Algorithm',  type: 'select', choices: ['cubic', 'bbr'] },
	transport_mode:        { cat: 'B', label: 'Transport Mode',        type: 'select', choices: ['quic', 'quic-dgram'] },
	stripe_arq:            { cat: 'B', label: 'ARQ Enabled',           type: 'bool' },
	stripe_fec_type:       { cat: 'B', label: 'FEC Type',             type: 'select', choices: ['xor', 'reed-solomon'] },
	stripe_fec_window:     { cat: 'B', label: 'FEC Window',           type: 'number', min: 1, max: 64 },
	stripe_fec_interleave: { cat: 'B', label: 'FEC Interleave',       type: 'number', min: 0, max: 64 },
	stripe_disable_gso:    { cat: 'B', label: 'Disable GSO',          type: 'bool' },
	detect_starlink:       { cat: 'B', label: 'Detect Starlink',      type: 'bool' },
	starlink_default_pipes: { cat: 'B', label: 'Starlink Default Pipes', type: 'number', min: 1, max: 32 },
	starlink_transport:    { cat: 'B', label: 'Starlink Transport',    type: 'select', choices: ['quic', 'quic-dgram'] },
	stripe_enabled:        { cat: 'B', label: 'Stripes Enabled',      type: 'bool' },

	/* Category C — server-coupled, read-only */
	role:                  { cat: 'C', label: 'Role',                  type: 'text' },
	bind_ip:               { cat: 'C', label: 'Bind IP',              type: 'text' },
	remote_addr:           { cat: 'C', label: 'Remote Address',        type: 'text' },
	remote_port:           { cat: 'C', label: 'Remote Port',           type: 'text' },
	tun_name:              { cat: 'C', label: 'TUN Name',             type: 'text' },
	tun_cidr:              { cat: 'C', label: 'TUN CIDR',             type: 'text' },
	stripe_port:           { cat: 'C', label: 'Stripe Port',          type: 'text' },
	stripe_data_shards:    { cat: 'C', label: 'Data Shards (K)',      type: 'text' },
	stripe_parity_shards:  { cat: 'C', label: 'Parity Shards (M)',    type: 'text' },
	tls_cert:              { cat: 'C', label: 'TLS Certificate',      type: 'text' },
	tls_key:               { cat: 'C', label: 'TLS Key',              type: 'text' },
	tls_ca:                { cat: 'C', label: 'TLS CA',               type: 'text' },
	tls_server_name:       { cat: 'C', label: 'TLS Server Name',      type: 'text' },
	metrics_listen:        { cat: 'C', label: 'Metrics Listen',       type: 'text' },
	control_api_listen:    { cat: 'C', label: 'Control API Listen',    type: 'text' },
	control_api_auth_token: { cat: 'C', label: 'Control API Token',   type: 'text' },
};

var CAT_LABELS = {
	'A': { name: 'Category A — Hot-reload', color: '#22a722', desc: 'Changes applied instantly without restart' },
	'B': { name: 'Category B — Restart Required', color: '#e6a200', desc: 'Changes require tunnel restart to take effect' },
	'C': { name: 'Category C — Server-coupled (Read Only)', color: '#d32f2f', desc: 'Cannot be modified — requires matching server configuration' },
};

/* ── Helpers ───────────────────────────────────────────────────────────── */

function catBadge(cat) {
	var info = CAT_LABELS[cat] || { name: cat, color: '#888' };
	return E('span', {
		'style': 'display:inline-block;padding:1px 6px;border-radius:3px;font-size:0.75em;' +
		          'font-weight:bold;color:#fff;background:' + info.color,
		'title': info.desc || ''
	}, cat);
}

/* ── View ──────────────────────────────────────────────────────────────── */

return view.extend({
	title: _('MPQUIC Tunnels — Configuration'),

	selectedTunnel: null,
	tunnelData: null,
	configData: null,

	load: function() {
		return callTunnels();
	},

	render: function(tunnelsResp) {
		var tunnels = tunnelsResp.tunnels || tunnelsResp.instances || [];
		if (!Array.isArray(tunnels) && typeof tunnels === 'object')
			tunnels = Object.values(tunnels);

		this.tunnelData = tunnels;

		/* ── Tunnel selector ──────────────────────────────────────── */
		var options = tunnels.map(function(t) {
			var name = t.name || t.instance || '';
			return E('option', { 'value': name }, name + ' [' + (t.state || '?') + ']');
		});
		options.unshift(E('option', { 'value': '' }, '— Select tunnel —'));

		var selector = E('select', {
			'class': 'cbi-input-select',
			'style': 'min-width:260px;margin-right:12px',
			'change': ui.createHandlerFn(this, function(ev) {
				this.selectedTunnel = ev.target.value;
				if (this.selectedTunnel)
					return this.loadConfig(this.selectedTunnel);
				else
					dom.content(document.querySelector('#mpquic-config-form'), '');
			})
		}, options);

		var body = E('div', {}, [
			E('h2', {}, _('MPQUIC Tunnels — Configuration')),
			E('div', { 'class': 'cbi-section', 'style': 'margin-bottom:16px' }, [
				E('label', { 'style': 'margin-right:8px;font-weight:bold' }, 'Tunnel:'),
				selector,
			]),
			E('div', { 'id': 'mpquic-config-form' }),
		]);

		return body;
	},

	loadConfig: function(name) {
		var formEl = document.querySelector('#mpquic-config-form');
		dom.content(formEl, E('p', { 'style': 'color:#888' }, 'Loading configuration…'));

		return callTunnelConfig(name).then(L.bind(function(resp) {
			this.configData = resp.config || resp;
			dom.content(formEl, this.renderConfigForm(name, this.configData));
		}, this)).catch(function(err) {
			dom.content(formEl, E('div', { 'class': 'alert-message warning' },
				E('p', {}, 'Error loading config: ' + (err.message || err))));
		});
	},

	renderConfigForm: function(name, config) {
		var sections = { 'A': [], 'B': [], 'C': [] };
		var allKeys = Object.keys(PARAM_META);

		/* Group parameters by category */
		allKeys.forEach(function(key) {
			var meta = PARAM_META[key];
			var value = config[key];
			if (value === undefined) value = '';
			sections[meta.cat].push({ key: key, meta: meta, value: value });
		});

		/* Also add unknown keys from config (show in Cat. C as read-only) */
		Object.keys(config).forEach(function(key) {
			if (!PARAM_META[key]) {
				sections['C'].push({ key: key, meta: { cat: 'C', label: key, type: 'text' }, value: config[key] });
			}
		});

		var formSections = ['A', 'B', 'C'].map(L.bind(function(cat) {
			var info = CAT_LABELS[cat];
			var fields = sections[cat].map(L.bind(function(f) {
				return this.renderField(f.key, f.meta, f.value);
			}, this));

			return E('fieldset', { 'class': 'cbi-section', 'style': 'margin-bottom:20px;border-left:4px solid ' + info.color + ';padding-left:12px' }, [
				E('legend', { 'style': 'font-weight:bold;font-size:1.1em;color:' + info.color }, [
					catBadge(cat), ' ', info.name
				]),
				E('p', { 'style': 'color:#666;font-size:0.85em;margin:4px 0 12px 0' }, info.desc),
				E('table', { 'class': 'table', 'style': 'width:100%' }, fields),
			]);
		}, this));

		/* ── Action buttons ───────────────────────────────────────── */
		var actions = E('div', { 'style': 'margin-top:16px;display:flex;gap:12px' }, [
			E('button', {
				'class': 'cbi-button cbi-button-action',
				'click': ui.createHandlerFn(this, 'handleValidate', name)
			}, 'Validate'),
			E('button', {
				'class': 'cbi-button cbi-button-apply',
				'click': ui.createHandlerFn(this, 'handleApply', name, false)
			}, 'Apply'),
			E('button', {
				'class': 'cbi-button cbi-button-apply',
				'style': 'background:#e6a200',
				'click': ui.createHandlerFn(this, 'handleApply', name, true)
			}, 'Apply + Restart'),
		]);

		return E('div', {}, formSections.concat([actions]));
	},

	renderField: function(key, meta, value) {
		var isReadOnly = (meta.cat === 'C');
		var inputEl;

		switch (meta.type) {
		case 'select':
			if (isReadOnly) {
				inputEl = E('input', {
					'type': 'text', 'value': String(value), 'readonly': 'readonly',
					'class': 'cbi-input-text', 'data-key': key,
					'style': 'background:#eee;color:#888;width:250px'
				});
			} else {
				var opts = (meta.choices || []).map(function(c) {
					return E('option', { 'value': c, 'selected': (String(value) === c) ? 'selected' : null }, c);
				});
				inputEl = E('select', {
					'class': 'cbi-input-select', 'data-key': key, 'style': 'width:250px'
				}, opts);
			}
			break;

		case 'bool':
			if (isReadOnly) {
				inputEl = E('input', {
					'type': 'text', 'value': String(!!value), 'readonly': 'readonly',
					'class': 'cbi-input-text', 'data-key': key,
					'style': 'background:#eee;color:#888;width:100px'
				});
			} else {
				inputEl = E('input', {
					'type': 'checkbox', 'data-key': key,
					'checked': value ? 'checked' : null,
					'style': 'width:20px;height:20px'
				});
			}
			break;

		case 'number':
			inputEl = E('input', {
				'type': 'number', 'value': String(value != null ? value : ''),
				'class': 'cbi-input-text', 'data-key': key,
				'min': meta.min, 'max': meta.max,
				'readonly': isReadOnly ? 'readonly' : null,
				'style': (isReadOnly ? 'background:#eee;color:#888;' : '') + 'width:150px'
			});
			break;

		default: /* text */
			inputEl = E('input', {
				'type': 'text', 'value': String(value != null ? value : ''),
				'class': 'cbi-input-text', 'data-key': key,
				'readonly': isReadOnly ? 'readonly' : null,
				'style': (isReadOnly ? 'background:#eee;color:#888;' : '') + 'width:250px'
			});
		}

		return E('tr', {}, [
			E('td', { 'style': 'width:200px;padding:4px 8px;vertical-align:middle' }, [
				catBadge(meta.cat), ' ',
				E('strong', {}, meta.label)
			]),
			E('td', { 'style': 'padding:4px 8px' }, inputEl),
		]);
	},

	/* ── Collect form values (Cat.A + Cat.B only) ─────────────────── */
	collectChanges: function() {
		var changes = {};
		var inputs = document.querySelectorAll('#mpquic-config-form [data-key]');

		inputs.forEach(L.bind(function(el) {
			var key = el.getAttribute('data-key');
			var meta = PARAM_META[key];
			if (!meta || meta.cat === 'C') return; /* skip read-only */

			var newVal;
			switch (meta.type) {
			case 'bool':
				newVal = el.checked;
				break;
			case 'number':
				newVal = el.value !== '' ? Number(el.value) : null;
				break;
			default:
				newVal = el.value;
			}

			/* Only include changed values */
			var origVal = this.configData ? this.configData[key] : undefined;
			if (meta.type === 'bool') {
				if (newVal !== !!origVal) changes[key] = newVal;
			} else if (meta.type === 'number') {
				if (newVal !== origVal) changes[key] = newVal;
			} else {
				if (String(newVal) !== String(origVal || '')) changes[key] = newVal;
			}
		}, this));

		return changes;
	},

	handleValidate: function(name, ev) {
		var changes = this.collectChanges();

		if (Object.keys(changes).length === 0) {
			ui.addNotification(null, E('p', {}, 'No changes to validate'), 'info');
			return;
		}

		return callConfigValidate(name, JSON.stringify(changes)).then(function(resp) {
			if (resp.error) {
				ui.addNotification(null, E('p', {}, '✗ Validation failed: ' + resp.error), 'warning');
			} else {
				var msg = '✓ Validation passed';
				if (resp.needs_restart) msg += ' (restart required)';
				ui.addNotification(null, E('p', {}, msg), 'info');
			}
		}).catch(function(err) {
			ui.addNotification(null, E('p', {}, 'Validation error: ' + (err.message || err)), 'danger');
		});
	},

	handleApply: function(name, autoRestart, ev) {
		var changes = this.collectChanges();

		if (Object.keys(changes).length === 0) {
			ui.addNotification(null, E('p', {}, 'No changes to apply'), 'info');
			return;
		}

		var msg = 'Apply changes to ' + name + '?\n\n';
		msg += Object.keys(changes).map(function(k) {
			return '  ' + k + ' = ' + JSON.stringify(changes[k]);
		}).join('\n');

		if (autoRestart) msg += '\n\nTunnel will be restarted automatically.';

		if (!confirm(msg)) return;

		return callConfigSet(name, JSON.stringify(changes), autoRestart).then(L.bind(function(resp) {
			if (resp.error) {
				ui.addNotification(null, E('p', {}, '✗ Apply failed: ' + resp.error), 'warning');
			} else {
				var info = '✓ Configuration applied to ' + name;
				if (resp.needs_restart && resp.restart_applied) info += ' — tunnel restarted';
				else if (resp.needs_restart) info += ' — restart required';
				ui.addNotification(null, E('p', {}, info), 'info');
				/* Reload config to reflect saved state */
				return this.loadConfig(name);
			}
		}, this)).catch(function(err) {
			ui.addNotification(null, E('p', {}, 'Apply error: ' + (err.message || err)), 'danger');
		});
	},

	handleSaveApply: null,
	handleSave: null,
	handleReset: null,
});
