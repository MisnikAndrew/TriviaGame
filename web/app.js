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
let timer = null;        // { deadline, total, frozen } для полосы времени; frozen — остаток на паузе
let nameDraft = null;    // несохранённый ввод имени
let codeDraft = '';      // введённый код комнаты
let leaveArmed = false;  // «Сдаться» нажато один раз
let userUid = lget('uid') || '';   // профиль этого браузера (если сервер ведёт профили)
let loginEs = null;      // EventSource, для которого вход в профиль уже выполнен
let myProfile = null;    // свой профиль для строки в меню
let page = null;         // { kind: 'profile' | 'top', uid, from, data, error } — открыта страница профиля или рейтинга
let setup = null;        // настройки новой игры (kind: 'room' | 'bot'); null — обычное меню
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
  const prev = state;
  state = st;
  if (st.name) { name = st.name; lset('name', name); }
  const g = st.game;
  timer = g && (g.phase === 'pick' || g.phase === 'question')
    ? { deadline: performance.now() + g.timeLeftMs, total: g.timeTotalMs, frozen: g.paused ? g.timeLeftMs : null }
    : null;
  if (!g || g.phase === 'final') leaveArmed = false;
  if (st.screen !== 'menu') { setup = null; page = null; }
  if (st.screen === 'menu' && prev && prev.screen !== 'menu' && prev.screen !== 'replaced') setTimeout(loadMyProfile, 400); // итоги матча уже записаны
  ensureLogin();
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

// Из чего создатель игры выбирает правила.
/* ---------- профиль и рейтинг ---------- */
const catInfo = (id) => (info && info.categories.find((c) => c.id === id)) || { name: id, icon: '❔' };
const pct = (a, b) => (b ? Math.round((100 * a) / b) : 0);

async function getJSON(path) {
  try {
    const r = await fetch('/api/' + path);
    const j = await r.json().catch(() => ({}));
    return r.ok ? { ok: true, data: j } : { ok: false, error: j.error || 'Не удалось загрузить' };
  } catch {
    return { ok: false, error: 'Нет связи с сервером' };
  }
}

// Входит в профиль, хранящийся в этом браузере, или заводит новый. Ответ 401
// значит, что профиль потерян (например, базу пересоздали): тогда заводим новый.
async function login(creds = { uid: lget('uid') || '', secret: lget('secret') || '' }, retry = true) {
  try {
    const r = await fetch('/api/login', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ pid, name, ...creds }),
    });
    const j = await r.json().catch(() => ({}));
    if (r.ok) {
      lset('uid', j.uid);
      if (j.secret) lset('secret', j.secret);
      userUid = j.uid;
      loadMyProfile();
      return 'ok';
    }
    if (r.status === 401 && retry) { lset('uid', ''); lset('secret', ''); return login({ uid: '', secret: '' }, false); }
    return r.status === 401 ? 'bad' : 'fail';
  } catch {
    return 'fail';
  }
}

function ensureLogin() {
  if (!info || !info.profiles || !es || loginEs === es || !state) return;
  loginEs = es;
  login();
}

async function loadMyProfile() {
  if (!userUid) return;
  const r = await getJSON('profile?uid=' + encodeURIComponent(userUid));
  if (r.ok) myProfile = r.data;
  if (state && state.screen === 'menu' && !page && !setup) render();
}

async function openPage(kind, uid = '', from = '') {
  page = { kind, uid, from, data: null, error: null };
  render();
  const r = await getJSON(kind === 'top' ? 'top?limit=30' : 'profile?uid=' + encodeURIComponent(uid));
  if (!page || page.kind !== kind || page.uid !== uid) return;
  if (r.ok) page.data = r.data; else page.error = r.error;
  render();
}

function closePage() {
  const from = page && page.from;
  page = null;
  if (from === 'top') openPage('top'); else render();
}

// Код для входа в профиль на другом устройстве: «профиль:секрет».
async function useCode(text) {
  const [uid, secret] = text.trim().split(':');
  if (!uid || !secret) { toast('Код должен выглядеть как профиль:секрет'); return; }
  const res = await login({ uid, secret }, false);
  if (res === 'ok') { myProfile = null; page = null; toast('Профиль подключён'); render(); }
  else toast(res === 'bad' ? 'Код не подошёл' : 'Не удалось войти');
}

function ratingLine(p) {
  const games = `${p.games} ${plural(p.games, 'игра', 'игры', 'игр')}`;
  return p.ratedGames ? `Рейтинг ${p.rating} · ${p.rank} место · ${games}` : `Рейтинг ${p.rating} · ${games}`;
}

function profileBar() {
  return h('div', { class: 'card profilebar' },
    avatar({ name, bot: false }),
    h('div', { class: 'pbinfo' },
      h('div', { class: 'pbname' }, name),
      h('div', { class: 'muted small' }, myProfile ? ratingLine(myProfile) : 'Профиль загружается…')),
    h('div', { class: 'pbbtns' },
      h('button', { class: 'btn small', onclick: () => openPage('profile', userUid) }, 'Профиль'),
      h('button', { class: 'btn small', onclick: () => openPage('top') }, '🏆 Рейтинг')));
}

function pageView() {
  let body;
  if (page.error) body = h('p', { class: 'muted warn' }, page.error);
  else if (!page.data) body = h('div', { class: 'spinner' });
  else body = page.kind === 'top' ? topView(page.data) : profileView(page.data);
  return [
    body,
    h('button', { class: 'btn ghost small', onclick: closePage }, 'Назад'),
  ].flat();
}

function topView(list) {
  if (!list.length) {
    return [h('h2', null, '🏆 Рейтинг'), h('div', { class: 'card muted' },
      'Пока никто не сыграл рейтинговую партию. Рейтинг меняется в играх, где участвуют минимум двое людей с профилями (боты не в счёт).')];
  }
  return [
    h('h2', null, '🏆 Рейтинг'),
    h('div', { class: 'card standings' }, list.map((t, i) =>
      h('button', {
        class: 'srow linkrow' + (t.id === userUid ? ' me' : ''), onclick: () => openPage('profile', t.id, 'top'),
      },
        h('span', { class: 'place' }, i + 1),
        avatar({ name: t.name, bot: false }),
        h('span', { class: 'lname' }, t.name),
        h('span', { class: 'muted small' }, `${t.wins} из ${t.games}`),
        h('span', { class: 'pscore' }, t.rating)))),
  ];
}

const RESULT_ICON = { win: '🏆', draw: '🤝', lose: '😔' };

function profileView(p) {
  const own = p.id === userUid;
  const tile = (value, label) => h('div', { class: 'tile' }, h('div', { class: 'tv' }, value), h('div', { class: 'tl' }, label));
  const withEnough = p.cats.filter((c) => c.asked >= 5);
  const best = withEnough.length ? withEnough.reduce((a, b) => (pct(b.correct, b.asked) > pct(a.correct, a.asked) ? b : a)) : null;
  const fav = p.cats[0];
  const catName = (c) => `${catInfo(c.cat).icon} ${catInfo(c.cat).name}`;

  return [
    h('div', { class: 'hero' },
      h('div', { class: 'bigavatar' }, avatar({ name: p.name, bot: false })),
      h('h1', null, p.name),
      h('p', null, p.ratedGames ? `Рейтинг ${p.rating} · ${p.rank} место` : `Рейтинг ${p.rating} — рейтинговых игр ещё не было`),
      h('p', { class: 'small' }, `С ${new Date(p.since * 1000).toLocaleDateString('ru-RU')}`)),
    p.games + p.answered === 0 ? h('div', { class: 'card muted' }, 'Здесь появится статистика после первой сыгранной игры.') : null,
    h('div', { class: 'statgrid' },
      tile(p.games, 'игр'),
      tile(`${p.wins}/${p.draws}/${p.losses}`, 'побед/ничьих/поражений'),
      tile(`${pct(p.wins, p.games)}%`, 'доля побед'),
      tile(`${p.correct} из ${p.answered}`, `верных ответов (${pct(p.correct, p.answered)}%)`),
      tile(p.bestStreak, 'верных подряд — рекорд'),
      tile(p.bestScore, 'очков — рекорд за матч'),
      tile(p.fastestMs ? (p.fastestMs / 1000).toFixed(1) + ' с' : '—', 'самый быстрый верный ответ'),
      tile(p.points, 'очков всего'),
      p.tiebreaksWon ? tile(p.tiebreaksWon, 'решающих вопросов выиграно') : null),
    p.cats.length ? h('div', { class: 'card stack' },
      h('div', { class: 'label' }, 'Темы'),
      fav ? h('div', { class: 'small' }, `Чаще всего: ${catName(fav)}`) : null,
      best ? h('div', { class: 'small' }, `Лучше всего: ${catName(best)} — ${pct(best.correct, best.asked)}% верных`) : null,
      p.cats.map((c) => h('div', { class: 'catrow' },
        h('div', { class: 'cattop' }, h('span', null, catName(c)), h('span', { class: 'muted small' }, `${c.correct} из ${c.asked} · ${pct(c.correct, c.asked)}%`)),
        h('div', { class: 'bar' }, h('i', { style: { width: pct(c.correct, c.asked) + '%' } }))))) : null,
    p.recent.length ? h('div', { class: 'card standings' },
      h('div', { class: 'label' }, 'Последние игры'),
      p.recent.map((m) => h('div', { class: 'srow' },
        h('span', { class: 'place' }, RESULT_ICON[m.result] || ''),
        h('span', { class: 'lname' }, m.place ? `${m.place} место из ${m.players}` : `вышел (игроков: ${m.players})`),
        m.rated ? h('span', { class: 'delta ' + (m.delta >= 0 ? 'up' : 'down') }, (m.delta > 0 ? '+' : '') + m.delta) : h('span', { class: 'muted small' }, 'без рейтинга'),
        h('span', { class: 'pscore' }, m.score)))) : null,
    own ? ownProfileTools() : null,
  ];
}

// Профиль живёт в браузере; код позволяет открыть его на другом устройстве.
function ownProfileTools() {
  const code = `${lget('uid') || ''}:${lget('secret') || ''}`;
  let typed = '';
  return h('div', { class: 'card' }, h('details', null,
    h('summary', null, 'Профиль на другом устройстве'),
    h('p', { class: 'muted small' }, 'Откройте игру на другом устройстве и введите этот код в этом же разделе. Не показывайте его другим: по нему можно играть от вашего имени.'),
    h('div', { class: 'linkbox' }, code),
    h('button', { class: 'btn small', onclick: () => copyText(code) }, 'Скопировать код'),
    h('label', { class: 'label' }, 'Войти по коду'),
    h('div', { class: 'row' },
      h('input', { class: 'input', autocomplete: 'off', placeholder: 'профиль:секрет', oninput: (e) => { typed = e.target.value; } }),
      h('button', { class: 'btn small', onclick: () => useCode(typed) }, 'Войти'))));
}

const TIME_CHOICES = [10, 15, 20, 30, 45, 60];  // секунд на ответ
const ROUND_CHOICES = [3, 4, 6, 8, 10];
const QUESTION_CHOICES = [1, 2, 3, 4, 5];
const MIN_CATS = 2;                              // меньше двух тем колесо не нужно
const BOT_CHOICES = { room: [0, 1, 2, 3, 4, 5, 6, 7], bot: [1, 2, 3, 4, 5, 6, 7] };
const LEVELS = [['easy', 'Новичок'], ['normal', 'Обычный'], ['hard', 'Эксперт']];

const withDefault = (list, v) => [...new Set([...list, v])].sort((a, b) => a - b);

async function openSetup(kind) {
  await saveName();
  setup = {
    kind,
    secs: info ? Math.round(info.questionMs / 1000) : 30,
    rounds: info ? info.rounds : 6,
    qpr: info ? info.qPerRound : 3,
    speed: false,
    tiebreak: false,
    level: 'normal',
    hints: false,
    bots: kind === 'bot' ? 1 : 0,
    cats: new Set(info ? info.categories.map((c) => c.id) : []),
  };
  render();
}

async function createGame() {
  const s = setup;
  const all = info && s.cats.size === info.categories.length;
  await api(s.kind, {
    seconds: s.secs, rounds: s.rounds, qPerRound: s.qpr, speed: s.speed, tiebreak: s.tiebreak, hints: s.hints, bots: s.bots,
    cats: all ? [] : [...s.cats],
    level: s.bots > 0 ? s.level : '',
  });
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

// Явное «включить/выключить», а не «переключить»: двойное нажатие ничего не ломает.
function setPause(on) { api('pause', { on }); }

function answer(index) {
  const g = state && state.game;
  if (!g || g.phase !== 'question' || g.watching) return;
  const me = g.players[g.me];
  if (me.answer >= 0 || (g.hints && (g.hints.hidden || []).includes(index))) return;
  me.answer = index; // сразу подсвечиваем, не дожидаясь сервера
  render();
  api('answer', { seq: g.question.seq, index });
}

const useHint = (kind) => api('hint', { kind });

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

// Игроки, кроме меня, в порядке мест за столом.
const others = (g) => g.players.filter((_, i) => i !== g.me);

function header(g) {
  const reveal = g.phase === 'reveal';
  const tie = g.tiebreak && g.phase !== 'final';
  const tags = g.double || g.rules.speed ? h('div', { class: 'tags' },
    g.double ? h('div', { class: 'x2tag' }, '×2 очки') : null,
    g.rules.speed ? h('div', { class: 'speedtag' }, '⚡ время важно') : null) : null;
  const round = h('div', { class: 'rnd' }, tie ? 'Решающий вопрос' : `Раунд ${g.round}/${g.rounds}`);

  if (g.players.length === 2) {
    const me = g.players[g.me], opp = g.players[1 - g.me];
    return h('div', { class: 'hdr' },
      playerBox(me, 'me', reveal ? me.points : 0),
      h('div', { class: 'mid' }, round, tags),
      playerBox(opp, 'opp', reveal ? opp.points : 0));
  }
  // за столом больше двоих: полоса с игроками, у выбирающего подсвечена рамка
  return h('div', { class: 'stack' },
    h('div', { class: 'mid wide' }, round, tags),
    h('div', { class: 'strip' }, g.players.map((p, i) => h('div', {
      class: 'pchip' + (i === g.me ? ' me' : '') + (g.phase === 'pick' && i === g.chooser ? ' picker' : '') + (p.connected && !p.left ? '' : ' off'),
    },
      avatar(p),
      h('div', { class: 'pname' }, stripBot(p.name)),
      h('div', { class: 'pscore' }, p.score),
      reveal && p.points ? h('div', { class: 'gain' }, '+' + p.points) : null,
      !reveal && g.phase === 'question' && p.answered ? h('div', { class: 'done' }, '✓') : null))));
}

const dots = (arr) => h('div', { class: 'dots' }, arr.map((v) => h('i', { class: 'dot d' + v })));

function board(g) {
  const duel = g.players.length === 2;
  return h('div', { class: 'card board' + (duel ? '' : ' solo') }, g.board.map((r, i) =>
    h('div', { class: 'brow' + (g.phase !== 'final' && i === g.round - 1 ? ' cur' : '') },
      dots(r.res[g.me]),
      h('div', { class: 'bcat' },
        r.cat
          ? [h('span', { class: 'bicon' }, r.cat.icon), h('span', { class: 'bname' }, r.cat.name)]
          : h('span', { class: 'bwait' }, `Раунд ${i + 1}`),
        i === g.rounds - 1 ? h('span', { class: 'x2' }, '×2') : null),
      duel ? dots(r.res[1 - g.me]) : h('div'))));
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
  if (setup) return setupView();
  if (page) return pageView();
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

  if (info && info.profiles && userUid) v.push(profileBar());

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
    bigButton('👥', 'Играть с друзьями', 'Комната до 8 игроков, можно добавить ботов', '', () => openSetup('room')),
    bigButton('🤖', 'Против ботов', 'Выберите сложность и правила', '', () => openSetup('bot'))));

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
        h('li', null, 'Тему раунда выбирает то один игрок, то другой — на колесе из пяти случайных тем. Пока тема выбирается, любой из игроков может поставить игру на паузу.'),
        h('li', null, `На ответ — ${sec} ${plural(sec, 'секунда', 'секунды', 'секунд')}. Верный ответ даёт ${info.points} ${plural(info.points, 'очко', 'очка', 'очков')}, скорость ответа не важна.`),
        h('li', null, 'В последнем раунде очки удваиваются.'),
        h('li', null, `При создании комнаты или игры с ботом можно выбрать время на ответ, число раундов и вопросов, темы и сложность бота, а также включить учёт скорости (верный ответ тогда стоит от ${info.minPoints} до ${info.points} очков) и решающий вопрос при ничьей.`),
        h('li', null, 'Побеждает тот, у кого больше очков. В комнате можно играть до восьми человек, в том числе с ботами: создатель нажимает «Начать», а темы выбирают по очереди.')))));
    v.push(h('div', { class: 'card' },
      h('div', { class: 'label' }, `${info.categories.length} тем, ${info.questions} вопросов`),
      h('div', { class: 'chips' }, info.categories.map((c) => h('span', { class: 'chip' }, `${c.icon} ${c.name}`)))));
  }
  return v;
}

const rulesLines = (r) => [
  `${r.rounds} ${plural(r.rounds, 'раунд', 'раунда', 'раундов')} по ${r.qPerRound} ${plural(r.qPerRound, 'вопросу', 'вопроса', 'вопросов')} · ` +
    `${r.seconds} ${plural(r.seconds, 'секунда', 'секунды', 'секунд')} на ответ`,
  (r.speed ? 'скорость учитывается' : 'скорость не учитывается') + (r.hints ? ' · с подсказками' : '') + ' · ' + tiebreakLine(r),
];

const tiebreakLine = (r) => !r.tiebreak ? 'при равном счёте — ничья'
  : r.speed ? 'при равном счёте — решающий вопрос, побеждает первый верный ответ'
    : 'при равном счёте — решающие вопросы, пока верно не ответит ровно один';

function choice(title, sub, on, onclick) {
  return h('button', { class: 'choice' + (on ? ' on' : ''), 'aria-pressed': String(on), onclick },
    title, h('span', { class: 'sub' }, sub));
}

function chipRow(values, current, onPick, label = (v) => v) {
  return h('div', { class: 'chips' }, values.map((v) =>
    h('button', { class: 'chip pick' + (v === current ? ' on' : ''), 'aria-pressed': String(v === current), onclick: () => onPick(v) }, label(v))));
}

function setupView() {
  const s = setup;
  const min = info ? info.minPoints : 50, max = info ? info.points : 100;
  const cats = info ? info.categories : [];
  const set = (patch) => { Object.assign(s, patch); render(); };
  const toggleCat = (id) => { s.cats.has(id) ? s.cats.delete(id) : s.cats.add(id); render(); };
  const bot = s.kind === 'bot';
  const maxSeats = info ? info.maxSeats : 8;
  return [
    h('div', { class: 'hero' },
      h('div', { class: 'logo' }, bot ? '🤖' : '👥'),
      h('h1', null, bot ? 'Игра с ботами' : 'Новая комната'),
      h('p', null, bot ? 'Выберите соперников и правила' : 'Правила увидят все игроки')),
    h('div', { class: 'card' },
      h('div', { class: 'label' }, bot ? 'Сколько ботов' : 'Боты в комнате (друзья заходят по ссылке, мест всего ' + maxSeats + ')'),
      chipRow(BOT_CHOICES[s.kind].filter((n) => n <= maxSeats - 1), s.bots, (v) => set({ bots: v }))),
    s.bots > 0 ? h('div', { class: 'card' },
      h('div', { class: 'label' }, 'Сложность ботов'),
      chipRow(LEVELS.map((l) => l[0]), s.level, (v) => set({ level: v }), (v) => LEVELS.find((l) => l[0] === v)[1])) : null,
    h('div', { class: 'card stack' },
      h('div', { class: 'label' }, 'Очки за верный ответ'),
      choice('Время не учитывается', `Верный ответ — всегда ${max}`, !s.speed, () => set({ speed: false })),
      choice('Время учитывается', `От ${min} до ${max}: чем быстрее ответ, тем больше очков`, s.speed, () => set({ speed: true }))),
    h('div', { class: 'card stack' },
      h('div', { class: 'label' }, 'Если счёт равный'),
      choice('Ничья', 'Матч заканчивается вничью', !s.tiebreak, () => set({ tiebreak: false })),
      choice('Решающий вопрос',
        s.speed ? 'Побеждает тот, кто первым ответит верно' : 'Вопросы идут, пока верно не ответит ровно один игрок',
        s.tiebreak, () => set({ tiebreak: true }))),
    h('div', { class: 'card stack' },
      h('div', { class: 'label' }, 'Подсказки (каждая — один раз за матч)'),
      choice('Без подсказок', 'Только знания', !s.hints, () => set({ hints: false })),
      choice('С подсказками', '50/50, право на ошибку и +10 секунд обоим', s.hints, () => set({ hints: true }))),
    h('div', { class: 'card' },
      h('div', { class: 'label' }, 'Время на ответ, секунд'),
      chipRow(withDefault(TIME_CHOICES, s.secs), s.secs, (v) => set({ secs: v }))),
    h('div', { class: 'card stack' },
      h('div', null,
        h('div', { class: 'label' }, 'Раундов'),
        chipRow(withDefault(ROUND_CHOICES, s.rounds), s.rounds, (v) => set({ rounds: v }))),
      h('div', null,
        h('div', { class: 'label' }, 'Вопросов в раунде'),
        chipRow(withDefault(QUESTION_CHOICES, s.qpr), s.qpr, (v) => set({ qpr: v })))),
    h('div', { class: 'card' },
      h('div', { class: 'label' }, `Темы: ${s.cats.size} из ${cats.length}`),
      h('div', { class: 'chips' }, cats.map((c) =>
        h('button', {
          class: 'chip pick' + (s.cats.has(c.id) ? ' on' : ''), 'aria-pressed': String(s.cats.has(c.id)),
          onclick: () => toggleCat(c.id),
        }, `${c.icon} ${c.name}`))),
      h('div', { class: 'row cats-actions' },
        h('button', { class: 'btn ghost small', onclick: () => set({ cats: new Set(cats.map((c) => c.id)) }) }, 'Все'),
        h('button', { class: 'btn ghost small', onclick: () => set({ cats: new Set() }) }, 'Сбросить')),
      s.cats.size < MIN_CATS ? h('p', { class: 'muted warn' }, `Выберите минимум ${MIN_CATS} темы`) : null),
    h('button', { class: 'btn gold block', disabled: s.cats.size < MIN_CATS, onclick: createGame }, bot ? 'Начать игру' : 'Создать комнату'),
    h('button', { class: 'btn ghost small', onclick: () => { setup = null; render(); } }, 'Назад'),
  ].filter(Boolean);
}

function queueView() {
  return [h('div', { class: 'wait' },
    h('div', { class: 'spinner' }),
    h('h2', null, 'Ищем соперника…'),
    h('p', { class: 'muted' }, 'Если никого нет рядом, можно сыграть с ботом.'),
    h('button', { class: 'btn primary block', onclick: async () => { await api('cancel'); await api('bot'); } }, '🤖 Играть с ботом'),
    h('button', { class: 'btn ghost small', onclick: () => api('cancel') }, 'Отмена'))];
}

function roomView(code, rules, cats, lobby) {
  const link = `${shareBase()}/?room=${code}`;
  const host = lobby && lobby.host;
  return [h('div', { class: 'wait' },
    h('p', { class: 'muted' }, 'Код комнаты'),
    h('div', { class: 'bigcode' }, code),
    rules ? h('div', { class: 'rules' }, rulesLines(rules).map((l) => h('div', null, l))) : null,
    cats && cats.length ? h('div', { class: 'chips' }, cats.map((c) => h('span', { class: 'chip' }, `${c.icon} ${c.name}`))) : null,
    host ? h('p', { class: 'muted' }, 'Отправьте друзьям ссылку — достаточно открыть её в браузере (устройства должны быть в одной сети):') : null,
    host ? h('div', { class: 'linkbox' }, link) : null,
    host ? h('button', { class: 'btn gold block', onclick: () => copyText(link) }, '📋 Скопировать ссылку') : null,
    lobby ? h('div', { class: 'card lobby' },
      h('div', { class: 'label' }, `Игроки: ${lobby.players.length} из ${lobby.max}`),
      lobby.players.map((p) => h('div', { class: 'lrow' + (p.connected ? '' : ' off') },
        avatar(p),
        h('span', { class: 'lname' }, stripBot(p.name) + (p.me ? ' (вы)' : '')),
        p.owner ? h('span', { class: 'chip' }, '👑 создатель') : null,
        !p.connected ? h('span', { class: 'chip' }, 'нет связи') : null))) : null,
    host ? h('button', { class: 'btn primary block', disabled: !lobby.canStart, onclick: () => api('start') },
      lobby.canStart ? '▶ Начать игру' : 'Нужен ещё хотя бы один участник') : h('p', { class: 'muted' }, 'Ждём, пока создатель начнёт игру…'),
    h('button', { class: 'btn ghost small', onclick: () => api('cancel') }, host ? 'Закрыть комнату' : 'Выйти из комнаты'))];
}

function replacedView() {
  return [h('div', { class: 'wait' },
    h('h2', null, 'Игра открыта в другой вкладке'),
    h('p', { class: 'muted' }, 'Одновременно можно играть только в одной вкладке с этим игроком.'),
    h('button', { class: 'btn primary block', onclick: () => { state = null; render(); connect(); } }, 'Играть здесь'))];
}

function gameView(g) {
  const away = g.phase === 'final' ? [] : others(g).filter((p) => !p.connected && !p.bot && !p.left);
  const lost = away.length
    ? h('div', { class: 'status bad' }, `${away.map((p) => stripBot(p.name)).join(', ')}: потеряна связь…`) : null;

  switch (g.phase) {
    case 'pick': return [header(g), board(g), pickPanel(g), lost, leaveButton('Сдаться и выйти')];
    case 'intro': return [header(g), introPanel(g), leaveButton('Сдаться и выйти')];
    case 'question':
    case 'reveal': return [header(g), ...questionPanel(g), lost, leaveButton('Сдаться и выйти')];
    default: return finalView(g);
  }
}

function pickPanel(g) {
  const title = g.paused ? '⏸ Пауза'
    : g.iPick ? 'Выберите категорию' : `${stripBot(g.chooserName)} выбирает категорию…`;
  const who = g.paused
    ? h('div', { class: 'psub' }, g.iPaused ? 'Вы поставили игру на паузу' : `На паузу поставил игрок ${stripBot(g.pausedBy)}`)
    : null;
  return h('div', { class: 'stack' },
    h('div', { class: 'title' }, title, who),
    timerBar(),
    h('div', { class: 'pausebar' },
      h('button', { class: g.paused ? 'btn gold small' : 'btn ghost small', onclick: () => setPause(!g.paused) },
        g.paused ? '▶ Продолжить' : '⏸ Пауза')),
    wheel(g.choices, g.iPick && !g.paused ? pickCategory : null, g.paused));
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
// at the top. Without onPick the wheel is only shown (the opponent's view);
// while the game is paused it is shown greyed out.
function wheel(choices, onPick, paused = false) {
  const C = 200, R = 190, step = 360 / choices.length;
  const at = (deg, r) => [C + r * Math.cos(deg * Math.PI / 180), C + r * Math.sin(deg * Math.PI / 180)];
  const f = (n) => n.toFixed(1);

  const root = svg('svg', {
    class: (onPick ? 'wheel live' : 'wheel idle') + (paused ? ' paused' : ''), viewBox: '0 0 400 400',
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
    h('p', { class: 'muted' }, g.tiebreak ? 'Счёт равный — решающий вопрос' : `Раунд ${g.round} из ${g.rounds}`),
    g.double && !g.tiebreak ? h('div', { class: 'x2tag' }, 'Последний раунд — очки ×2') : null);
}

function questionPanel(g) {
  const q = g.question;
  const reveal = g.phase === 'reveal';
  const me = g.players[g.me];
  const hidden = (g.hints && g.hints.hidden) || [];
  const duel = g.players.length === 2;
  const opts = q.options.map((text, i) => {
    let cls = 'opt';
    if (reveal) {
      cls += ' locked ' + (i === g.correct ? 'correct' : i === me.answer ? 'wrong' : 'dim');
    } else if (hidden.includes(i)) {
      cls += ' locked dim crossed';
    } else if (me.answer >= 0 || g.watching) {
      cls += ' locked' + (i === me.answer ? ' picked' : '');
    }
    const marks = reveal ? others(g).filter((p) => p.answer === i) : [];
    return h('button', { class: cls, onclick: () => answer(i) },
      h('span', { class: 'letter' }, 'ABCD'[i]),
      h('span', { class: 'otext' }, text),
      marks.length ? h('span', { class: 'marks' }, marks.map((p) =>
        h('span', { class: 'oppmark' }, duel ? stripBot(p.name) : [...stripBot(p.name)].slice(0, 6).join('')))) : null);
  });

  const active = g.players.filter((p) => !p.left);
  const answered = active.filter((p) => p.answered).length;
  let status = h('div', { class: 'status' }, ' ');
  if (g.watching) {
    status = h('div', { class: 'status muted' }, 'Решающий вопрос играют лидеры — вы наблюдаете');
  } else if (reveal) {
    if (me.answer === g.correct) status = h('div', { class: 'status good' }, me.points ? `Верно! +${me.points}` : 'Верно!');
    else if (me.answer < 0) status = h('div', { class: 'status bad' }, 'Время вышло');
    else status = h('div', { class: 'status bad' }, 'Неверно');
  } else if (me.answer >= 0 && answered < active.length) {
    status = h('div', { class: 'status muted' }, duel ? 'Ответ принят. Ждём соперника…' : `Ответ принят. Ответили ${answered} из ${active.length}`);
  } else if (me.answer < 0 && answered > 0) {
    status = h('div', { class: 'status muted' }, duel ? 'Соперник уже ответил!' : `Уже ответили: ${answered} из ${active.length}`);
  }

  return [
    h('div', { class: 'qmeta' },
      h('span', null, `${g.cat.icon} ${g.cat.name} · ` + (g.tiebreak ? 'решающий вопрос' : `вопрос ${g.qIndex} из ${g.qPerRound}`)),
      reveal ? null : h('span', { id: 'timerText', class: 'ttext' })),
    reveal ? h('div', { class: 'timer' }) : timerBar(),
    g.notice && !reveal ? h('div', { class: 'status notice' }, '⏱ ' + g.notice) : null,
    h('div', { class: 'card qtext' }, q.text),
    h('div', { class: 'opts' }, opts),
    status,
    g.hints && !reveal ? hintBar(g.hints, me.answer >= 0) : null,
  ];
}

function hintBar(hn, answered) {
  const btn = (kind, label, left, on = false) => h('button', {
    class: 'btn small hint' + (on ? ' on' : ''), disabled: !left || answered || on, onclick: () => useHint(kind),
  }, label);
  return h('div', { class: 'hints' },
    btn('fifty', '50/50', hn.fifty),
    btn('second', hn.armed ? '✓ Право на ошибку' : 'Право на ошибку', hn.second, hn.armed),
    btn('extra', '+10 сек. всем', hn.extra));
}

function finalView(g) {
  const me = g.players[g.me];
  const duel = g.players.length === 2;
  const look = { win: ['🏆', 'Победа!'], lose: ['😔', duel ? 'Поражение' : `${me.place ? me.place + ' место' : 'Вы вышли из игры'}`], draw: ['🤝', duel ? 'Ничья' : 'Первое место делят несколько игроков'] }[g.result] || ['🏁', 'Игра окончена'];

  const rest = others(g).filter((p) => !p.left && !p.bot);
  let label = 'Реванш', cls = 'btn primary block', disabled = false;
  if (!g.canRematch) { label = duel ? 'Соперник вышел' : 'Некому играть дальше'; disabled = true; }
  else if (me.rematch) { label = 'Ждём остальных…'; disabled = true; }
  else if (rest.some((p) => p.rematch)) { label = 'Принять реванш'; cls = 'btn gold block'; }

  const table = duel ? null : h('div', { class: 'card standings' }, g.order.map((i) => {
    const p = g.players[i];
    return h('div', { class: 'srow' + (i === g.me ? ' me' : '') + (p.left ? ' off' : '') },
      h('span', { class: 'place' }, p.left ? '—' : p.place),
      avatar(p),
      h('span', { class: 'lname' }, stripBot(p.name) + (i === g.me ? ' (вы)' : '') + (p.left ? ' — вышел' : '')),
      !p.left && !p.bot && p.rematch ? h('span', { class: 'chip' }, 'ещё раз ✓') : null,
      h('span', { class: 'pscore' }, p.score));
  }));

  return [
    h('div', { class: 'result' },
      h('div', { class: 'emoji' }, look[0]),
      h('h2', null, look[1]),
      g.forfeit ? h('p', { class: 'muted' }, duel ? 'Соперник покинул игру — победа засчитана' : 'Остальные игроки вышли — победа засчитана') : null,
      g.tiebreak ? h('p', { class: 'muted' }, g.result === 'draw' ? 'Даже решающие вопросы не выявили победителя' : 'Первое место решилось на решающем вопросе') : null,
      duel ? h('div', { class: 'final-score' }, me.score, h('span', null, ':'), g.players[1 - g.me].score) : null,
      duel ? h('p', { class: 'muted' }, `${me.name} — ${stripBot(g.players[1 - g.me].name)}`) : null),
    table,
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
    case 'room': view = roomView(state.room, state.rules, state.roomCats, state.lobby); break;
    case 'game': view = gameView(state.game); break;
    default: view = replacedView(); break;
  }
  // анимацию «появления» играем только при смене экрана, а не при каждом снимке
  const g = state.game;
  const key = state.screen + (setup ? ':setup' : '') + (page ? ':' + page.kind : '') + (g ? `:${g.phase === 'reveal' ? 'question' : g.phase}:${g.round}:${g.qIndex}` : '');
  app.className = key !== lastKey ? 'enter' : '';
  lastKey = key;
  app.replaceChildren(...view.filter(Boolean));
}

function tick() {
  if (timer) {
    const fill = document.getElementById('timerFill');
    if (fill) {
      const left = timer.frozen ?? Math.max(0, timer.deadline - performance.now());
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
  ensureLogin();
  if (state && state.screen === 'menu') render();
}).catch(() => {});

render();
connect();
requestAnimationFrame(tick);
