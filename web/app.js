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

  async function bootstrap() {
    if ('Notification' in window && Notification.permission === 'default') {
      try {
        await Notification.requestPermission();
      } catch (_) {
        /* ignore */
      }
    }
    try {
      const res = await fetch(`${apiBase}/queue?t=${encodeURIComponent(token)}`);
      if (res.ok) {
        const list = await res.json();
        // Backlog cards render SILENTLY: (re)opening the tab with N pending
        // cards must not fire N browser notifications at once.
        list.forEach((env) => renderEnvelope(env, { silent: true }));
      }
    } catch (_) {
      /* ws will fill in */
    }
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
