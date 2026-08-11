(() => {
  const $queue = document.getElementById('queue');
  const $status = document.getElementById('status');
  const tpl = document.getElementById('card-tpl');
  const params = new URLSearchParams(window.location.search);
  const token = params.get('t') || '';
  const wsScheme = location.protocol === 'https:' ? 'wss' : 'ws';
  const wsURL = `${wsScheme}://${location.host}/ws?t=${encodeURIComponent(token)}`;
  const apiBase = `${location.protocol}//${location.host}/api`;

  const cards = new Map();

  function setStatus(text, mode) {
    $status.textContent = text;
    $status.className = `status ${mode}`;
  }

  function fmtTime(iso) {
    const d = new Date(iso);
    if (isNaN(d.getTime())) return '';
    return d.toLocaleTimeString();
  }

  // renderEnvelope adds a card for env. Pass { silent: true } to render a
  // backlog card WITHOUT firing a browser Notification — the initial snapshot
  // (GET /api/queue and the WebSocket bootstrap frames) can carry N pending
  // cards, and notifying for each would fire N notifications at once every time
  // a phone tab (re)loads. Only cards that arrive live over the WebSocket after
  // the snapshot completes should notify.
  function renderEnvelope(env, opts) {
    if (!env || !env.id || cards.has(env.id)) return;
    const silent = opts && opts.silent;
    clearEmpty();
    const node = tpl.content.firstElementChild.cloneNode(true);
    node.dataset.id = env.id;
    node.querySelector('.agent').textContent = env.agent_id || 'agent';
    node.querySelector('.time').textContent = fmtTime(env.expires_at);
    node.querySelector('.time').dateTime = env.expires_at || '';
    node.querySelector('.prompt').textContent = env.prompt || '';
    node.querySelector('.context').textContent = env.context || '';
    const $choices = node.querySelector('.choices');
    (env.choices || []).forEach((c) => {
      const btn = document.createElement('button');
      btn.className = 'choice' + (c.is_default ? ' choice--default' : '');
      btn.dataset.key = c.key;
      btn.textContent = c.label || c.key;
      btn.addEventListener('click', () => answer(env.id, c.key, node));
      $choices.appendChild(btn);
    });
    $queue.prepend(node);
    cards.set(env.id, node);
    if (!silent) notify(env);
  }

  function removeCard(id) {
    const node = cards.get(id);
    if (!node) return;
    node.classList.add('card--answered');
    setTimeout(() => {
      node.remove();
      cards.delete(id);
      if (cards.size === 0) renderEmpty();
    }, 220);
  }

  function renderEmpty() {
    if (cards.size > 0) return;
    if ($queue.querySelector('.empty')) return;
    const div = document.createElement('div');
    div.className = 'empty';
    div.textContent = 'queue empty — agents are working.';
    $queue.appendChild(div);
  }

  function clearEmpty() {
    const e = $queue.querySelector('.empty');
    if (e) e.remove();
  }

  async function answer(id, choiceKey, node) {
    node.querySelectorAll('button').forEach((b) => (b.disabled = true));
    try {
      const res = await fetch(
        `${apiBase}/queue/${encodeURIComponent(id)}/answer?t=${encodeURIComponent(token)}`,
        {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ choice_key: choiceKey }),
        },
      );
      if (!res.ok) {
        const text = await res.text();
        node.classList.add('card--error');
        node.querySelectorAll('button').forEach((b) => (b.disabled = false));
        console.error('answer failed', res.status, text);
        return;
      }
      removeCard(id);
    } catch (err) {
      console.error(err);
      node.querySelectorAll('button').forEach((b) => (b.disabled = false));
    }
  }

  function notify(env) {
    if (typeof Notification === 'undefined') return;
    if (Notification.permission !== 'granted') return;
    try {
      new Notification(`${env.agent_id} needs you`, {
        body: (env.prompt || '').slice(0, 120),
      });
    } catch (_) {
      /* notifications best-effort */
    }
  }

  // syncQueueSnapshot reconciles the local cards Map against the daemon's
  // live queue (GET /api/queue). Cards that were answered, expired, or
  // evicted while this phone was disconnected — or dropped from a full
  // slow-subscriber channel — are removed; live cards missing locally are
  // rendered silently (renderEnvelope dedupes). The WS bootstrap burst only
  // pushes LIVE envelopes, so it can add but never tell us what to REMOVE;
  // without this reconcile stale cards linger after a drop and tapping them
  // later fails with 404/409. Runs at startup (bootstrap) and on every
  // ws.onopen so each reconnect reconciles the phone to the daemon's actual
  // live state — the resync internal/daemon/queue.go's Subscribe comment
  // already promises.
  async function syncQueueSnapshot() {
    let list;
    try {
      const res = await fetch(`${apiBase}/queue?t=${encodeURIComponent(token)}`);
      if (!res.ok) return;
      list = await res.json();
    } catch (_) {
      return; /* leave local state untouched; the WS burst will fill in */
    }
    const live = new Set((list || []).map((env) => env.id));
    // Drop local cards no longer live on the daemon.
    for (const id of [...cards.keys()]) {
      if (!live.has(id)) removeCard(id);
    }
    // Render any live card we don't already show (silent — backlog).
    (list || []).forEach((env) => renderEnvelope(env, { silent: true }));
  }

  async function bootstrap() {
    if ('Notification' in window && Notification.permission === 'default') {
      try {
        await Notification.requestPermission();
      } catch (_) {
        /* ignore */
      }
    }
    await syncQueueSnapshot();
    if (cards.size === 0) renderEmpty();
    connect();
  }

  function connect() {
    setStatus('connecting…', 'idle');
    const ws = new WebSocket(wsURL);
    // The daemon replays the current queue as a burst of `envelope` frames
    // immediately after connect (the bootstrap snapshot) before any live
    // events. Those are backlog too — render them silently. Any card already
    // rendered from the REST snapshot is skipped by renderEnvelope's dedupe,
    // and a short settle window flips notifications on for genuinely-live
    // arrivals that come after the replay.
    let liveArmed = false;
    let armTimer = null;
    const armLive = () => {
      liveArmed = true;
      if (armTimer) {
        clearTimeout(armTimer);
        armTimer = null;
      }
    };
    ws.onopen = () => {
      setStatus('live', 'ok');
      // Reconcile against the daemon's live queue: a WS drop cancels the
      // prior subscriber, so answer/expiry/eviction events broadcast during
      // the disconnect never reached this phone, and the bootstrap burst
      // below only pushes LIVE envelopes — it cannot tell us what to REMOVE.
      // Re-fetch /api/queue and drop local cards no longer live; live cards
      // missing locally render silently (renderEnvelope dedupes).
      syncQueueSnapshot();
      // Give the snapshot burst a moment to drain, then treat later frames as live.
      armTimer = setTimeout(armLive, 750);
    };
    ws.onclose = () => {
      setStatus('disconnected — retrying', 'err');
      if (armTimer) clearTimeout(armTimer);
      setTimeout(connect, 2000);
    };
    ws.onerror = () => setStatus('error', 'err');
    ws.onmessage = (msg) => {
      try {
        const ev = JSON.parse(msg.data);
        if (ev.kind === 'envelope' && ev.envelope) {
          // Notify only for live arrivals after the initial snapshot has settled.
          renderEnvelope(ev.envelope, { silent: !liveArmed });
        } else if (ev.kind === 'answer' && ev.answer) {
          removeCard(ev.answer.envelope_id);
        }
      } catch (_) {
        /* malformed frame, skip */
      }
    };
  }

  bootstrap();
})();
