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

  function renderEnvelope(env) {
    if (!env || !env.id || cards.has(env.id)) return;
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
    notify(env);
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
        list.forEach(renderEnvelope);
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
    ws.onopen = () => setStatus('live', 'ok');
    ws.onclose = () => {
      setStatus('disconnected — retrying', 'err');
      setTimeout(connect, 2000);
    };
    ws.onerror = () => setStatus('error', 'err');
    ws.onmessage = (msg) => {
      try {
        const ev = JSON.parse(msg.data);
        if (ev.kind === 'envelope' && ev.envelope) {
          renderEnvelope(ev.envelope);
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
