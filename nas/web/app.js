const SOURCE_LABELS = {
  'hongguo': '红果', 'huangdou': '黄豆', 'huangju': '黄剧',
  'huangguo-video': '黄果·视频', 'huangguoai': '黄果·AI', 'cloudfront': '黄果·原'
};

const state = {
  source: 'hongguo', sources: ['hongguo'], category: '', query: '', page: 1,
  items: [], dramas: {}, jobs: [], chapters: [], selected: new Set(),
  current: null, detailJob: {}, tab: 'browse', timer: null
};

async function api(path, body) {
  const options = body === undefined ? {} : {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'guoapp-nas' },
    body: JSON.stringify(body)
  };
  const response = await fetch(path, options);
  if (response.status === 401) { location.href = '/login'; throw new Error('需要重新登录'); }
  const result = await response.json();
  if (!result.ok) throw new Error(result.error || '请求失败');
  return result.data;
}
const rpc = (action, fields) => api('/api/rpc', Object.assign({ action }, fields || {}));

function toast(message) {
  const node = document.getElementById('toast');
  node.textContent = message;
  node.classList.add('show');
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => node.classList.remove('show'), 2600);
}

function bytes(value) {
  if (!value) return '-';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let index = 0, size = value;
  while (size >= 1024 && index < units.length - 1) { size /= 1024; index++; }
  return size.toFixed(size >= 100 || index === 0 ? 0 : 1) + ' ' + units[index];
}

function coverURL(drama) {
  if (!drama.cover) return '';
  return '/cover?id=' + encodeURIComponent(drama.id) +
    '&source=' + encodeURIComponent(drama.source || '') +
    '&cover=' + encodeURIComponent(drama.cover);
}

function dramaSource(drama) {
  return drama.source || (drama.id || '').split(':')[0];
}

function remember(drama) {
  if (drama && drama.id) state.dramas[drama.id] = Object.assign(state.dramas[drama.id] || {}, drama);
  return drama;
}

function el(tag, className, html) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (html !== undefined) node.innerHTML = html;
  return node;
}

async function boot() {
  const info = await api('/api/state');
  state.sources = info.sources;
  state.source = info.sources[0];
  const select = document.getElementById('source');
  select.innerHTML = '';
  info.sources.forEach((code) => {
    const option = document.createElement('option');
    option.value = code;
    option.textContent = SOURCE_LABELS[code] || code;
    select.appendChild(option);
  });
  select.onchange = () => { state.source = select.value; state.category = ''; state.page = 1; loadCategories(); loadCatalog(); };
  document.getElementById('state').textContent =
    (info.allSources ? '全站源版' : '仅红果版') + ' · ' + (info.directory || '');
  await loadCategories();
  await loadCatalog();
  switchTab('browse');
}

async function loadCategories() {
  const wrap = document.getElementById('categories');
  wrap.innerHTML = '<span class="muted">分类加载中…</span>';
  try {
    const data = await rpc('categories', { source: state.source });
    wrap.innerHTML = '';
    (data.items || data || []).forEach((item) => {
      const button = el('button', (item.id || '') === state.category ? 'active' : '');
      button.textContent = item.name || '全部';
      button.onclick = () => {
        state.category = item.id || '';
        state.page = 1;
        loadCategories();
        loadCatalog();
      };
      wrap.appendChild(button);
    });
  } catch (error) {
    wrap.innerHTML = '';
    wrap.appendChild(el('span', 'muted', '分类不可用：' + error.message));
  }
}

async function loadCatalog() {
  const grid = document.getElementById('grid');
  const notice = document.getElementById('notice');
  grid.innerHTML = '';
  notice.classList.add('hidden');
  notice.textContent = '';
  grid.appendChild(el('span', 'muted', '加载中…'));
  try {
    const data = await rpc('catalog', {
      source: state.source, category: state.category, query: state.query, page: state.page
    });
    state.items = data.items || [];
    if (data.warning) { notice.textContent = data.warning; notice.classList.remove('hidden'); }
    document.getElementById('page').textContent = '第 ' + (data.page || state.page) + ' 页 · ' + state.items.length + ' 部';
    document.getElementById('prev').disabled = state.page <= 1;
    document.getElementById('next').disabled = !data.hasMore;
    grid.innerHTML = '';
    if (!state.items.length) { grid.appendChild(el('p', 'muted', '没有结果，换个关键词试试')); return; }
    state.items.forEach((drama) => {
      remember(drama);
      const card = el('div', 'card-drama');
      const image = el('img', 'poster');
      image.loading = 'lazy';
      const url = coverURL(drama);
      if (url) image.src = url; else image.style.visibility = 'hidden';
      image.onerror = () => { image.style.visibility = 'hidden'; };
      const body = el('div', 'card-body');
      body.appendChild(el('p', 'card-title', escapeText(drama.title || '未命名')));
      const meta = el('div', 'card-meta');
      meta.appendChild(el('span', '', (drama.episodes || 0) + ' 集'));
      if (drama.releaseStatus) meta.appendChild(el('span', '', drama.releaseStatus));
      if (drama.heat) meta.appendChild(el('span', '', drama.heat));
      body.appendChild(meta);
      card.appendChild(image);
      card.appendChild(body);
      card.onclick = () => openDetail(drama);
      grid.appendChild(card);
    });
  } catch (error) {
    grid.innerHTML = '';
    grid.appendChild(el('p', 'error', '加载失败：' + error.message));
  }
}

function escapeText(value) {
  return String(value).replace(/[&<>"]/g, (character) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[character]));
}

async function openDetail(drama) {
  state.current = drama;
  state.selected = new Set();
  state.chapters = [];
  document.getElementById('modal').classList.remove('hidden');
  document.getElementById('detail-title').textContent = drama.title || '加载中';
  document.getElementById('detail-meta').textContent = '正在读取分集…';
  document.getElementById('detail-desc').textContent = drama.description || '';
  document.getElementById('episodes').innerHTML = '';
  updateDetailCount();
  try {
    const data = await rpc('detail', { drama });
    state.chapters = data.chapters || [];
    document.getElementById('detail-meta').textContent =
      (SOURCE_LABELS[dramaSource(data.drama || drama)] || '') + ' · 共 ' + state.chapters.length + ' 集';
    renderEpisodes();
  } catch (error) {
    document.getElementById('detail-meta').textContent = '分集读取失败：' + error.message;
  }
}

function renderEpisodes() {
  const wrap = document.getElementById('episodes');
  wrap.innerHTML = '';
  const grid = el('div', 'episode-grid');
  state.chapters.forEach((chapter, index) => {
    const number = index + 1;
    const label = el('label', 'episode' + (isDownloaded(number) ? ' done' : ''));
    const box = document.createElement('input');
    box.type = 'checkbox';
    box.checked = state.selected.has(number);
    box.onchange = () => {
      if (box.checked) state.selected.add(number); else state.selected.delete(number);
      updateDetailCount();
    };
    label.appendChild(box);
    label.appendChild(el('span', '', '第 ' + number + ' 集' + (isDownloaded(number) ? ' ✓' : '')));
    grid.appendChild(label);
  });
  wrap.appendChild(grid);
}

function isDownloaded(number) {
  const drama = state.current;
  if (!drama) return false;
  return state.jobs.some((job) => job.drama.id === drama.id && job.index === number && job.state === 'completed');
}

function updateDetailCount() {
  document.getElementById('detail-count').textContent = '已选 ' + state.selected.size + ' 集';
}

async function enqueue() {
  if (!state.selected.size) { toast('先勾选要下载的集数'); return; }
  const drama = state.current;
  const entries = [];
  state.chapters.forEach((chapter, index) => {
    if (state.selected.has(index + 1)) {
      entries.push({
        chapter: {
          id: chapter.id, source: chapter.source, title: chapter.title,
          currentEpisode: chapter.currentEpisode, mediaSize: chapter.mediaSize
        },
        index: index + 1
      });
    }
  });
  try {
    const data = await rpc('enqueueDownloads', {
      drama, entries, quality: Number(document.getElementById('detail-quality').value)
    });
    toast('已加入下载：新增 ' + data.added + ' 集');
    document.getElementById('modal').classList.add('hidden');
    switchTab('downloads');
    await refreshJobs();
  } catch (error) {
    toast('加入失败：' + error.message);
  }
}

async function refreshJobs() {
  try {
    state.jobs = (await rpc('downloads')).jobs || [];
  } catch (error) {
    state.jobs = [];
  }
  const active = state.jobs.filter((job) => job.state === 'downloading' || job.state === 'queued').length;
  document.getElementById('badge-downloads').textContent = active ? '(' + active + ')' : '';
  if (state.tab === 'downloads') renderDownloads();
  if (state.tab === 'merges') renderMergeTab();
}

function renderDownloads() {
  const wrap = document.getElementById('download-groups');
  const summary = document.getElementById('download-summary');
  wrap.innerHTML = '';
  if (!state.jobs.length) { wrap.appendChild(el('p', 'muted', '还没有下载任务')); return; }
  const groups = new Map();
  state.jobs.forEach((job) => {
    if (!groups.has(job.drama.id)) groups.set(job.drama.id, []);
    groups.get(job.drama.id).push(job);
  });
  const total = state.jobs.length, done = state.jobs.filter((job) => job.state === 'completed').length;
  const size = state.jobs.reduce((sum, job) => sum + (job.bytes || 0), 0);
  summary.textContent = total + ' 个任务 · 完成 ' + done + ' · 已下载 ' + bytes(size);
  groups.forEach((jobs, id) => {
    jobs.sort((a, b) => a.index - b.index);
    const drama = jobs[0].drama;
    remember(drama);
    const group = el('div', 'group');
    const head = el('div', 'group-head');
    const url = coverURL(drama);
    if (url) {
      const image = el('img');
      image.src = url;
      image.onerror = () => image.remove();
      head.appendChild(image);
    }
    head.appendChild(el('strong', '', escapeText(drama.title || id)));
    head.appendChild(el('span', 'muted', jobs.length + ' 集'));
    head.appendChild(el('span', 'spacer'));
    const merge = el('button', 'primary', '合并此剧已下载集');
    merge.onclick = () => { switchTab('merges'); prepareMerge(id); };
    head.appendChild(merge);
    group.appendChild(head);
    const table = el('table');
    table.innerHTML = '<tr><th>集数</th><th>状态</th><th>进度</th><th>大小</th><th></th></tr>';
    jobs.forEach((job) => {
      const row = document.createElement('tr');
      row.innerHTML = '<td>第 ' + job.index + ' 集</td>' +
        '<td><span class="pill ' + job.state + '">' + STATE_LABELS[job.state] + '</span>' +
        (job.error ? '<div class="muted">' + escapeText(job.error) + '</div>' : '') + '</td>' +
        '<td><div class="bar"><i style="width:' + Math.round((job.progress || 0) * 100) + '%"></i></div></td>' +
        '<td>' + bytes(job.bytes) + '</td><td></td>';
      const cell = row.lastElementChild;
      const actions = {
        queued: [['pause', '暂停'], ['remove', '删除']],
        downloading: [['pause', '暂停'], ['remove', '删除']],
        paused: [['resume', '继续'], ['remove', '删除']],
        failed: [['resume', '重试'], ['remove', '删除']],
        completed: [['archive', '标记已整理'], ['remove', '删除文件']]
      };
      (actions[job.state] || []).forEach(([command, label]) => {
        const button = el('button', 'ghost', label);
        button.onclick = async () => {
          try {
            await rpc('controlDownloads', { jobId: job.id, command });
            await refreshJobs();
          } catch (error) { toast('操作失败：' + error.message); }
        };
        cell.appendChild(button);
      });
      table.appendChild(row);
    });
    group.appendChild(table);
    wrap.appendChild(group);
  });
}

const STATE_LABELS = {
  queued: '排队中', downloading: '下载中', paused: '已暂停',
  failed: '失败', completed: '已完成', removing: '删除中'
};

async function controlAll(command) {
  try {
    await rpc('controlDownloads', { jobId: '', command });
    await refreshJobs();
  } catch (error) { toast('操作失败：' + error.message); }
}

function completedIds(dramaID) {
  return state.jobs
    .filter((job) => job.drama.id === dramaID && job.state === 'completed')
    .map((job) => job.index);
}

function prepareMerge(dramaID) {
  const select = document.getElementById('merge-drama');
  select.value = dramaID || select.value;
  renderMergeTab();
}

function renderMergeTab() {
  const select = document.getElementById('merge-drama');
  const current = select.value;
  const options = [];
  const seen = new Set();
  state.jobs.forEach((job) => {
    if (job.state !== 'completed' || seen.has(job.drama.id)) return;
    seen.add(job.drama.id);
    options.push(job.drama);
  });
  select.innerHTML = '';
  options.forEach((drama) => {
    const option = document.createElement('option');
    option.value = drama.id;
    option.textContent = drama.title + '（' + completedIds(drama.id).length + ' 集已下载）';
    select.appendChild(option);
  });
  if (!options.length) {
    select.innerHTML = '<option value="">暂无可合并的已下载剧集</option>';
  }
  if (current) select.value = current;
  const list = state.jobs.filter((job) => job.drama.id === select.value && job.state === 'completed')
    .map((job) => job.index).sort((a, b) => a - b);
  if (list.length) {
    document.getElementById('merge-from').value = list[0];
    document.getElementById('merge-to').value = list[list.length - 1];
  }
  renderMergeJobs();
}

async function renderMergeJobs() {
  let data;
  try {
    data = await api('/api/merge/list');
  } catch (error) { return; }
  const wrap = document.getElementById('merge-jobs');
  wrap.innerHTML = '';
  const jobs = data.jobs || [];
  const active = jobs.filter((job) => job.state === 'running' || job.state === 'queued').length;
  document.getElementById('badge-merges').textContent = active ? '(' + active + ')' : '';
  if (!jobs.length) { wrap.appendChild(el('p', 'muted', '还没有合并任务')); }
  jobs.forEach((job) => {
    const group = el('div', 'group');
    const head = el('div', 'group-head');
    head.appendChild(el('strong', '', escapeText((job.drama && job.drama.title) || '合并任务')));
    head.appendChild(el('span', 'pill ' + job.state, MERGE_STATE[job.state] || job.state));
    head.appendChild(el('span', 'muted', escapeText(job.stage || '')));
    head.appendChild(el('span', 'spacer'));
    ({
      running: [['cancel', '取消']], queued: [['cancel', '取消']],
      failed: [['retry', '重试'], ['remove', '移除']]
    }[job.state] || []).forEach(([command, label]) => {
      const button = el('button', 'ghost', label);
      button.onclick = async () => {
        try { await api('/api/merge/control', { id: job.id, command }); renderMergeJobs(); } catch (error) { toast(error.message); }
      };
      head.appendChild(button);
    });
    group.appendChild(head);
    group.appendChild(el('div', '', '<div style="padding:10px 14px"><div class="bar"><i style="width:' +
      Math.round((job.progress || 0) * 100) + '%"></i></div>' +
      (job.error ? '<div class="error">' + escapeText(job.error) + '</div>' : '') + '</div>'));
    wrap.appendChild(group);
  });
  renderLibrary(data.items || []);
}

const MERGE_STATE = { queued: '排队中', running: '处理中', failed: '未完成', done: '已完成' };

async function submitMerge() {
  const id = document.getElementById('merge-drama').value;
  if (!id) { toast('请先选择剧集'); return; }
  const from = Number(document.getElementById('merge-from').value);
  const to = Number(document.getElementById('merge-to').value);
  const ids = state.jobs
    .filter((job) => job.drama.id === id && job.state === 'completed' && job.index >= from && job.index <= to)
    .map((job) => job.id);
  if (ids.length < 2) { toast('范围内没有足够的已下载分集'); return; }
  try {
    await api('/api/merge', { jobIds: ids });
    toast('合并任务已加入队列');
    switchTab('merges');
    renderMergeJobs();
  } catch (error) { toast('提交失败：' + error.message); }
}

function renderLibrary(items) {
  const wrap = document.getElementById('library-items');
  wrap.innerHTML = '';
  if (!items.length) { wrap.appendChild(el('p', 'muted', '还没有合并成品')); return; }
  items.forEach((item) => {
    const group = el('div', 'group');
    const head = el('div', 'group-head');
    head.appendChild(el('strong', '', escapeText((item.drama && item.drama.title) || '成品')));
    head.appendChild(el('span', 'muted', episodeLabel(item.episodes) + ' · ' + bytes(item.bytes) +
      ' · ' + Math.round(item.duration || 0) + ' 秒' +
      (item.videoTranscodes || item.audioTranscodes ? ' · 统一格式 ' + (item.videoTranscodes || 0) + ' 段视频/' + (item.audioTranscodes || 0) + ' 段音轨' : '')));
    head.appendChild(el('span', 'spacer'));
    const download = el('a');
    download.href = '/file?id=' + item.id + '&download=1';
    download.innerHTML = '<button>下载成品</button>';
    head.appendChild(download);
    const exportButton = el('button', 'ghost', '导出到媒体目录');
    exportButton.onclick = async () => {
      try {
        const data = await api('/api/library/export', { id: item.id });
        toast('已导出：' + data.folder);
      } catch (error) { toast('导出失败：' + error.message); }
    };
    head.appendChild(exportButton);
    const remove = el('button', 'ghost', '删除');
    remove.onclick = async () => {
      if (!confirm('删除成品文件？此操作不可恢复')) return;
      try { await api('/api/library/delete', { id: item.id, file: true }); renderMergeJobs(); } catch (error) { toast(error.message); }
    };
    head.appendChild(remove);
    group.appendChild(head);
    if (item.exports && item.exports.length) {
      group.appendChild(el('div', '', '<div class="group-head muted">已导出：' +
        item.exports.map((entry) => escapeText(entry.file)).join('；') + '</div>'));
    }
    wrap.appendChild(group);
  });
}

function episodeLabel(episodes) {
  if (!episodes || !episodes.length) return '';
  const sorted = episodes.slice().sort((a, b) => a - b);
  const first = sorted[0], last = sorted[sorted.length - 1];
  return last - first + 1 === sorted.length ? '第 ' + first + '-' + last + ' 集' : '共 ' + sorted.length + ' 集';
}

function switchTab(name) {
  state.tab = name;
  document.querySelectorAll('nav#tabs button').forEach((button) => {
    button.classList.toggle('active', button.dataset.tab === name);
  });
  ['browse', 'downloads', 'merges', 'library', 'settings'].forEach((key) => {
    document.getElementById('tab-' + key).classList.toggle('hidden', key !== name);
  });
  if (name === 'downloads') renderDownloads();
  if (name === 'merges') renderMergeTab();
  if (name === 'library') renderMergeJobs();
  if (name === 'settings') loadSettings();
}

function toggleProxyURL() {
  document.getElementById('proxy-url-row').classList.toggle('hidden',
    document.getElementById('proxy-mode').value !== 'manual');
}

async function loadSettings() {
  const note = document.getElementById('settings-note');
  note.textContent = '读取中…';
  try {
    const data = await rpc('resourceSettings');
    document.getElementById('proxy-mode').value = data.proxyMode || 'auto';
    document.getElementById('proxy-url').value = data.proxyUrl || '';
    document.getElementById('catalog-concurrency').value = data.catalogConcurrency || 3;
    document.getElementById('catalog-interval').value =
      data.catalogIntervalMs === undefined ? 250 : data.catalogIntervalMs;
    document.getElementById('download-concurrency').value = data.downloadConcurrency || 2;
    document.getElementById('download-by-source').checked = !!data.downloadBySource;
    document.getElementById('proxy-status').textContent = data.systemProxyStatus || '';
    toggleProxyURL();
    note.textContent = data.warning || '已读取当前设置';
  } catch (error) {
    note.textContent = '读取失败：' + error.message;
  }
}

async function saveSettings() {
  const note = document.getElementById('settings-note');
  const whole = (id, fallback, minimum) => {
    const value = Number(document.getElementById(id).value);
    return Number.isFinite(value) && value >= minimum ? Math.round(value) : fallback;
  };
  try {
    const data = await rpc('saveResourceSettings', {
      settings: {
        proxyMode: document.getElementById('proxy-mode').value,
        proxyUrl: document.getElementById('proxy-url').value.trim(),
        catalogConcurrency: whole('catalog-concurrency', 3, 1),
        catalogIntervalMs: whole('catalog-interval', 250, 0),
        downloadConcurrency: whole('download-concurrency', 2, 1),
        downloadBySource: document.getElementById('download-by-source').checked
      }
    });
    document.getElementById('proxy-status').textContent = data.systemProxyStatus || '';
    note.textContent = '已保存当前设置';
    toast('网络与下载设置已保存');
  } catch (error) {
    note.textContent = '保存失败：' + error.message;
    toast('保存失败：' + error.message);
  }
}

function applyRange(text) {
  const numbers = new Set();
  String(text || '').split(',').forEach((part) => {
    const range = part.trim().match(/^(\d+)\s*[-–]\s*(\d+)$/);
    const single = part.trim().match(/^(\d+)$/);
    if (range) {
      const from = Number(range[1]), to = Number(range[2]);
      for (let number = Math.min(from, to); number <= Math.max(from, to); number++) numbers.add(number);
    } else if (single) numbers.add(Number(single[1]));
  });
  state.selected = new Set([...numbers].filter((number) => number >= 1 && number <= state.chapters.length));
  renderEpisodes();
  updateDetailCount();
}

document.getElementById('search').addEventListener('submit', (event) => {
  event.preventDefault();
  state.query = document.getElementById('query').value.trim();
  state.page = 1;
  loadCatalog();
});
document.getElementById('prev').onclick = () => { if (state.page > 1) { state.page--; loadCatalog(); } };
document.getElementById('next').onclick = () => { state.page++; loadCatalog(); };
document.querySelectorAll('nav#tabs button').forEach((button) => {
  button.onclick = () => switchTab(button.dataset.tab);
});
document.getElementById('detail-close').onclick = () => document.getElementById('modal').classList.add('hidden');
document.getElementById('modal').onclick = (event) => {
  if (event.target.id === 'modal') document.getElementById('modal').classList.add('hidden');
};
document.getElementById('detail-download').onclick = enqueue;
document.querySelector('[data-range="all"]').onclick = () => {
  state.selected = new Set(state.chapters.map((_, index) => index + 1));
  renderEpisodes(); updateDetailCount();
};
document.querySelector('[data-range="none"]').onclick = () => {
  state.selected = new Set();
  renderEpisodes(); updateDetailCount();
};
document.getElementById('range-apply').onclick = () => applyRange(document.getElementById('range-text').value);
document.getElementById('pause-all').onclick = () => controlAll('pauseAll');
document.getElementById('resume-all').onclick = () => controlAll('resumeAll');
document.getElementById('merge-submit').onclick = submitMerge;
document.getElementById('merge-drama').onchange = renderMergeTab;
document.getElementById('proxy-mode').onchange = toggleProxyURL;
document.getElementById('settings-save').onclick = saveSettings;
document.getElementById('logout').onclick = async () => {
  try { await api('/api/logout', {}); } catch (error) { /* 忽略 */ }
  location.href = '/login';
};

setInterval(() => {
  if (!document.hidden && state.tab !== 'browse') refreshJobs();
}, 2000);

boot().then(refreshJobs).catch((error) => toast(error.message));
