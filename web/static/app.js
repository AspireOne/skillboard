import { createEditor, replaceContent } from './editor.js';

const POLL_MS = 1500;
const SAVE_DEBOUNCE_MS = 600;
const SEARCH_DEBOUNCE_MS = 150;
const TOAST_MS = 6000;
const PALETTE_FILE_LIMIT = 8;
const DESCRIPTION_LIMIT = 1024; // mirrors lint.maxDescriptionChars
// Measured on the real skill descriptions (2026-10-29). GPT 6.1 Sol, GLM 5.3 Flash and
// DeepSeek V4.1 agree within 3%; Claude's tokenizer needs ~70% more tokens for the same text.
const CHARS_PER_TOKEN = 5.3;
const CHARS_PER_CLAUDE_TOKEN = 3.1;
// Estimated cost of one entry in a harness's skill list beyond the name and description:
// bullet, separators and, in Codex, the SKILL.md path.
const LIST_ENTRY_TOKENS = 20;
const SKILL_FILE = 'SKILL.md';
const GLOBAL = 'global'; // workspace kind; every other scope is a project directory
const MARKDOWN = /\.(md|markdown)$/i;
const LOCAL_HINT = 'Ignored by your dotfiles, so it stays on this machine';
// In stacking order; the colour of each lives in style.css as --harness-<id>.
const HARNESSES = [{ id: 'codex', label: 'Codex' }, { id: 'claude', label: 'Claude Code' }, { id: 'pi', label: 'Pi' }];
const USAGE_PERIODS = [{ days: 30, label: '30 days' }, { days: 90, label: '90 days' }, { days: null, label: 'All time' }];
// The first is the default. Each palette lives in style.css under [data-theme="<id>"];
// theme.js applies the saved one before first paint.
const THEMES = [{ id: 'violet', label: 'Violet' }, { id: 'sand', label: 'Sand' }, { id: 'linen', label: 'Linen' }];
const THEME_COOKIE_MAX_AGE = 10 * 365 * 24 * 60 * 60;

// Lucide icons (ISC licence), inlined so the app works offline.
const ICONS = {
  search: '<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>',
  overview: '<rect width="7" height="9" x="3" y="3" rx="1"/><rect width="7" height="5" x="14" y="3" rx="1"/><rect width="7" height="9" x="14" y="12" rx="1"/><rect width="7" height="5" x="3" y="16" rx="1"/>',
  file: '<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/><path d="M16 13H8"/><path d="M16 17H8"/><path d="M10 9H8"/>',
  plus: '<path d="M5 12h14"/><path d="M12 5v14"/>',
  archive: '<rect width="20" height="5" x="2" y="3" rx="1"/><path d="M4 8v11a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8"/><path d="M10 12h4"/>',
  trash: '<path d="M3 6h18"/><path d="M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6"/><path d="M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2"/><path d="M10 11v6"/><path d="M14 11v6"/>',
  restore: '<rect width="20" height="5" x="2" y="3" rx="1"/><path d="M4 8v11a2 2 0 0 0 2 2h2"/><path d="M20 8v11a2 2 0 0 1-2 2h-2"/><path d="m9 15 3-3 3 3"/><path d="M12 12v9"/>',
  error: '<circle cx="12" cy="12" r="10"/><path d="M12 8v4"/><path d="M12 16h.01"/>',
  warning: '<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/><path d="M12 9v4"/><path d="M12 17h.01"/>',
  info: '<circle cx="12" cy="12" r="10"/><path d="M12 16v-4"/><path d="M12 8h.01"/>',
  check: '<path d="M20 6 9 17l-5-5"/>',
  chevron: '<path d="m9 18 6-6-6-6"/>',
  link: '<path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/>',
  text: '<path d="M17 6.1H3"/><path d="M21 12.1H3"/><path d="M15.1 18H3"/>',
  offline: '<path d="M12 20h.01"/><path d="M8.5 16.43a5 5 0 0 1 7 0"/><path d="M2 8.82a15 15 0 0 1 4.17-2.65"/><path d="M10.66 5c4.01-.36 8.14.9 11.34 3.76"/><path d="M16.85 11.25a10 10 0 0 1 2.22 1.68"/><path d="M5 13a10 10 0 0 1 5.24-2.76"/><path d="m2 2 20 20"/>',
};

const state = {
  scopes: null, // one tree per workspace, the project first; paths start with the workspace ID
  treeJSON: '',
  route: { kind: 'overview' }, // or { kind: 'file', path }
  // { path, saved, draft, mtime, binary, conflict: null | 'changed' | 'deleted', saveError }
  open: null,
  editor: null,
  frontmatterNote: null, // description stats inside the SKILL.md editor
  harnesses: null,
  usage: null, // one entry per session that used a skill; scanned once per launch
  usageError: null,
  usagePeriod: USAGE_PERIODS[0],
  skillsClosed: new Set(), // workspace IDs; skills start expanded
  purgatoryOpen: new Set(), // workspace IDs; purgatory starts collapsed
  offline: false,
};

// ---------- helpers ----------

const nodes = (...children) => children.flat(Infinity).filter((c) => c != null && c !== false);

function h(tag, props = {}, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (value == null || value === false) continue;
    if (key.startsWith('on')) el.addEventListener(key.slice(2), value);
    else if (key === 'class') el.className = value;
    else if (key === 'style') el.style.cssText = value;
    else el.setAttribute(key, value === true ? '' : value);
  }
  el.append(...nodes(children));
  return el;
}

function icon(name) {
  const span = h('span', { class: 'icon' });
  span.innerHTML = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round">${ICONS[name]}</svg>`;
  return span;
}

async function api(method, url, body, init = {}) {
  const res = await fetch(url, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : {},
    body: body ? JSON.stringify(body) : undefined,
    ...init,
  });
  if (!res.ok) {
    const err = new Error((await res.text()).trim() || res.statusText);
    err.status = res.status;
    throw err;
  }
  return res.json();
}

const fileURL = (path) => `/api/file?path=${encodeURIComponent(path)}`;
const plural = (n, word) => `${n} ${word}${n === 1 ? '' : 's'}`;
const fmt = (n) => n.toLocaleString('en-US');
const tokens = (chars) => Math.round(chars / CHARS_PER_TOKEN);
const claudeTokens = (chars) => Math.round(chars / CHARS_PER_CLAUDE_TOKEN);
// What an automatic skill costs every session: its entry in the skill list.
const listedChars = (skill) => skill.name.length + skill.descriptionChars;
const listTokens = (skill) => tokens(listedChars(skill)) + LIST_ENTRY_TOKENS;
const listClaudeTokens = (skill) => claudeTokens(listedChars(skill)) + LIST_ENTRY_TOKENS;
const sum = (items, fn) => items.reduce((n, item) => n + fn(item), 0);
// Local calendar dates, as the server writes them.
const isoDay = (date) => date.toLocaleDateString('sv-SE');
function daysAgo(day) {
  const n = Math.round((new Date(isoDay(new Date())) - new Date(day)) / 86400000);
  return n === 0 ? 'today' : n === 1 ? 'yesterday' : `${n} days ago`;
}

// Usage per skill name within the chosen period. Last use is over all time.
function usageByName() {
  const days = state.usagePeriod.days;
  const since = days && isoDay(new Date(Date.now() - (days - 1) * 86400000));
  const byName = new Map();
  for (const u of state.usage) {
    let entry = byName.get(u.skill);
    if (!entry) byName.set(u.skill, entry = { total: 0, manual: 0, last: '', ...Object.fromEntries(HARNESSES.map((hn) => [hn.id, 0])) });
    if (u.day > entry.last) entry.last = u.day;
    if (since && u.day < since) continue;
    entry.total++;
    entry[u.harness]++;
    if (u.manual) entry.manual++;
  }
  return byName;
}
const periodText = () => (state.usagePeriod.days ? `in ${state.usagePeriod.label}` : 'all time');
const usageDetail = (u) => [
  ...HARNESSES.filter((hn) => u[hn.id]).map((hn) => `${hn.label} ${u[hn.id]}`),
  u.manual > 0 && `${u.manual} invoked by hand`,
].filter(Boolean).join(' · ');

const countBy = (issues, severity) => issues.filter((i) => i.severity === severity).length;

const allSkills = () => state.scopes.flatMap((scope) => scope.skills);
const allFiles = () => state.scopes.flatMap((scope) => [...scope.rootFiles, ...scope.skills.flatMap((s) => s.files)]);
const fileInTree = (path) => allFiles().find((f) => f.path === path);
const skillOf = (path) => allSkills().find((s) => path.startsWith(s.dir + '/'));
const scopeOf = (path) => state.scopes.find((scope) => path.startsWith(scope.id + '/'));
const inScope = (path) => path.slice(path.indexOf('/') + 1); // drops the workspace ID
const hasProject = () => state.scopes.some((scope) => scope.kind !== GLOBAL);
const skillEntry = (skill) => skill.files.find((f) => f.path === `${skill.dir}/${SKILL_FILE}`)?.path ?? skill.files[0]?.path;
const isDirty = () => !!state.open && !state.open.binary && state.open.draft !== state.open.saved;

function toast(message) {
  const el = h('div', { class: 'toast' }, icon('error'), h('div', {}, message));
  document.getElementById('toasts').append(el);
  setTimeout(() => el.remove(), TOAST_MS);
}

function fail(action, err) {
  console.error(action, err);
  toast(`${action} failed: ${err.message}`);
}

// ---------- saving ----------

let saveTimer = null;
let saving = null; // the in-flight save, so saves never overlap
let saveSeq = 0; // bumps on every completed save; stale polls must not undo it

function scheduleSave() {
  clearTimeout(saveTimer);
  saveTimer = setTimeout(() => flushSave(), SAVE_DEBOUNCE_MS);
}

async function flushSave({ overwrite = false } = {}) {
  clearTimeout(saveTimer);
  while (saving) await saving;
  const open = state.open;
  if (!open || open.binary || (open.conflict && !overwrite) || (!isDirty() && !overwrite)) return;

  const content = open.draft;
  const baseMtime = overwrite ? fileInTree(open.path)?.mtime ?? null : open.mtime;
  saving = (async () => {
    try {
      const file = await api('PUT', fileURL(open.path), { content, baseMtime });
      Object.assign(open, { saved: content, mtime: file.mtime, conflict: null, saveError: null });
      saveSeq++;
    } catch (err) {
      if (err.status === 409) open.conflict = 'changed';
      else {
        open.saveError = err.message;
        fail(`Saving ${open.path}`, err);
      }
    }
  })();
  await saving;
  saving = null;
  if (open === state.open) {
    renderChrome();
    if (isDirty() && !open.conflict && !open.saveError) scheduleSave(); // typed while saving
  }
}

// Last-chance save when the window closes; keepalive lets it outlive the page.
window.addEventListener('pagehide', () => {
  if (!isDirty() || state.open.conflict) return;
  fetch(fileURL(state.open.path), {
    method: 'PUT',
    keepalive: true,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ content: state.open.draft, baseMtime: state.open.mtime }),
  });
});

// ---------- sync with disk ----------

async function refreshTree() {
  const seq = saveSeq;
  let scopes;
  try {
    scopes = await api('GET', '/api/tree');
  } catch (err) {
    if (!state.offline) console.error('refreshing failed', err);
    setOffline(true);
    return;
  }
  setOffline(false);
  const json = JSON.stringify(scopes);
  if (json === state.treeJSON) return;
  const first = !state.scopes;
  state.scopes = scopes;
  state.treeJSON = json;
  if (first) return;
  if (seq === saveSeq) await reconcileOpenFile();
  renderSidebar();
  if (state.route.kind === 'overview') renderMain();
  else renderChrome();
}

// Picks up edits made on disk (often by an agent) to the open file.
async function reconcileOpenFile() {
  const open = state.open;
  if (!open || open.binary || saving) return;
  const onDisk = fileInTree(open.path);
  if (!onDisk) {
    open.conflict = 'deleted';
  } else if (onDisk.mtime === open.mtime) {
    if (open.conflict === 'deleted') open.conflict = null;
  } else if (!isDirty()) {
    const { content, file } = await api('GET', fileURL(open.path));
    if (open !== state.open || isDirty()) return;
    Object.assign(open, { saved: content, draft: content, mtime: file.mtime, conflict: null });
    replaceContent(state.editor, content);
  } else {
    open.conflict = 'changed';
  }
}

function setOffline(offline) {
  if (offline === state.offline) return;
  state.offline = offline;
  if (state.scopes) renderChrome();
}

// ---------- navigation ----------

async function navigate(route, { replace = false } = {}) {
  await flushSave();
  if (route.kind === 'file') {
    try {
      const { content, file } = await api('GET', fileURL(route.path));
      state.open = { path: route.path, saved: content, draft: content, mtime: file.mtime, binary: false, conflict: null, saveError: null };
    } catch (err) {
      if (err.status !== 415) return fail(`Opening ${route.path}`, err);
      state.open = { path: route.path, binary: true };
    }
  } else {
    state.open = null;
  }
  state.route = route;
  revealOpenSkill();
  const hash = route.kind === 'file' ? '#' + encodeURIComponent(route.path) : '#';
  if (replace) history.replaceState(null, '', hash);
  else history.pushState(null, '', hash);
  renderSidebar();
  renderMain();
}

// Expands the sidebar section holding the open skill, once; it can be collapsed again.
function revealOpenSkill() {
  const skill = state.open && skillOf(state.open.path);
  if (!skill) return;
  const id = scopeOf(skill.dir).id;
  if (skill.enabled) state.skillsClosed.delete(id);
  else state.purgatoryOpen.add(id);
}

const openFile = (path) => navigate({ kind: 'file', path });
const openOverview = () => navigate({ kind: 'overview' });

function routeFromHash() {
  const path = decodeURIComponent(location.hash.slice(1));
  return path && fileInTree(path) ? { kind: 'file', path } : { kind: 'overview' };
}

window.addEventListener('popstate', () => navigate(routeFromHash(), { replace: true }));

function openLink(href) {
  if (/^[a-z][a-z0-9+.-]*:/i.test(href)) {
    window.open(href, '_blank', 'noopener');
    return;
  }
  const dir = state.open.path.split('/').slice(0, -1).join('/');
  const path = decodeURIComponent(new URL(href, `http://x/${dir}/`).pathname.slice(1));
  if (fileInTree(path)) openFile(path);
  else toast(`${path} does not exist`);
}

// ---------- actions ----------

// The server re-lints before fixing, so an issue that changed meanwhile is refused, not misapplied.
async function applyFix(skill, issue, button) {
  button.disabled = true;
  await flushSave();
  try {
    await api('POST', '/api/skills/fix', { dir: skill.dir, message: issue.message });
  } catch (err) {
    button.disabled = false;
    return fail(`Fixing ${skill.name}`, err);
  }
  await refreshTree(); // also reloads the open file if the fix changed it
}

async function setEnabled(skill, enabled) {
  await flushSave();
  const scope = scopeOf(skill.dir);
  let dir;
  try {
    ({ dir } = await api('POST', `/api/skills/${scope.id}/${encodeURIComponent(skill.name)}/enabled`, { enabled }));
  } catch (err) {
    return fail(`Moving ${skill.name}`, err);
  }
  const open = state.open;
  if (open?.path.startsWith(skill.dir + '/')) {
    open.path = dir + open.path.slice(skill.dir.length);
    state.route = { kind: 'file', path: open.path };
    history.replaceState(null, '', '#' + encodeURIComponent(open.path));
  }
  if (!enabled) state.purgatoryOpen.add(scope.id);
  await refreshTree();
  renderSidebar();
  renderChrome();
}

function createSkill(scope) {
  promptDialog({
    title: scope.kind === GLOBAL ? 'New skill' : `New skill in ${scope.label}`,
    hint: 'Lowercase letters, digits and hyphens. It becomes the directory and the name in the frontmatter.',
    prefix: `${scope.skillsDir}/`,
    placeholder: 'my-skill',
    confirm: 'Create skill',
    async submit(name) {
      const file = await api('POST', '/api/skills', { scope: scope.id, name });
      await refreshTree();
      await openFile(file.path);
    },
  });
}

function createFile(skill) {
  promptDialog({
    title: `New file in ${skill.name}`,
    hint: 'Reference material goes in references/, scripts in scripts/, templates in assets/.',
    prefix: `${skill.name}/`,
    value: 'references/',
    confirm: 'Create file',
    async submit(rel) {
      if (!rel || rel.endsWith('/')) throw new Error('Enter a file name.');
      const path = `${skill.dir}/${rel}`;
      await api('PUT', fileURL(path), { content: '', baseMtime: null });
      await refreshTree();
      await openFile(path);
    },
  });
}

function deleteSkill(skill) {
  const scope = scopeOf(skill.dir);
  dialog({
    title: `Delete ${skill.name}?`,
    hint: `This permanently deletes ${scope.root}/${inScope(skill.dir)} and its ${plural(skill.files.length, 'file')}. To only hide the skill, move it to purgatory instead.`,
    confirm: 'Delete skill',
    danger: true,
    async submit() {
      await flushSave();
      await api('DELETE', `/api/skills?dir=${encodeURIComponent(skill.dir)}`);
      if (state.open?.path.startsWith(skill.dir + '/')) {
        state.open = null; // its file is gone; nothing left to save
        await navigate({ kind: 'overview' }, { replace: true });
      }
      await refreshTree();
    },
  });
}

async function resolveConflict(keepMine) {
  if (keepMine) return flushSave({ overwrite: true });
  state.open.draft = state.open.saved; // drop local edits, then load the disk version
  state.open.conflict = null;
  await navigate({ kind: 'file', path: state.open.path }, { replace: true });
}

// ---------- overlays ----------

function overlay(panel, onClose) {
  const el = h('div', { class: 'overlay', onmousedown: (e) => e.target === el && close() }, panel);
  const close = () => {
    el.remove();
    document.removeEventListener('keydown', onKey, true);
    onClose?.();
  };
  const onKey = (e) => {
    if (e.key === 'Escape') {
      e.preventDefault();
      close();
    }
  };
  document.addEventListener('keydown', onKey, true);
  document.body.append(el);
  return close;
}

// A dialog with optional content above its actions. A failed submit keeps it open and
// shows the error inline. Cancel has focus, so Enter never confirms by accident.
function dialog({ title, hint, content, confirm, danger = false, submit }) {
  const error = h('div', { class: 'error-text' });
  const cancel = h('button', { type: 'button', class: 'btn', onclick: () => close() }, 'Cancel');
  const form = h('form', { class: 'panel dialog' },
    h('h3', {}, title),
    hint && h('p', {}, hint),
    content,
    error,
    h('div', { class: 'dialog-actions' }, cancel,
      h('button', { type: 'submit', class: `btn ${danger ? 'danger' : 'primary'}` }, confirm)));
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    try {
      await submit();
      close();
    } catch (err) {
      error.textContent = err.message;
    }
  });
  const close = overlay(form);
  cancel.focus();
}

function promptDialog({ title, hint, prefix, placeholder, value = '', confirm, submit }) {
  const input = h('input', { placeholder, spellcheck: 'false', autocomplete: 'off' });
  input.value = value;
  dialog({
    title,
    hint,
    content: h('label', { class: 'field' }, prefix && h('span', { class: 'prefix' }, prefix), input),
    confirm,
    submit: () => submit(input.value.trim()),
  });
  input.focus();
  input.setSelectionRange(input.value.length, input.value.length);
}

function openPalette() {
  if (!state.scopes || document.querySelector('.overlay')) return;
  const input = h('input', { placeholder: 'Search files and contents…', spellcheck: 'false', autocomplete: 'off' });
  const results = h('div', { class: 'palette-results' });
  let items = [];
  let selected = 0;
  let timer = null;
  let contentHits = [];

  const highlight = (text, query) => {
    const i = query ? text.toLowerCase().indexOf(query.toLowerCase()) : -1;
    if (i < 0) return text;
    return [text.slice(0, i), h('mark', {}, text.slice(i, i + query.length)), text.slice(i + query.length)];
  };

  const draw = ({ scroll = true } = {}) => {
    const query = input.value.trim();
    const q = query.toLowerCase();
    const files = allFiles()
      .filter((f) => !q || f.path.toLowerCase().includes(q))
      .slice(0, query ? PALETTE_FILE_LIMIT : PALETTE_FILE_LIMIT * 2);
    items = [
      ...files.map((f) => ({ path: f.path, title: inScope(f.path).split('/').slice(-2).join('/'), detail: f.path, kind: 'file' })),
      ...contentHits.map((hit) => ({ path: hit.path, title: hit.text, detail: `${hit.path}:${hit.line}`, kind: 'text' })),
    ];
    selected = Math.min(selected, Math.max(0, items.length - 1));
    const row = (item, i) => h('button', {
      class: `palette-item${i === selected ? ' selected' : ''}`,
      onmousemove: () => { if (selected !== i) { selected = i; draw({ scroll: false }); } },
      onclick: () => choose(item),
    }, icon(item.kind), h('div', { class: 'main' },
      h('div', { class: 'line1' }, highlight(item.title, query)),
      h('div', { class: 'line2' }, item.detail)));
    results.replaceChildren(...nodes(
      items.length === 0 && h('div', { class: 'palette-empty' }, query ? 'No matches' : 'No files'),
      files.length > 0 && h('div', { class: 'palette-group' }, 'Files'),
      items.slice(0, files.length).map(row),
      contentHits.length > 0 && h('div', { class: 'palette-group' }, 'In contents'),
      items.slice(files.length).map((item, i) => row(item, i + files.length)),
    ));
    if (scroll) results.querySelector('.selected')?.scrollIntoView({ block: 'nearest' });
  };

  const choose = (item) => {
    close();
    openFile(item.path);
  };

  input.addEventListener('input', () => {
    selected = 0;
    draw();
    clearTimeout(timer);
    const query = input.value.trim();
    if (!query) {
      contentHits = [];
      return draw();
    }
    timer = setTimeout(async () => {
      try {
        const hits = await api('GET', `/api/search?q=${encodeURIComponent(query)}`);
        if (input.value.trim() === query) {
          contentHits = hits;
          draw();
        }
      } catch (err) {
        fail('Search', err);
      }
    }, SEARCH_DEBOUNCE_MS);
  });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      selected = (selected + (e.key === 'ArrowDown' ? 1 : -1) + items.length) % Math.max(1, items.length);
      draw();
    } else if (e.key === 'Enter' && items[selected]) {
      e.preventDefault();
      choose(items[selected]);
    }
  });

  const close = overlay(h('div', { class: 'panel' }, h('div', { class: 'palette-input' }, icon('search'), input), results));
  draw();
  input.focus();
}

function showTooltip(e, text) {
  const tip = document.getElementById('tooltip');
  tip.textContent = text;
  tip.hidden = false;
  tip.style.left = `${e.clientX + 12}px`;
  tip.style.top = `${e.clientY + 14}px`;
}
const hideTooltip = () => { document.getElementById('tooltip').hidden = true; };

// ---------- rendering: sidebar ----------

function renderSidebar() {
  const nav = document.getElementById('nav');
  const openPath = state.open?.path;
  const current = openPath && skillOf(openPath);

  const skillItem = (skill) => {
    const errors = countBy(skill.issues ?? [], 'error');
    const warnings = countBy(skill.issues ?? [], 'warning');
    return h('button', {
      class: `nav-item${skill.enabled ? '' : ' parked'}${current === skill ? ' active' : ''}`,
      onclick: () => skillEntry(skill) && openFile(skillEntry(skill)),
    },
    h('span', { class: 'label' }, skill.name),
    skill.manualOnly && h('span', { class: 'pill manual', title: 'disable-model-invocation: true' }, 'manual'),
    skill.local && h('span', { class: 'pill local', title: LOCAL_HINT }, 'local'),
    errors > 0 ? h('span', { class: 'dot error', title: plural(errors, 'error') })
      : warnings > 0 && h('span', { class: 'dot warning', title: plural(warnings, 'warning') }));
  };

  const global = state.scopes.find((scope) => scope.kind === GLOBAL);
  const projects = state.scopes.filter((scope) => scope.kind !== GLOBAL);

  const scopeFiles = (scope) => [
    hasProject() && h('div', { class: 'nav-scope', title: scope.root }, scope.label),
    scope.rootFiles.map((f) =>
      h('button', { class: `nav-item${f.path === openPath ? ' active' : ''}`, onclick: () => openFile(f.path) },
        icon('file'), h('span', { class: 'label' }, inScope(f.path)))),
  ];

  const scopeSkills = (scope, label) => {
    const enabled = scope.skills.filter((s) => s.enabled);
    const parked = scope.skills.filter((s) => !s.enabled);
    const showEnabled = !state.skillsClosed.has(scope.id);
    const showParked = state.purgatoryOpen.has(scope.id);
    const toggle = (set) => () => {
      if (set.has(scope.id)) set.delete(scope.id);
      else set.add(scope.id);
      renderSidebar();
    };
    return [
      h('div', { class: `nav-section${showEnabled ? ' expanded' : ''}` },
        h('button', { class: 'nav-toggle', onclick: toggle(state.skillsClosed) },
          h('span', { class: 'chevron' }, icon('chevron')), label, h('span', { class: 'count' }, String(enabled.length))),
        h('button', { class: 'nav-add', title: 'New skill', onclick: () => createSkill(scope) }, icon('plus'))),
      showEnabled && enabled.map(skillItem),
      parked.length > 0 && h('div', { class: `nav-section${showParked ? ' expanded' : ''}` },
        h('button', { class: 'nav-toggle', onclick: toggle(state.purgatoryOpen) },
          h('span', { class: 'chevron' }, icon('chevron')), 'Purgatory', h('span', { class: 'count' }, String(parked.length)))),
      showParked && parked.map(skillItem),
    ];
  };

  nav.replaceChildren(...nodes(
    h('button', { class: `nav-item${state.route.kind === 'overview' ? ' active' : ''}`, onclick: openOverview },
      icon('overview'), h('span', { class: 'label' }, 'Overview')),
    // Global instructions first, then each project, then the global skills: the
    // project's own files are what a launch from it is about, so they sit near the top.
    global && scopeFiles(global),
    projects.map((scope) => [scopeFiles(scope), scopeSkills(scope, 'Skills')]),
    global && [projects.length > 0 && h('div', { class: 'nav-rule' }), scopeSkills(global, projects.length > 0 ? 'Global skills' : 'Skills')],
  ));
}

// ---------- rendering: main ----------

function renderMain() {
  const main = document.getElementById('main');
  state.editor?.destroy();
  state.editor = null;
  state.frontmatterNote = null;

  if (state.route.kind === 'overview') {
    main.replaceChildren(
      h('header', { class: 'topbar', id: 'topbar' }),
      h('div', { class: 'scroll' }, renderOverview()),
    );
    renderChrome();
    return;
  }

  const open = state.open;
  const doc = h('div', { class: `doc${MARKDOWN.test(open.path) ? '' : ' plain'}` });
  main.replaceChildren(
    h('header', { class: 'topbar', id: 'topbar' }),
    h('div', { id: 'strip' }),
    h('div', { class: 'scroll' }, h('div', { class: 'page' }, h('div', { id: 'page-head' }), doc)),
  );

  if (open.binary) {
    doc.append(h('div', { class: 'empty-state' }, 'This is not a text file, so it can’t be edited here.'));
  } else {
    const skill = skillOf(open.path);
    // Filled by renderPageHead, so it follows the skill data without a new editor.
    if (skill && open.path === `${skill.dir}/${SKILL_FILE}`) state.frontmatterNote = h('span', { class: 'fm-note' });
    state.editor = createEditor(doc, {
      doc: open.saved,
      isMarkdown: MARKDOWN.test(open.path),
      frontmatterNote: state.frontmatterNote,
      onChange(text) {
        open.draft = text;
        open.saveError = null;
        renderStatus();
        if (!open.conflict) scheduleSave();
      },
      onOpenLink: openLink,
    });
  }
  renderChrome();
}

// Re-renders everything around the editor, leaving the editor itself untouched.
function renderChrome() {
  renderTopbar();
  if (state.route.kind !== 'file') return;
  renderStrip();
  renderPageHead();
}

function renderTopbar() {
  const bar = document.getElementById('topbar');
  if (!bar) return;
  const route = state.route;
  const skill = route.kind === 'file' && skillOf(route.path);
  const sep = () => h('span', { class: 'sep' }, icon('chevron'));

  let crumbs;
  if (route.kind === 'overview') crumbs = [h('span', { class: 'current' }, 'Overview')];
  else if (skill) {
    const rel = route.path.slice(skill.dir.length + 1);
    crumbs = [h('span', {}, skill.enabled ? 'Skills' : 'Purgatory'), sep(), h('span', { class: rel === SKILL_FILE ? 'current' : '' }, skill.name),
      rel !== SKILL_FILE && [sep(), h('span', { class: 'current' }, rel)]];
  } else crumbs = [h('span', { class: 'current' }, inScope(route.path))];
  if (route.kind === 'file' && hasProject()) crumbs = [h('span', {}, scopeOf(route.path).label), sep(), crumbs];

  bar.replaceChildren(...nodes(
    h('div', { class: 'crumbs' }, crumbs),
    h('div', { id: 'save-status' }),
    skill && h('button', { class: 'btn', onclick: () => setEnabled(skill, !skill.enabled) },
      icon(skill.enabled ? 'archive' : 'restore'), skill.enabled ? 'Move to purgatory' : 'Restore'),
    skill && h('button', { class: 'btn', onclick: () => deleteSkill(skill) }, icon('trash'), 'Delete'),
  ));
  renderStatus();
}

function renderStatus() {
  const el = document.getElementById('save-status');
  if (!el) return;
  const open = state.open;
  let cls = '';
  let content = null;
  if (state.offline) {
    cls = 'error';
    content = [icon('offline'), 'Disconnected'];
  } else if (open && !open.binary) {
    if (open.saveError) [cls, content] = ['error', [icon('error'), 'Not saved']];
    else if (open.conflict) [cls, content] = ['error', 'Conflict'];
    else if (isDirty()) [cls, content] = ['unsaved', 'Editing'];
    else content = [icon('check'), 'Saved'];
  }
  el.className = `save-status ${cls}`;
  el.replaceChildren(...nodes(content));
}

function renderStrip() {
  const strip = document.getElementById('strip');
  const conflict = state.open?.conflict;
  if (conflict === 'deleted') {
    strip.replaceChildren(h('div', { class: 'strip' }, icon('warning'),
      h('span', { class: 'grow' }, 'This file was moved or deleted on disk. Edits here will not be saved.')));
  } else if (conflict === 'changed') {
    strip.replaceChildren(h('div', { class: 'strip' }, icon('warning'),
      h('span', { class: 'grow' }, 'This file changed on disk while you were editing it.'),
      h('button', { class: 'btn outline', onclick: () => resolveConflict(false) }, 'Use disk version'),
      h('button', { class: 'btn outline', onclick: () => resolveConflict(true) }, 'Keep mine')));
  } else {
    strip.replaceChildren();
  }
}

function renderPageHead() {
  const head = document.getElementById('page-head');
  const path = state.route.path;
  const skill = skillOf(path);
  const scope = scopeOf(path);
  const isProject = scope.kind !== GLOBAL;

  if (!skill) {
    const rel = inScope(path);
    head.replaceChildren(
      h('div', { class: 'eyebrow', title: scope.root },
        rel !== 'AGENTS.md' ? scope.root : isProject ? `Project instructions · ${scope.label}` : 'Global instructions'),
      h('h1', { class: 'title' }, rel),
    );
    return;
  }

  const chars = skill.descriptionChars;
  const issues = skill.issues ?? [];
  head.replaceChildren(...nodes(
    h('div', { class: `eyebrow${skill.enabled ? '' : ' parked'}` },
      [isProject ? `Project skill · ${scope.label}` : 'Skill', !skill.enabled && ' · in purgatory, hidden from every harness']),
    h('h1', { class: 'title' }, skill.name),
    (skill.manualOnly || skill.local || state.usage) && h('div', { class: 'meta' },
      state.usage && usageChip(skill),
      skill.manualOnly && h('span', { class: 'chip', title: 'disable-model-invocation: true' }, 'Manual invocation only'),
      skill.local && h('span', { class: 'chip', title: LOCAL_HINT }, 'Machine-local')),
    issues.length > 0 && h('ul', { class: 'issues' },
      issues.map((i) => h('li', { class: i.severity }, icon(i.severity), h('span', { class: 'message' }, i.message),
        i.fix && h('button', { class: 'btn outline', onclick: (e) => applyFix(skill, i, e.currentTarget) }, i.fix.label)))),
    renderSkillFiles(skill, path),
  ));
  state.frontmatterNote?.replaceChildren(
    h('span', {
      title: skill.manualOnly ? 'Manual-only: the description never enters the model’s context'
        : 'Description, loaded into the context of every agent session',
    }, `≈${fmt(tokens(chars))} tokens`, h('span', { class: 'muted' }, `≈${fmt(claudeTokens(chars))} on Claude`)),
    h('span', { class: chars > DESCRIPTION_LIMIT ? 'over' : '', title: 'Description length against the spec limit' },
      `${fmt(chars)} / ${fmt(DESCRIPTION_LIMIT)} chars`,
      h('span', { class: 'gauge' }, h('i', { style: `width: ${Math.min(100, (chars / DESCRIPTION_LIMIT) * 100)}%` }))),
  );
}

function usageChip(skill) {
  const u = usageByName().get(skill.name);
  if (!u) return h('span', { class: 'chip', title: 'No session in any harness has used it' }, 'Never used');
  return h('span', { class: 'chip', title: u.total ? usageDetail(u) : null },
    `${plural(u.total, 'session')} ${periodText()}`, h('span', { class: 'muted' }, `last used ${daysAgo(u.last)}`));
}

// Files grouped by top-level folder: the skill's own root files first, then one
// labelled row per folder; a long row scrolls sideways, so every folder stays one line.
function renderSkillFiles(skill, activePath) {
  const groups = new Map([['', []]]);
  for (const f of skill.files) {
    const rel = f.path.slice(skill.dir.length + 1);
    const slash = rel.indexOf('/');
    const folder = slash < 0 ? '' : rel.slice(0, slash);
    if (!groups.has(folder)) groups.set(folder, []);
    groups.get(folder).push({ path: f.path, label: rel.slice(slash + 1), title: rel });
  }
  const tab = (f) => h('button', {
    class: `tab${f.path === activePath ? ' active' : ''}`,
    title: f.title,
    onclick: () => f.path !== activePath && openFile(f.path),
  }, h('span', { class: 'tab-label' }, f.label));

  return h('div', { class: 'files' }, [...groups].map(([folder, files]) => folder === ''
    ? h('div', { class: 'files-row root' }, files.map(tab),
      h('button', { class: 'tab add', title: 'New file', onclick: () => createFile(skill) }, icon('plus')))
    : [h('div', { class: 'files-folder' }, folder), h('div', { class: 'files-row' }, files.map(tab))]));
}

// ---------- rendering: overview ----------

// Project skills are marked with their directory, since several projects can load at once.
function projectTag(skill) {
  const scope = scopeOf(skill.dir);
  return scope.kind !== GLOBAL && h('span', { class: 'tag' }, scope.label);
}

function renderOverview() {
  const skills = allSkills();
  const enabled = skills.filter((s) => s.enabled);
  const auto = enabled.filter((s) => !s.manualOnly).sort((a, b) => listTokens(b) - listTokens(a));
  const parked = skills.length - enabled.length;
  const totalChars = sum(auto, listedChars);
  const totalTokens = sum(auto, listTokens);
  const maxTokens = Math.max(1, ...auto.map(listTokens));
  const issues = skills.flatMap((s) => (s.issues ?? []).map((i) => ({ ...i, skill: s })));
  const severityRank = { error: 0, warning: 1, info: 2 };
  issues.sort((a, b) => severityRank[a.severity] - severityRank[b.severity]);
  const errors = countBy(issues, 'error');
  const warnings = countBy(issues, 'warning');

  const tile = (label, value, note, tone, iconName) =>
    h('div', { class: `tile ${tone ?? ''}` },
      h('div', { class: 'tile-label' }, label),
      h('div', { class: 'tile-value' }, iconName && icon(iconName), value),
      h('div', { class: 'tile-note' }, note));

  const lintTone = errors ? 'bad' : warnings ? 'warn' : 'good';
  let harnessTile = h('div', {});
  const harnessRows = h('div', { class: 'rows' });

  const drawHarnesses = (checks) => {
    const linked = checks.filter((c) => c.status === 'linked').length;
    const next = tile('Harness links', `${linked}/${checks.length}`,
      linked === checks.length ? 'Every harness sees the shared files' : plural(checks.length - linked, 'link') + ' need attention',
      linked === checks.length ? 'good' : 'bad', linked === checks.length ? 'check' : 'warning');
    harnessTile.replaceWith(next);
    harnessTile = next;
    harnessRows.replaceChildren(...checks.map((c) => h('div', { class: 'row' },
      icon('link'),
      h('span', { class: 'name' }, c.harness),
      h('span', { class: 'mono' }, `${c.base}${c.path} → ${c.target}`),
      h('span', { class: `status ${c.status}`, title: c.detail || null },
        icon(c.status === 'linked' ? 'check' : 'warning'), c.detail ? `${c.status}: ${c.detail}` : c.status),
      c.status === 'missing' && h('button', { class: 'btn outline', onclick: () => createLink(c) }, 'Create link'))));
  };
  const createLink = async (check) => {
    try {
      await api('POST', '/api/harnesses', { scope: check.scope, path: check.path });
      state.harnesses = await api('GET', '/api/harnesses');
      drawHarnesses(state.harnesses);
    } catch (err) {
      fail(`Linking ${check.base}${check.path}`, err);
    }
  };
  if (state.harnesses) queueMicrotask(() => drawHarnesses(state.harnesses));
  api('GET', '/api/harnesses').then((checks) => {
    state.harnesses = checks;
    drawHarnesses(checks);
  }, (err) => fail('Checking harness links', err));

  const bars = h('div', { class: 'bars' }, auto.map((s) => h('div', {
    class: 'bar-row',
    onmousemove: (e) => showTooltip(e, `${s.name} · ≈${fmt(listTokens(s))} tokens (≈${fmt(listClaudeTokens(s))} on Claude) · ${((listTokens(s) / totalTokens) * 100).toFixed(1)}% of total`),
    onmouseleave: hideTooltip,
  },
  h('button', { class: 'bar-name', onclick: () => { hideTooltip(); openFile(skillEntry(s)); } }, s.name, projectTag(s)),
  h('div', { class: 'bar-track' }, h('div', { class: 'bar', style: `width: ${(listTokens(s) / maxTokens) * 100}%` })),
  h('span', { class: 'bar-value' }, fmt(listTokens(s))))));

  return h('div', { class: 'page wide' },
    h('div', { class: 'eyebrow' }, state.scopes.map((scope) => scope.label).join(' + ')),
    h('h1', { class: 'title' }, 'Overview'),
    h('div', { class: 'tiles' },
      tile('Enabled skills', String(enabled.length),
        `${auto.length} automatic · ${enabled.length - auto.length} manual${parked ? ` · ${parked} in purgatory` : ''}`),
      tile('Always-on context', `≈${fmt(totalTokens)}`,
        [`tokens · ≈${fmt(sum(auto, listClaudeTokens))} on Claude`, h('br'),
          h('span', { class: 'muted', title: `${fmt(totalChars)} chars of names and descriptions, plus ≈${LIST_ENTRY_TOKENS} tokens per skill for list formatting and the SKILL.md path` },
            `${fmt(totalChars)} chars + ≈${LIST_ENTRY_TOKENS} tokens per skill`)]),
      tile('Lint', issues.length ? plural(errors || warnings || issues.length, errors ? 'error' : warnings ? 'warning' : 'note') : 'Clean',
        issues.length ? `${plural(errors, 'error')} · ${plural(warnings, 'warning')}` : 'No issues in any skill',
        lintTone, { good: 'check', warn: 'warning', bad: 'error' }[lintTone]),
      harnessTile),
    usageSection(),
    h('section', { class: 'section' },
      h('div', { class: 'section-head' }, h('h2', {}, 'Context cost by skill'),
        h('p', {}, 'Tokens per skill list entry: name, description and formatting. Automatic skills are listed in every session.')),
      bars),
    h('section', { class: 'section' },
      h('div', { class: 'section-head' }, h('h2', {}, 'Issues'), issues.length > 0 && h('p', {}, plural(issues.length, 'finding'))),
      h('div', { class: `rows${issues.length ? ' table' : ''}` }, issues.length === 0
        ? h('div', { class: 'all-clear' }, icon('check'), 'Every skill passes lint.')
        : issues.map((i) => h('button', { class: `row ${i.severity}`, onclick: () => skillEntry(i.skill) && openFile(skillEntry(i.skill)) },
          icon(i.severity),
          h('span', { class: 'name' }, i.skill.name, projectTag(i.skill)),
          h('span', { class: 'detail' }, i.message))))),
    h('section', { class: 'section' },
      h('div', { class: 'section-head' }, h('h2', {}, 'Harness links'),
        h('p', {}, 'Harnesses that don’t read .agents natively need a symlink into it.')),
      harnessRows),
  );
}

// Sessions per skill, stacked by harness, plus the enabled skills nobody used.
function usageSection() {
  const head = (...extra) => h('div', { class: 'section-head' }, h('h2', {}, 'Usage'), ...extra);
  if (state.usageError) {
    return h('section', { class: 'section' }, head(h('p', {}, `Couldn’t read the session transcripts: ${state.usageError}`)));
  }
  if (!state.usage) return h('section', { class: 'section' }, head(h('p', {}, 'Reading session transcripts…')));

  const byName = usageByName();
  const skills = allSkills();
  const used = skills.filter((s) => byName.get(s.name)?.total).sort((a, b) => byName.get(b.name).total - byName.get(a.name).total);
  const unused = skills.filter((s) => s.enabled && !byName.get(s.name)?.total);
  const unusedAuto = unused.filter((s) => !s.manualOnly);
  const max = Math.max(1, ...used.map((s) => byName.get(s.name).total));
  const totals = Object.fromEntries(HARNESSES.map((hn) => [hn.id, sum(used, (s) => byName.get(s.name)[hn.id])]));
  const since = state.usage.reduce((min, u) => (u.day && u.day < min ? u.day : min), isoDay(new Date()));

  const section = h('section', { class: 'section' },
    head(
      h('p', { title: `From the Claude Code, Codex and Pi transcripts on this machine, since ${since}` }, 'Sessions that used each skill'),
      h('div', { class: 'segmented' }, USAGE_PERIODS.map((p) => h('button', {
        class: p === state.usagePeriod ? 'active' : '',
        onclick: () => {
          state.usagePeriod = p;
          section.replaceWith(usageSection());
        },
      }, p.label)))),
    h('div', { class: 'legend' }, HARNESSES.map((hn) =>
      h('span', {}, h('i', { style: `background: var(--harness-${hn.id})` }), hn.label, h('span', { class: 'muted' }, fmt(totals[hn.id]))))),
    used.length === 0
      ? h('div', { class: 'rows' }, h('div', { class: 'all-clear' }, `No skill was used ${periodText()}.`))
      : h('div', { class: 'bars' }, used.map((s) => {
        const u = byName.get(s.name);
        return h('div', {
          class: 'bar-row',
          onmousemove: (e) => showTooltip(e, `${s.name} · ${plural(u.total, 'session')} · ${usageDetail(u)} · last used ${daysAgo(u.last)}`),
          onmouseleave: hideTooltip,
        },
        h('button', { class: 'bar-name', onclick: () => { hideTooltip(); openFile(skillEntry(s)); } }, s.name, projectTag(s)),
        h('div', { class: 'bar-track' }, h('div', { class: 'bar stack', style: `width: ${(u.total / max) * 100}%` },
          HARNESSES.filter((hn) => u[hn.id]).map((hn) => h('i', { style: `flex-grow: ${u[hn.id]}; background: var(--harness-${hn.id})` })))),
        h('span', { class: 'bar-value' }, fmt(u.total)));
      })),
    unused.length > 0 && h('div', { class: 'unused' },
      h('span', { class: 'unused-label' }, `Unused ${periodText()}`,
        unusedAuto.length > 0 && h('span', { class: 'muted' }, ` · ${plural(unusedAuto.length, 'automatic skill')} ${unusedAuto.length === 1 ? 'costs' : 'cost'} ≈${fmt(sum(unusedAuto, listTokens))} tokens every session`)),
      h('div', { class: 'unused-list' }, unused.map((s) =>
        h('button', { class: 'chip', onclick: () => openFile(skillEntry(s)) }, s.name, projectTag(s))))),
  );
  return section;
}

// ---------- theme ----------

const currentTheme = () => THEMES.find((t) => t.id === document.documentElement.dataset.theme) ?? THEMES[0];

function setTheme(theme) {
  document.documentElement.dataset.theme = theme.id;
  document.cookie = `theme=${theme.id}; max-age=${THEME_COOKIE_MAX_AGE}; path=/; SameSite=Strict`;
  renderThemeSwitch();
  syncFavicon();
}

function renderThemeSwitch() {
  const active = currentTheme();
  document.getElementById('theme-switch').replaceChildren(...THEMES.map((theme) =>
    h('button', {
      class: theme === active ? 'swatch active' : 'swatch',
      'data-theme': theme.id,
      title: `${theme.label} theme`,
      'aria-label': `${theme.label} theme`,
      'aria-pressed': String(theme === active),
      onclick: () => setTheme(theme),
    })));
}

// The window's icon is the brand mark painted in the current theme.
function syncFavicon() {
  const css = getComputedStyle(document.documentElement);
  const mark = document.querySelector('.brand-mark').cloneNode(true);
  mark.setAttribute('xmlns', 'http://www.w3.org/2000/svg');
  mark.querySelector('rect').setAttribute('fill', css.getPropertyValue('--accent'));
  mark.querySelector('path').setAttribute('fill', css.getPropertyValue('--on-accent'));
  document.querySelector('link[rel="icon"]').href = `data:image/svg+xml,${encodeURIComponent(mark.outerHTML)}`;
}

// ---------- boot ----------

for (const el of document.querySelectorAll('[data-icon]')) el.replaceWith(icon(el.dataset.icon));
document.getElementById('search-trigger').addEventListener('click', openPalette);
renderThemeSwitch();
syncFavicon();
document.addEventListener('keydown', (e) => {
  const mod = e.ctrlKey || e.metaKey;
  if (mod && e.key.toLowerCase() === 'k') {
    e.preventDefault();
    openPalette();
  } else if (mod && e.key.toLowerCase() === 's') {
    e.preventDefault();
    flushSave();
  }
});

// Keeps the server alive; it exits shortly after this window closes.
const live = new EventSource('/api/live');
live.onopen = () => setOffline(false);
live.onerror = () => setOffline(true);
// A launch for other projects took this window over; save, then follow it. Edits that
// could not be saved keep the window here rather than lose them.
live.addEventListener('move', async (e) => {
  await flushSave();
  if (isDirty()) {
    toast('Another Skillboard was opened, but this file has unsaved changes. Resolve them, then launch it again.');
    return;
  }
  live.close();
  location.replace(e.data);
});

await refreshTree();
if (state.scopes) {
  const project = state.scopes.find((scope) => scope.kind !== GLOBAL);
  if (project) document.title = `${project.label} · Skillboard`;
  await navigate(routeFromHash(), { replace: true });
  setInterval(refreshTree, POLL_MS);
  api('GET', '/api/usage').then((uses) => { state.usage = uses; }, (err) => {
    state.usageError = err.message;
    fail('Reading skill usage', err);
  }).then(() => (state.route.kind === 'overview' ? renderMain() : renderChrome()));
} else {
  document.getElementById('main').replaceChildren(h('div', { class: 'empty-state' }, 'Skillboard is not running. Start it with `skills`.'));
}
