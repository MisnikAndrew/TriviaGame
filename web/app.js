'use strict';

/* ---------------------------------------------------------------------------
   Клиент «Борьбы умов». Сервер присылает по SSE полный снимок того, что нужно
   показать (state), а любое действие игрока — обычный POST. Поэтому весь UI —
   это функция render(state); переподключение ничего не ломает.
   Весь пользовательский текст попадает в DOM через textContent (см. h()).
--------------------------------------------------------------------------- */

const safe = (fn) => { try { return fn(); } catch { return null; } };
const lget = (k) => safe(() => localStorage.getItem(k));
const lset = (k, v) => safe(() => localStorage.setItem(k, v));
const sget = (k) => safe(() => sessionStorage.getItem(k));
const sset = (k, v) => safe(() => sessionStorage.setItem(k, v));

function randomHex(bytes) {
  const b = new Uint8Array(bytes);
  crypto.getRandomValues(b); // доступно и по http в локальной сети, в отличие от randomUUID
  return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
}

// id игрока живёт в sessionStorage: переживает перезагрузку вкладки, но у каждой
// вкладки свой — можно сыграть сам с собой в двух вкладках.
let pid = sget('pid');
if (!pid) { pid = randomHex(12); sset('pid', pid); }
let name = lget('name') || 'Игрок' + (100 + Math.floor(Math.random() * 900));

let state = null;        // последнее состояние от сервера
let info = null;         // /api/info
let es = null;           // EventSource
let timer = null;        // { deadline, total } для полосы времени
let nameDraft = null;    // несохранённый ввод имени
let codeDraft = '';      // введённый код комнаты
let leaveArmed = false;  // «Сдаться» нажато один раз
let lastKey = '';
let inviteCode = (new URLSearchParams(location.search).get('room') || '')
  .toUpperCase().replace(/[^A-Z0-9]/g, '').slice(0, 8);

const app = document.getElementById('app');

/* ---------- маленький hyperscript ---------- */
function h(tag, props, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') e.className = v;
    else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else if (k === 'style') Object.assign(e.style, v);
    else e.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    e.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return e;
}

// Same as h(), for SVG elements.
function svg(tag, props, ...kids) {
  const e = document.createElementNS('http://www.w3.org/2000/svg', tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    e.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return e;
}

const plural = (n, one, few, many) => {
  const m10 = n % 10, m100 = n % 100;
  if (m10 === 1 && m100 !== 11) return one;
  if (m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14)) return few;
  return many;
};
const stripBot = (s) => s.replace(/^🤖\s*/, '');

/* ---------- сеть ---------- */
async function api(path, body = {}) {
  try {
    const r = await fetch('/api/' + path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ pid, name, ...body }),
    });
    if (r.ok) return true;
    const j = await r.json().catch(() => ({}));
    if (r.status === 404) connect(); // сервер перезапускался и нас не помнит
    toast(j.error || 'Что-то пошло не так');
  } catch {
    toast('Нет связи с сервером');
  }
  return false;
}

function connect() {
  if (es) { es.onerror = null; es.close(); }
  const src = new EventSource(`/api/events?pid=${pid}&name=${encodeURIComponent(name)}`);
  es = src;
  src.onopen = () => banner(false);
  src.onerror = () => { // переподключаемся сами, чтобы всегда использовать актуальное имя
    banner(true);
    src.onerror = null;
    src.close();
    setTimeout(() => { if (es === src) connect(); }, 1500);
  };
  src.addEventListener('state', (e) => onState(JSON.parse(e.data)));
  src.addEventListener('replaced', () => { // та же игра открыта в другой вкладке
    src.onerror = null;
    src.close();
    banner(false);
    timer = null;
    state = { screen: 'replaced' };
    render();
  });
}

function onState(st) {
  state = st;
  if (st.name) { name = st.name; lset('name', name); }
  const g = st.game;
  timer = g && (g.phase === 'pick' || g.phase === 'question')
    ? { deadline: performance.now() + g.timeLeftMs, total: g.timeTotalMs }
    : null;
  if (!g || g.phase === 'final') leaveArmed = false;
  render();
}

function banner(show) { document.getElementById('banner').hidden = !show; }

let toastTimer;
function toast(msg) {
  const t = document.getElementById('toast');
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, 3200);
}

/* ---------- действия ---------- */
async function saveName() {
  if (nameDraft == null) return;
  const v = nameDraft.trim();
  nameDraft = null;
  if (v && v !== name) {
    name = v;
    lset('name', v);
    await api('name', { name: v });
  }
}

async function play(kind) {
  await saveName();
  await api(kind);
}

async function joinByCode() {
  const code = codeDraft.trim().toUpperCase();
  if (!code) return;
  await saveName();
  if (await api('join', { code })) codeDraft = '';
}

async function joinInvite() {
  await saveName();
  const code = inviteCode;
  inviteCode = '';
  history.replaceState(null, '', location.pathname);
  if (!(await api('join', { code }))) render();
}

function pickCategory(index) { api('pick', { index }); }

function answer(index) {
  const g = state && state.game;
  if (!g || g.phase !== 'question' || g.myAnswer >= 0) return;
  g.myAnswer = index; // сразу подсвечиваем, не дожидаясь сервера
  render();
  api('answer', { seq: g.question.seq, index });
}

function shareBase() {
  if (info && info.publicUrl) return info.publicUrl; // друг подключится из интернета
  const local = ['localhost', '127.0.0.1', '[::1]'].includes(location.hostname);
  if (local && info && info.urls && info.urls.length) {
    // с localhost друг не откроется — подставляем адрес в локальной сети
    return info.urls.find((u) => /\/\/(192\.168|10\.|172\.(1[6-9]|2\d|3[01]))\./.test(u)) || info.urls[0];
  }
  return location.origin;
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast('Ссылка скопирована');
    return;
  } catch { /* нет secure context — пробуем по-старому */ }
  const ta = h('textarea', { style: { position: 'fixed', opacity: '0' } });
  ta.value = text;
  document.body.append(ta);
  ta.select();
  try { document.execCommand('copy'); toast('Ссылка скопирована'); } catch { toast('Скопируйте ссылку вручную'); }
  ta.remove();
}

/* ---------- общие компоненты ---------- */
function avatar(p) {
  let hash = 0;
  for (const ch of p.name) hash = (hash * 31 + ch.codePointAt(0)) >>> 0;
  const letter = p.bot ? '🤖' : ([...p.name][0] || '?').toUpperCase();
  return h('div', { class: 'avatar', style: { background: `hsl(${hash % 360} 60% 42%)` } }, letter);
}

function playerBox(p, side, gain) {
  return h('div', { class: `player ${side}${p.connected ? '' : ' off'}` },
    avatar(p),
    h('div', { class: 'pname' }, stripBot(p.name)),
    h('div', { class: 'pscore' }, p.score),
    gain ? h('div', { class: 'gain' }, '+' + gain) : null);
}

function header(g) {
  const reveal = g.phase === 'reveal';
  return h('div', { class: 'hdr' },
    playerBox(g.me, 'me', reveal ? g.myPoints : 0),
    h('div', { class: 'mid' },
      h('div', { class: 'rnd' }, `Раунд ${g.round}/${g.rounds}`),
      g.double ? h('div', { class: 'x2tag' }, '×2 очки') : null),
    playerBox(g.opp, 'opp', reveal ? g.oppPoints : 0));
}

const dots = (arr) => h('div', { class: 'dots' }, arr.map((v) => h('i', { class: 'dot d' + v })));

function board(g) {
  return h('div', { class: 'card board' }, g.board.map((r, i) =>
    h('div', { class: 'brow' + (g.phase !== 'final' && i === g.round - 1 ? ' cur' : '') },
      dots(r.me),
      h('div', { class: 'bcat' },
        r.cat
          ? [h('span', { class: 'bicon' }, r.cat.icon), h('span', { class: 'bname' }, r.cat.name)]
          : h('span', { class: 'bwait' }, `Раунд ${i + 1}`),
        i === g.rounds - 1 ? h('span', { class: 'x2' }, '×2') : null),
      dots(r.opp))));
}

const timerBar = () => h('div', { class: 'timer' }, h('div', { id: 'timerFill', class: 'tfill' }));

function leaveButton(label) {
  return h('div', { class: 'leave' },
    h('button', {
      class: 'btn ghost small',
      onclick: () => {
        if (!leaveArmed) {
          leaveArmed = true;
          render();
          setTimeout(() => { if (leaveArmed) { leaveArmed = false; render(); } }, 3000);
          return;
        }
        leaveArmed = false;
        api('leave');
      },
    }, leaveArmed ? 'Точно сдаться? Нажмите ещё раз' : label));
}

function bigButton(icon, title, sub, cls, onclick) {
  return h('button', { class: 'btn ' + cls, onclick },
    h('span', { class: 'ico' }, icon),
    h('span', null, title, h('span', { class: 'sub' }, sub)));
}

/* ---------- экраны ---------- */
function menuView() {
  const v = [
    h('div', { class: 'hero' },
      h('div', { class: 'logo' }, '🧠'),
      h('h1', null, 'Борьба умов'),
      h('p', null, 'Дуэль знаний один на один')),
  ];

  if (inviteCode) {
    v.push(h('div', { class: 'card invite stack' },
      h('div', null, 'Вас приглашают в комнату ', h('span', { class: 'code' }, inviteCode)),
      h('button', { class: 'btn gold block', onclick: joinInvite }, 'Присоединиться')));
  }

  v.push(h('div', { class: 'card' },
    h('label', { class: 'label', for: 'nameInput' }, 'Ваше имя'),
    h('input', {
      id: 'nameInput', class: 'input', maxlength: 16, autocomplete: 'off',
      value: nameDraft ?? name,
      oninput: (e) => { nameDraft = e.target.value; },
      onchange: saveName,
      onkeydown: (e) => { if (e.key === 'Enter') e.target.blur(); },
    })));

  v.push(h('div', { class: 'stack' },
    bigButton('⚡', 'Быстрая игра', 'Случайный соперник онлайн', 'primary', () => play('quick')),
    bigButton('👥', 'Играть с другом', 'Создайте комнату и отправьте ссылку', '', () => play('room')),
    bigButton('🤖', 'Против бота', 'Потренироваться в одиночку', '', () => play('bot'))));

  v.push(h('div', { class: 'card' },
    h('label', { class: 'label', for: 'codeInput' }, 'Есть код комнаты?'),
    h('div', { class: 'row' },
      h('input', {
        id: 'codeInput', class: 'input', maxlength: 8, autocomplete: 'off', placeholder: 'ABCD',
        style: { textTransform: 'uppercase' }, value: codeDraft,
        oninput: (e) => { codeDraft = e.target.value; },
        onkeydown: (e) => { if (e.key === 'Enter') joinByCode(); },
      }),
      h('button', { class: 'btn small', onclick: joinByCode }, 'Войти'))));

  if (info) {
    const sec = Math.round(info.questionMs / 1000);
    v.push(h('div', { class: 'card' }, h('details', null,
      h('summary', null, 'Как играть'),
      h('ul', null,
        h('li', null, `В матче ${info.rounds} ${plural(info.rounds, 'раунд', 'раунда', 'раундов')}, в каждом — ${info.qPerRound} ${plural(info.qPerRound, 'вопрос', 'вопроса', 'вопросов')} по одной теме.`),
        h('li', null, 'Тему раунда выбирает то один игрок, то другой — из трёх случайных.'),
        h('li', null, `На ответ — ${sec} ${plural(sec, 'секунда', 'секунды', 'секунд')}. Верный ответ даёт 50–100 очков: чем быстрее, тем больше.`),
        h('li', null, 'В последнем раунде очки удваиваются.'),
        h('li', null, 'Побеждает тот, у кого больше очков.')))));
    v.push(h('div', { class: 'card' },
      h('div', { class: 'label' }, `${info.categories.length} тем, ${info.questions} вопросов`),
      h('div', { class: 'chips' }, info.categories.map((c) => h('span', { class: 'chip' }, `${c.icon} ${c.name}`)))));
  }
  return v;
}

function queueView() {
  return [h('div', { class: 'wait' },
    h('div', { class: 'spinner' }),
    h('h2', null, 'Ищем соперника…'),
    h('p', { class: 'muted' }, 'Если никого нет рядом, можно сыграть с ботом.'),
    h('button', { class: 'btn primary block', onclick: async () => { await api('cancel'); await api('bot'); } }, '🤖 Играть с ботом'),
    h('button', { class: 'btn ghost small', onclick: () => api('cancel') }, 'Отмена'))];
}

function roomView(code) {
  const link = `${shareBase()}/?room=${code}`;
  return [h('div', { class: 'wait' },
    h('p', { class: 'muted' }, 'Код комнаты'),
    h('div', { class: 'bigcode' }, code),
    h('p', { class: 'muted' }, 'Отправьте другу ссылку — достаточно открыть её в браузере (устройства должны быть в одной сети):'),
    h('div', { class: 'linkbox' }, link),
    h('button', { class: 'btn gold block', onclick: () => copyText(link) }, '📋 Скопировать ссылку'),
    h('div', { class: 'spinner' }),
    h('p', { class: 'muted' }, 'Ждём друга…'),
    h('button', { class: 'btn ghost small', onclick: () => api('cancel') }, 'Отмена'))];
}

function replacedView() {
  return [h('div', { class: 'wait' },
    h('h2', null, 'Игра открыта в другой вкладке'),
    h('p', { class: 'muted' }, 'Одновременно можно играть только в одной вкладке с этим игроком.'),
    h('button', { class: 'btn primary block', onclick: () => { state = null; render(); connect(); } }, 'Играть здесь'))];
}

function gameView(g) {
  const lost = g.phase !== 'final' && !g.opp.connected && !g.opp.bot
    ? h('div', { class: 'status bad' }, 'Соперник потерял соединение…') : null;

  switch (g.phase) {
    case 'pick': return [header(g), board(g), pickPanel(g), lost, leaveButton('Сдаться и выйти')];
    case 'intro': return [header(g), introPanel(g), leaveButton('Сдаться и выйти')];
    case 'question':
    case 'reveal': return [header(g), ...questionPanel(g), lost, leaveButton('Сдаться и выйти')];
    default: return finalView(g);
  }
}

function pickPanel(g) {
  return h('div', { class: 'stack' },
    h('div', { class: 'title' }, g.iPick ? 'Выберите категорию' : `${stripBot(g.chooserName)} выбирает категорию…`),
    timerBar(),
    wheel(g.choices, g.iPick ? pickCategory : null));
}

// Splits a category name into lines of about `max` characters at word borders.
function wrapName(name, max = 11) {
  const lines = [];
  let cur = '';
  for (const word of name.split(/\s+/)) {
    if (cur && cur.length + 1 + word.length > max) { lines.push(cur); cur = word; }
    else cur = cur ? `${cur} ${word}` : word;
  }
  lines.push(cur);
  return lines;
}

// The category wheel: one coloured sector per category, the first one centred
// at the top. Without onPick the wheel is only shown (the opponent's view).
function wheel(choices, onPick) {
  const C = 200, R = 190, step = 360 / choices.length;
  const at = (deg, r) => [C + r * Math.cos(deg * Math.PI / 180), C + r * Math.sin(deg * Math.PI / 180)];
  const f = (n) => n.toFixed(1);

  const root = svg('svg', {
    class: onPick ? 'wheel live' : 'wheel idle', viewBox: '0 0 400 400',
    role: 'group', 'aria-label': 'Категории',
  });
  choices.forEach((c, i) => {
    const from = -90 - step / 2 + i * step;
    const [x0, y0] = at(from, R), [x1, y1] = at(from + step, R);
    const [lx, ly] = at(from + step / 2, R * 0.62);

    const lines = wrapName(c.name);
    const size = Math.max(10, Math.min(16, 16 * 11 / Math.max(...lines.map((l) => l.length))));
    const iconSize = 30, lineH = size * 1.2;
    const top = ly - (iconSize + 4 + lines.length * lineH) / 2;

    const choose = () => {
      if (root.classList.contains('locked')) return;
      root.classList.add('locked');
      sector.classList.add('chosen');
      onPick(i);
    };
    const sector = svg('g', {
      class: `sector w${i % 5}`, 'aria-label': c.name,
      ...(onPick ? {
        role: 'button', tabindex: 0, onclick: choose,
        onkeydown: (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); choose(); } },
      } : {}),
    },
      svg('path', { d: `M${C} ${C}L${f(x0)} ${f(y0)}A${R} ${R} 0 0 1 ${f(x1)} ${f(y1)}Z` }),
      svg('text', { class: 'wicon', x: f(lx), y: f(top + iconSize * 0.85), 'text-anchor': 'middle', 'font-size': iconSize }, c.icon),
      svg('text', { class: 'wname', 'text-anchor': 'middle', 'font-size': f(size) },
        lines.map((l, k) => svg('tspan', { x: f(lx), y: f(top + iconSize + 4 + size + k * lineH) }, l))));
    root.append(sector);
  });
  root.append(
    svg('circle', { class: 'ring', cx: C, cy: C, r: R + 3 }),
    svg('g', { class: 'hub' }, svg('circle', { cx: C, cy: C, r: 32 }),
      svg('text', { x: C, y: C + 12, 'text-anchor': 'middle', 'font-size': 34 }, '🧠')));
  return root;
}

function introPanel(g) {
  return h('div', { class: 'intro' },
    h('div', { class: 'big' }, g.cat.icon),
    h('h2', null, g.cat.name),
    h('p', { class: 'muted' }, `Раунд ${g.round} из ${g.rounds}`),
    g.double ? h('div', { class: 'x2tag' }, 'Последний раунд — очки ×2') : null);
}

function questionPanel(g) {
  const q = g.question;
  const reveal = g.phase === 'reveal';
  const opts = q.options.map((text, i) => {
    let cls = 'opt';
    if (reveal) {
      cls += ' locked ' + (i === g.correct ? 'correct' : i === g.myAnswer ? 'wrong' : 'dim');
    } else if (g.myAnswer >= 0) {
      cls += ' locked' + (i === g.myAnswer ? ' picked' : '');
    }
    return h('button', { class: cls, onclick: () => answer(i) },
      h('span', { class: 'letter' }, 'ABCD'[i]),
      h('span', { class: 'otext' }, text),
      reveal && i === g.oppAnswer ? h('span', { class: 'oppmark' }, stripBot(g.opp.name)) : null);
  });

  let status = h('div', { class: 'status' }, ' ');
  if (reveal) {
    if (g.myAnswer === g.correct) status = h('div', { class: 'status good' }, `Верно! +${g.myPoints}`);
    else if (g.myAnswer < 0) status = h('div', { class: 'status bad' }, 'Время вышло');
    else status = h('div', { class: 'status bad' }, 'Неверно');
  } else if (g.myAnswer >= 0 && !g.oppAnswered) {
    status = h('div', { class: 'status muted' }, 'Ответ принят. Ждём соперника…');
  } else if (g.myAnswer < 0 && g.oppAnswered) {
    status = h('div', { class: 'status muted' }, 'Соперник уже ответил!');
  }

  return [
    h('div', { class: 'qmeta' },
      h('span', null, `${g.cat.icon} ${g.cat.name} · вопрос ${g.qIndex} из ${g.qPerRound}`),
      reveal ? null : h('span', { id: 'timerText', class: 'ttext' })),
    reveal ? h('div', { class: 'timer' }) : timerBar(),
    h('div', { class: 'card qtext' }, q.text),
    h('div', { class: 'opts' }, opts),
    status,
  ];
}

function finalView(g) {
  const look = { win: ['🏆', 'Победа!'], lose: ['😔', 'Поражение'], draw: ['🤝', 'Ничья'] }[g.result] || ['🏁', 'Игра окончена'];

  let label = 'Реванш', cls = 'btn primary block', disabled = false;
  if (g.oppLeft) { label = 'Соперник вышел'; disabled = true; }
  else if (g.rematchMe) { label = 'Ждём соперника…'; disabled = true; }
  else if (g.rematchOpp) { label = 'Принять реванш'; cls = 'btn gold block'; }

  return [
    h('div', { class: 'result' },
      h('div', { class: 'emoji' }, look[0]),
      h('h2', null, look[1]),
      g.forfeit ? h('p', { class: 'muted' }, 'Соперник покинул игру — победа засчитана') : null,
      h('div', { class: 'final-score' }, g.me.score, h('span', null, ':'), g.opp.score),
      h('p', { class: 'muted' }, `${g.me.name} — ${stripBot(g.opp.name)}`)),
    board(g),
    h('div', { class: 'stack' },
      h('button', { class: cls, disabled, onclick: () => api('rematch') }, label),
      h('button', { class: 'btn block', onclick: () => api('leave') }, 'В меню')),
  ];
}

/* ---------- отрисовка ---------- */
function render() {
  if (!state) {
    app.replaceChildren(h('div', { class: 'wait' }, h('div', { class: 'spinner' }), h('p', { class: 'muted' }, 'Подключаемся…')));
    return;
  }
  let view;
  switch (state.screen) {
    case 'menu': view = menuView(); break;
    case 'queue': view = queueView(); break;
    case 'room': view = roomView(state.room); break;
    case 'game': view = gameView(state.game); break;
    default: view = replacedView(); break;
  }
  // анимацию «появления» играем только при смене экрана, а не при каждом снимке
  const g = state.game;
  const key = state.screen + (g ? `:${g.phase === 'reveal' ? 'question' : g.phase}:${g.round}:${g.qIndex}` : '');
  app.className = key !== lastKey ? 'enter' : '';
  lastKey = key;
  app.replaceChildren(...view.filter(Boolean));
}

function tick() {
  if (timer) {
    const fill = document.getElementById('timerFill');
    if (fill) {
      const left = Math.max(0, timer.deadline - performance.now());
      fill.style.transform = `scaleX(${timer.total ? left / timer.total : 0})`;
      fill.classList.toggle('low', left < 5000);
      const t = document.getElementById('timerText');
      if (t) t.textContent = Math.ceil(left / 1000);
    }
  }
  requestAnimationFrame(tick);
}

document.addEventListener('keydown', (e) => {
  const g = state && state.game;
  if (!g || g.phase !== 'question' || e.target.tagName === 'INPUT') return;
  const i = '1234'.indexOf(e.key);
  if (i >= 0) answer(i);
});

document.addEventListener('visibilitychange', () => {
  if (!document.hidden && (!es || es.readyState !== EventSource.OPEN)) connect();
});

fetch('/api/info').then((r) => r.json()).then((i) => {
  info = i;
  if (state && state.screen === 'menu') render();
}).catch(() => {});

render();
connect();
requestAnimationFrame(tick);
