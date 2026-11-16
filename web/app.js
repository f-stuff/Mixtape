'use strict';
const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => Array.from(document.querySelectorAll(selector));
const ui = {
  tab: 'discover', artists: [], albums: [], artist: null, selected: new Set(),
  queueFilter: 'all', expanded: new Set(), collapsed: new Set(), queueState: null, busy: false, toastTimer: null,
  closed: false, seenBackend: false, failedPolls: 0, sessionID: null, pauseBusy: false,
  expandedErrors: new Set(), dismissedAppRelease: null,
  pageID: (globalThis.crypto?.randomUUID?.() ||
    `${Date.now().toString(16)}-${Math.random().toString(16).slice(2)}-${Math.random().toString(16).slice(2)}`),
};
const activeStatuses = new Set(['queued', 'preparing', 'searching', 'downloading', 'tagging']);
const runningStatuses = new Set(['preparing', 'searching', 'downloading', 'tagging']);
const labelForStatus = {
  queued: 'Waiting', preparing: 'Preparing', searching: 'Matching', downloading: 'Downloading',
  tagging: 'Tagging', review: 'Review', done: 'Done', failed: 'Failed', skipped: 'Skipped',
};
function esc(input) {
  return String(input ?? '').replace(/[&<>"']/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]));
}
function toast(message, isError = false) {
  const el = $('#toast');
  el.textContent = message;
  el.style.borderColor = isError ? '#cb7c89' : '#8a679d';
  el.hidden = false;
  el.classList.add('show');
  clearTimeout(ui.toastTimer);
  ui.toastTimer = setTimeout(() => { el.classList.remove('show'); el.hidden = true; }, 4400);
}
async function getJSON(path) {
  const res = await fetch(path, { cache: 'no-store', headers: path === '/api/state' ? {'X-Mixtape-Page': ui.pageID} : {} });
  if (!res.ok) throw new Error((await res.text()).trim() || `Request failed (${res.status})`);
  return res.json();
}
async function postJSON(path, data = {}) {
  const res = await fetch(path, {method: 'POST', headers: {'Content-Type': 'application/json', 'X-Mixtape': '1'}, body: JSON.stringify(data)});
  if (!res.ok) throw new Error((await res.text()).trim() || `Request failed (${res.status})`);
  return res.json();
}
function switchTab(tab) {
  ui.tab = tab;
  $$('.view').forEach(el => el.classList.toggle('active', el.id === `view-${tab}`));
  $$('[data-tab]').forEach(el => el.classList.toggle('active', el.dataset.tab === tab));
  $('#breadcrumbs').innerHTML = `MIXTAPE <span>/</span> ${esc(tab.toUpperCase().replace('-', ' '))}`;
  $('#selectionBar').hidden = tab !== 'discover' || ui.selected.size === 0;
  if (tab === 'queue') renderQueue();
  if (tab === 'settings') fillSettings();
  window.scrollTo({top: 0, behavior: 'smooth'});
}
$$('[data-tab]').forEach(el => el.addEventListener('click', () => switchTab(el.dataset.tab)));
$('#topOpenFolder').addEventListener('click', openMusicFolder);
$('#openMusicQueue').addEventListener('click', openMusicFolder);
async function openMusicFolder() {
  try {await postJSON('/api/library/open');}
  catch (err) {toast(`Couldn't open the folder: ${err.message}`, true);}
}
function loadingBlock(message) {
  return `<div class="loadingCard"><span class="spin">✳</span>${esc(message)}</div>`;
}
async function searchArtists(q) {
  const query = q.trim();
  if (query.length < 2) {toast('Type at least 2 letters to search.', true);return;}
  $('#artistSearch').value = query;
  ui.artist = null;
  ui.artists = [];
  ui.albums = [];
  ui.selected.clear();
  updateSelectionBar();
  $('#discoverEmpty').hidden = true;
  $('#albumResults').hidden = true;
  $('#artistResults').hidden = false;
  $('#artistResults').innerHTML = loadingBlock(`Finding artists matching “${query}”…`);
  $('#artistSearchButton').disabled = true;
  try {
    const results = await getJSON('/api/artists?q=' + encodeURIComponent(query));
    ui.artists = results;
    if (!results.length) {
      $('#artistResults').innerHTML = `<div class="loadingCard">Couldn't find “${esc(query)}”. Try another spelling or a shorter name.</div>`;
    } else {
      $('#artistResults').innerHTML = `<div class="resultsHeader"><div><h2>Is this your artist?</h2><p>Pick the right one to see their albums.</p></div></div>
        <div class="artistList">${results.map((artist, i) => `<button class="artistResult" data-artist-index="${i}">
          <span class="artistInitial">${esc(artist.name.slice(0, 1).toUpperCase())}</span>
          <span><strong>${esc(artist.name)}</strong><small>${esc([artist.description, artist.country].filter(Boolean).join(' · ') || 'Artist')}</small></span>
          <span class="artistArrow">→</span>
        </button>`).join('')}</div>`;
    }
  } catch(err) {
    $('#artistResults').innerHTML = `<div class="loadingCard">Something went wrong with artist search. ${esc(err.message)}</div>`;
  } finally {$('#artistSearchButton').disabled = false;}
}
$('#artistSearchForm').addEventListener('submit', ev => {ev.preventDefault();searchArtists($('#artistSearch').value);});
$$('[data-artist]').forEach(button => button.addEventListener('click', () => searchArtists(button.dataset.artist)));
$('#artistResults').addEventListener('click', event => {
  const button = event.target.closest('[data-artist-index]');
  if (button) selectArtist(Number(button.dataset.artistIndex));
});
async function selectArtist(index) {
  const artist = ui.artists[index];
  if (!artist) return;
  ui.artist = artist;
  ui.selected.clear();
  updateSelectionBar();
  $('#artistResults').hidden = true;
  $('#albumResults').hidden = false;
  $('#albumResults').innerHTML = loadingBlock(`Gathering ${artist.name}'s albums…`);
  try {
    const albums = await getJSON('/api/albums?artistId=' + encodeURIComponent(artist.id));
    // Ignore a late response if a different artist is selected.
    if (ui.artist?.id !== artist.id) return;
    ui.albums = albums;
    renderAlbums();
  } catch (err) {
    $('#albumResults').innerHTML = `<div class="resultsHeader"><h2>${esc(artist.name)}</h2><button class="secondaryButton" id="backToArtists">← Artists</button></div><div class="loadingCard">Album lookup failed: ${esc(err.message)}</div>`;
    $('#backToArtists').addEventListener('click', showArtists);
  }
}
function showArtists() {
  $('#albumResults').hidden = true;
  $('#artistResults').hidden = false;
  ui.selected.clear();
  updateSelectionBar();
}
function renderAlbums() {
  const artist = ui.artist;
  if (!artist) return;
  $('#albumResults').innerHTML = `<div class="resultsHeader"><div><h2>${esc(artist.name)} <span style="color:#756b80;font-size:15px;font-weight:600">· ${ui.albums.length} albums</span></h2><p>Select one or more albums.</p></div><button class="secondaryButton" id="backToArtists">← Other artists</button></div>
  <div class="discographyToolbar"><span>Studio albums by release date</span><button class="linkButton" id="selectAllAlbums">Select all</button><button class="linkButton" id="deselectAllAlbums">Clear selection</button></div>
  ${ui.albums.length ? `<div class="albumGrid">${ui.albums.map((album, i) => `<button class="albumCard ${ui.selected.has(album.id) ? 'selected' : ''}" data-album-index="${i}" aria-pressed="${ui.selected.has(album.id)}">
    <span class="albumCover"><img src="${esc(album.coverUrl)}" loading="lazy" alt=""></span><span class="albumTick">✓</span>
    <span class="albumName">${esc(album.title)}</span><span class="albumYear">${esc(album.date?.slice(0, 4) || 'Year unknown')}</span></button>`).join('')}</div>` : `<div class="loadingCard">No regular studio albums were found for this artist. Try another artist or use Paste links.</div>`}`;
  $('#backToArtists').addEventListener('click', showArtists);
  $('#selectAllAlbums').addEventListener('click', () => {ui.albums.forEach(a => ui.selected.add(a.id));renderAlbums();updateSelectionBar();});
  $('#deselectAllAlbums').addEventListener('click', () => {ui.selected.clear();renderAlbums();updateSelectionBar();});
}
$('#albumResults').addEventListener('click', event => {
  const button = event.target.closest('[data-album-index]');
  if (!button) return;
  const album = ui.albums[Number(button.dataset.albumIndex)];
  if (!album) return;
  if (ui.selected.has(album.id)) ui.selected.delete(album.id);
  else ui.selected.add(album.id);
  button.classList.toggle('selected', ui.selected.has(album.id));
  button.setAttribute('aria-pressed', String(ui.selected.has(album.id)));
  updateSelectionBar();
});
$('#albumResults').addEventListener('error', event => {
  if (event.target.tagName === 'IMG') event.target.remove();
}, true);
function updateSelectionBar() {
  $('#selectionBar').hidden = ui.tab !== 'discover' || ui.selected.size === 0;
  $('#selectedCount').textContent = `${ui.selected.size} album${ui.selected.size === 1 ? '' : 's'} selected`;
  $('#selectedArtist').textContent = ui.artist?.name || 'Ready to add';
}
$('#clearSelected').addEventListener('click', () => {ui.selected.clear();$$('.albumCard').forEach(b => {b.classList.remove('selected');b.setAttribute('aria-pressed','false');});updateSelectionBar();});
$('#addSelected').addEventListener('click', async () => {
  if (!ui.artist || !ui.selected.size) return;
  const albums = ui.albums.filter(a => ui.selected.has(a.id)).map(({id, title, date}) => ({id, title, date}));
  $('#addSelected').disabled = true;
  try {
    const result = await postJSON('/api/queue/albums', {artist: ui.artist.name, albums});
    if (result.added === 0) toast('Those albums are already in your queue.');
    else toast(`Added ${result.added} album${result.added===1?'':'s'} to the queue.`);
    ui.selected.clear();
    await refreshState();
    switchTab('queue');
  } catch(err) {toast(`Could not queue albums: ${err.message}`, true);}
  finally {$('#addSelected').disabled = false;updateSelectionBar();}
});
$('#linksInput').addEventListener('input', () => {
  const count = $('#linksInput').value.split(/\r?\n/).filter(s => s.trim()).length;
  $('#linkCount').textContent = count ? `${count} link${count===1?'':'s'} ready to add` : 'Add as many as you like, one per line';
});
$('#linksAddButton').addEventListener('click', async () => {
  const text = $('#linksInput').value.trim();
  if (!text) {toast('Paste one or more links first.', true);return;}
  $('#linksAddButton').disabled = true;
  try {
    const result = await postJSON('/api/queue/urls', {text});
    toast(result.added ? `Added ${result.added} link${result.added===1?'':'s'} to your queue.` : 'These links are already in your queue.');
    $('#linksInput').value = '';
    $('#linkCount').textContent = 'Add as many as you like, one per line';
    await refreshState();
    switchTab('queue');
  } catch(err) {toast(err.message, true);}
  finally {$('#linksAddButton').disabled = false;}
});
function statusBadge(status) {
  return `<span class="jobStatus ${esc(status)}">${esc(labelForStatus[status] || status)}</span>`;
}
function progressHtml(p) {return `<div class="jobBar"><div style="width:${Math.max(0, Math.min(100, Number(p) || 0))}%"></div></div>`;}
function renderSetup(setup, update) {
  if (!setup) return;
  const ready = setup.state === 'ready';
  $('#setupBanner').hidden = ready;
  $('#setupBanner').classList.toggle('error', setup.state === 'error');
  $('#setupTitle').textContent = setup.state === 'error' ? 'Setup failed' : 'Preparing audio tools';
  $('#setupMessage').textContent = setup.message || '';
  $('#setupProgressFill').style.width = `${Math.max(0, Math.min(100, setup.percent || 0))}%`;
  $('#setupIcon').textContent = setup.state === 'error' ? '!' : '↓';
  $('#setupRetry').hidden = setup.state !== 'error';
  $('#topStatus').textContent = ready ? 'Ready' : setup.state === 'error' ? 'Setup needs attention' : 'First-time setup';
  $('#toolStatus').textContent = !ready ? (setup.state === 'error' ? 'Setup failed' : 'Setting up tools')
    : update?.state === 'checking' ? 'Checking downloader updates…'
    : update?.state === 'error' ? 'Update check failed; current downloader still available'
    : update?.message || '✓ All tools ready';
  $('#refreshTools').disabled = !ready || update?.state === 'checking';
}
$('#setupRetry').addEventListener('click', async () => {
  try {await postJSON('/api/tools/retry');toast('Checking audio tools.');await refreshState();}
  catch(err) {toast(err.message, true);}
});
async function refreshTools() {
  try {await postJSON('/api/tools/update');toast('Checking for downloader updates.');await refreshState();}
  catch(err) {toast(err.message, true);}
}
$('#refreshTools').addEventListener('click', refreshTools);
// Check GitHub Releases automatically in the backend. Only show a small
// update notice when a verified, newer ZIP is available. No modal pop-ups.
function renderAppUpdate(update, state) {
  if (!update) return;
  const label = update.latest || '';
  const installing = update.state === 'downloading' || update.state === 'installing';
  const offered = !!update.canInstall;
  const queueActive = (state.jobs || []).some(j =>
    runningStatuses.has(j.status) || (j.status === 'queued' && !state.paused));
  const busy = queueActive || state.setup?.state !== 'ready' || installing;
  const banner = $('#appUpdateBanner');
  banner.hidden = !(installing || (offered && ui.dismissedAppRelease !== label));
  $('#appUpdateTitle').textContent = installing ? 'Updating Mixtape' : `Mixtape ${label} available`;
  $('#appUpdateMessage').textContent = installing ? (update.message || 'Preparing update…')
    : queueActive ? 'The update can be installed after the current queue finishes.'
    : 'Download the GitHub release, replace Mixtape, and restart. Your music and queue stay as they are.';
  $('#applyAppUpdate').hidden = installing;
  $('#dismissAppUpdate').hidden = installing;
  $('#applyAppUpdate').disabled = busy || !offered;
  $('#applyAppUpdateSettings').hidden = !offered || installing;
  $('#applyAppUpdateSettings').disabled = busy;
  $('#checkAppUpdate').disabled = update.state === 'checking' || installing;
  $('#appUpdateSettingsVersion').textContent = `Installed: ${update.current || state.version || 'unknown'}`;
  $('#appUpdateSettingsStatus').textContent = update.installError ? `Previous update failed: ${update.installError}. ${update.message || ''}` : (update.message || 'No update information yet');
}
async function checkAppUpdate() {
  try { await postJSON('/api/app-update/check'); toast('Checking GitHub Releases.'); await refreshState(); }
  catch (err) { toast(err.message, true); }
}
async function applyAppUpdate() {
  try { await postJSON('/api/app-update/apply'); toast('Downloading the update. Mixtape will restart once it is ready.'); await refreshState(); }
  catch (err) { toast(err.message, true); }
}
$('#checkAppUpdate').addEventListener('click', checkAppUpdate);
$('#applyAppUpdate').addEventListener('click', applyAppUpdate);
$('#applyAppUpdateSettings').addEventListener('click', applyAppUpdate);
$('#dismissAppUpdate').addEventListener('click', () => {
  ui.dismissedAppRelease = ui.queueState?.appUpdate?.latest || null;
  $('#appUpdateBanner').hidden = true;
});
function errorHTML(job) {
  if (!job.error) return '';
  const detail = (job.errorDetail || '').trim();
  const canExpand = detail && detail !== job.error;
  return `<div class="errorNotice">
    <div class="errorHeader"><span class="errorText">${esc(job.error)}</span>
      <button class="tinyButton copyErrorButton" type="button" data-job-act="copy-error" data-id="${esc(job.id)}" aria-label="Copy error for ${esc(job.title)}">Copy error</button>
    </div>
    ${canExpand ? `<details class="errorDetails" data-id="${esc(job.id)}" ${ui.expandedErrors.has(job.id) ? 'open' : ''}>
      <summary>Technical details</summary><pre>${esc(detail)}</pre>
    </details>` : ''}
  </div>`;
}
function errorReport(job) {
  const lines = [`Mixtape ${ui.queueState?.version || ''}`.trim(),
    `Item: ${[job.artist, job.album, job.title].filter(Boolean).join(' / ') || job.url || job.id}`,
    `Type: ${job.kind}`, `Status: ${job.status}`, `Error: ${job.error || 'Unknown error'}`];
  if (job.url) lines.push(`URL: ${job.url}`);
  if (job.errorDetail && job.errorDetail.trim() !== job.error) {
    lines.push('', 'Technical details:', job.errorDetail.trim());
  }
  return lines.join('\n');
}
async function copyText(text) {
  if (navigator.clipboard?.writeText) {
    try { await navigator.clipboard.writeText(text); return; }
    catch (_) { /* Fall back to selecting a temporary text box. */ }
  }
  const input = document.createElement('textarea');
  input.value = text;
  input.setAttribute('readonly', '');
  input.style.cssText = 'position:fixed;left:-9999px;top:0;opacity:0';
  document.body.appendChild(input);
  input.select();
  try {
    if (!document.execCommand('copy')) throw new Error('Clipboard access is blocked by your browser');
  } finally { input.remove(); }
}
function jobActionHTML(job, allowRemove = true) {
  let s = '';
  if (job.status === 'done' && job.output) s += `<button class="tinyButton openLocationButton" data-job-act="open" data-id="${esc(job.id)}">↗ ${job.kind === 'url' && !job.url?.includes('/playlist?') ? 'Show file' : 'Open folder'}</button>`;
  if (job.status === 'failed') s += `<button class="tinyButton" data-job-act="retry" data-id="${esc(job.id)}">↻ Try again</button>`;
  if (allowRemove) s += `<button class="tinyButton danger" data-job-act="remove" data-id="${esc(job.id)}">✕ Remove</button>`;
  return s;
}
function reviewHTML(job) {
  if (job.status !== 'review') return '';
  const candidates = (job.candidates || []).slice(0, 4);
  const choices = candidates.map(c => `<div class="reviewChoice"><span>${esc(c.title)}<small>${esc(c.channel || 'Unknown channel')} · ${esc(c.duration ? `${Math.floor(c.duration/60)}:${String(c.duration%60).padStart(2,'0')}` : 'Length unknown')} · ${esc(c.score)}% match</small></span><button class="tinyButton" data-job-act="choose" data-id="${esc(job.id)}" data-url="${esc(c.url)}">Use this</button></div>`).join('');
  return `<div class="reviewBox"><p>We couldn't confirm the right recording. Choose one or paste a YouTube link:</p>${choices || '<p>No matches appeared. Paste the song’s link below.</p>'}<div class="manualReview"><input class="manualUrl" data-id="${esc(job.id)}" placeholder="https://music.youtube.com/watch?v=…" aria-label="Choose another YouTube URL"><button class="tinyButton" data-job-act="manual" data-id="${esc(job.id)}">Use link</button></div></div>`;
}
function trackHTML(job) {
  const saved = job.status === 'done';
  return `<div class="trackRow ${job.status === 'skipped' ? 'trackSkipped' : ''}">
    <label class="trackSelect" title="${saved ? 'Already downloaded; unchecking will not delete saved files' : 'Include this song in downloads'}">
      <input type="checkbox" data-track-toggle data-id="${esc(job.id)}" aria-label="Download ${esc(job.title)}"
        ${job.status !== 'skipped' ? 'checked' : ''} ${saved ? 'disabled' : ''}>
      <span>${saved ? 'Saved' : 'Include'}</span>
    </label>
    <span class="trackNo">${esc(job.number || '♪')}</span><div class="trackText"><strong>${esc(job.title)}</strong><small>${esc(job.message || '')}</small></div>
    ${runningStatuses.has(job.status) ? `<div class="trackProgress"><div style="width:${Math.min(100,job.progress||0)}%"></div></div>` : ''}
    ${statusBadge(job.status)}
    ${job.status === 'done' && job.output ? `<button class="tinyButton openLocationButton" data-job-act="open" data-id="${esc(job.id)}" aria-label="Show ${esc(job.title)} in its folder">↗ Show file</button>` : ''}
    ${job.status === 'failed' ? `<button class="tinyButton" data-job-act="retry" data-id="${esc(job.id)}">Retry</button>` : ''}
    ${job.status === 'review' ? reviewHTML(job) : ''}
    ${errorHTML(job)}
  </div>`;
}
function albumHTML(album, children) {
  const done = children.filter(j => j.status === 'done').length;
  const selected = children.filter(j => j.status !== 'skipped');
  const skipped = children.length - selected.length;
  const needsReview = children.some(j => j.status === 'review');
  const working = children.some(j => runningStatuses.has(j.status));
  const failed = children.some(j => j.status === 'failed');
  const groupStatus = album.status === 'failed' ? 'failed' : needsReview ? 'review' : working ? 'downloading' : failed ? 'failed' :
    children.length && !selected.length ? 'skipped' : selected.length && done === selected.length ? 'done' : album.status === 'done' ? 'queued' : album.status;
  const completed = selected.length ? Math.round(done * 100 / selected.length) : children.length ? 0 : album.progress || 0;
  // Newly queued albums open automatically so individual tracks are easy to exclude.
  const open = ui.expanded.has(album.id) || (!ui.collapsed.has(album.id) && (needsReview || failed || selected.some(j => activeStatuses.has(j.status))));
  return `<div class="queueAlbumGroup"><div class="albumGroupHeader">
    <div class="albumGroupCover">♫<img src="https://coverartarchive.org/release-group/${esc(album.albumId)}/front-250" alt="" loading="lazy"></div>
    <div class="albumGroupInfo"><h3>${esc(album.album)}</h3><p>${esc(album.artist)} · ${children.length ? `${done} of ${selected.length} selected songs ready${skipped ? ` · ${skipped} skipped` : ''}` : album.message || 'Loading tracklist…'}</p></div>
    ${statusBadge(groupStatus)}
    ${children.some(j => j.status === 'done' && j.output) ? `<button class="tinyButton openLocationButton" data-job-act="open" data-id="${esc(album.id)}">↗ Open folder</button>` : ''}
    <button class="groupToggle" data-job-act="expand" data-id="${esc(album.id)}">${open ? '− Hide' : '+ Songs'}</button>
    ${album.status === 'failed' ? `<button class="tinyButton" data-job-act="retry" data-id="${esc(album.id)}">Retry</button>` : ''}
    <button class="tinyButton danger" data-job-act="remove" data-id="${esc(album.id)}">✕</button>
  </div>${progressHtml(completed)}
  ${errorHTML(album)}
  ${open && children.length ? `<div class="albumGroupBody">${children.map(trackHTML).join('')}</div>` : ''}
  </div>`;
}
function linkHTML(j) {
  return `<div class="queueCard"><div class="queueCardHeader"><div class="queueArt">↗</div><div class="queueInfo"><h3>${esc(j.title === 'YouTube link' ? 'From a YouTube link' : j.title)}</h3><p>${esc(j.url || '')}</p></div>${statusBadge(j.status)}</div>
  ${progressHtml(j.progress || 0)}<div class="jobMessage">${esc(j.message || '')}</div>
  ${errorHTML(j)}
  <div class="jobActions">${jobActionHTML(j)}</div></div>`;
}
function shouldShow(filter, album, children) {
  if (filter === 'all') return true;
  const jobs = [album, ...children];
  if (filter === 'working') return jobs.some(j => activeStatuses.has(j.status));
  if (filter === 'review') return jobs.some(j => j.status === 'review' || j.status === 'failed');
  if (filter === 'done') return children.length ? children.some(j => j.status === 'done') && children.every(j => j.status === 'done' || j.status === 'skipped') : album.status === 'done';
  return true;
}
function renderQueue() {
  const s = ui.queueState;
  if (!s) return;
  const jobs = s.jobs || [];
  const songs = jobs.filter(j => j.kind === 'track' || j.kind === 'url');
  const completed = songs.filter(j => j.status === 'done').length;
  const pending = jobs.filter(j => activeStatuses.has(j.status)).length;
  const review = jobs.filter(j => j.status === 'review' || j.status === 'failed').length;
  const failed = jobs.filter(j => j.status === 'failed').length;
  $('#statComplete').textContent = completed;
  $('#statPending').textContent = pending;
  $('#statReview').textContent = review;
  const workCount = pending + review;
  $('#navQueueCount').hidden = workCount === 0;
  $('#navQueueCount').textContent = workCount;
  const pauseButton = $('#pauseQueue');
  pauseButton.textContent = s.paused ? '▶ Resume queue' : 'Ⅱ Pause queue';
  pauseButton.disabled = ui.pauseBusy;
  pauseButton.setAttribute('aria-pressed', String(!!s.paused));
  pauseButton.classList.toggle('paused', !!s.paused);
  const pauseNote = $('#queuePauseNote');
  const activeDownload = jobs.some(j => runningStatuses.has(j.status));
  pauseNote.hidden = !s.paused;
  pauseNote.textContent = s.paused ? (activeDownload
    ? 'Queue paused — the current item will finish. No new downloads will start.'
    : 'Queue paused — pending downloads will start when you resume.') : '';
  // Display exactly the same state as the tray icon, calculated by Go.
  if (s.queueStatus) $('#topStatus').textContent = s.queueStatus;
  $('#retryFailed').hidden = failed === 0;
  $('#retryFailed').textContent = `↻ Retry failed (${failed})`;
  $('#appVersion').textContent = s.version || '1.0';
  if (ui.tab !== 'queue') return;
  // Don't destroy an input while someone is typing a missing-track URL.
  if (document.activeElement?.classList.contains('manualUrl')) return;
  const result = [];
  jobs.filter(j => j.kind === 'album').forEach(a => {
    const children = jobs.filter(j => j.parentId === a.id);
    if (shouldShow(ui.queueFilter, a, children)) result.push(albumHTML(a, children));
  });
  jobs.filter(j => j.kind === 'url').forEach(j => {if (shouldShow(ui.queueFilter, j, [])) result.push(linkHTML(j));});
  // Also display any track orphaned by an older queue or unexpected interruption.
  jobs.filter(j => j.kind === 'track' && !jobs.some(a => a.id === j.parentId)).forEach(j => {
    if (shouldShow(ui.queueFilter, j, [])) result.push(`<div class="queueCard">${trackHTML(j)}</div>`);
  });
  $('#queueList').innerHTML = result.join('');
  $('#queueEmpty').hidden = result.length > 0;
  if (!result.length) {
    $('#queueEmpty').querySelector('h3').textContent = jobs.length ? 'No matching jobs' : 'No downloads yet';
    $('#queueEmpty').querySelector('p').textContent = jobs.length ? 'Try another filter.' : 'Find an artist or add links to start.';
  }
}
$('#queueList').addEventListener('click', event => {
  const summary = event.target.closest('.errorDetails > summary');
  if (!summary) return;
  const details = summary.parentElement;
  const id = details.dataset.id;
  if (details.open) ui.expandedErrors.delete(id);
  else ui.expandedErrors.add(id);
});
$('#queueList').addEventListener('error', event => {
  if (event.target.tagName === 'IMG') event.target.remove();
}, true);
$('#queueList').addEventListener('change', async event => {
  const checkbox = event.target.closest('input[data-track-toggle]');
  if (!checkbox) return;
  const enabled = checkbox.checked;
  checkbox.disabled = true;
  try {
    await postJSON('/api/job/select', {id: checkbox.dataset.id, enabled});
    await refreshState();
  } catch (err) {
    checkbox.checked = !enabled;
    toast(`Couldn't update song: ${err.message}`, true);
  } finally {checkbox.disabled = false;}
});
$('#queueList').addEventListener('click', async event => {
  const button = event.target.closest('[data-job-act]');
  if (!button) return;
  const {jobAct: action, id} = button.dataset;
  if (action === 'expand') {
    const buttonOpen = button.textContent.includes('Hide');
    if (buttonOpen) { ui.expanded.delete(id); ui.collapsed.add(id); }
    else { ui.collapsed.delete(id); ui.expanded.add(id); }
    renderQueue(); return;
  }
  button.disabled = true;
  try {
    if (action === 'remove') {
      const j = ui.queueState.jobs.find(v => v.id === id);
      if (j?.kind === 'album' && !confirm('Remove this album and all its song jobs from the queue? Downloaded files will stay on disk.')) return;
      await postJSON('/api/job/remove', {id});toast('Removed from queue.');
    } else if (action === 'open') {
      await postJSON('/api/job/open', {id});
    } else if (action === 'copy-error') {
      const job = ui.queueState?.jobs?.find(j => j.id === id);
      if (!job || !job.error) throw new Error('This error is no longer available.');
      await copyText(errorReport(job));
      toast('Error copied to clipboard.');
      return;
    } else if (action === 'retry') {
      await postJSON('/api/job/retry', {id});toast('Added back to the queue.');
    } else if (action === 'choose') {
      await postJSON('/api/job/review', {id, url: button.dataset.url});toast('Song match chosen!');
    } else if (action === 'manual') {
      const input = Array.from($$('.manualUrl')).find(input => input.dataset.id === id);
      const url = input?.value.trim();
      if (!url) {toast('Paste a video link first.', true);return;}
      await postJSON('/api/job/review', {id, url});toast('Song match chosen!');
    }
    await refreshState();
  } catch(err) {toast(err.message, true);}
  finally {button.disabled = false;}
});
$$('[data-filter]').forEach(b => b.addEventListener('click', () => {
  ui.queueFilter = b.dataset.filter;
  $$('[data-filter]').forEach(x => x.classList.toggle('active', x === b));
  renderQueue();
}));
$('#pauseQueue').addEventListener('click', async () => {
  if (!ui.queueState || ui.pauseBusy) return;
  ui.pauseBusy = true;
  $('#pauseQueue').disabled = true;
  try {
    const paused = !ui.queueState.paused;
    await postJSON('/api/queue/pause', {paused});
    toast(paused ? 'Queue paused. The current item will finish.' : 'Queue resumed.');
    await refreshState();
  } catch(err) {toast(err.message, true);}
  finally { ui.pauseBusy = false; $('#pauseQueue').disabled = false; }
});
$('#retryFailed').addEventListener('click', async () => {
  const button = $('#retryFailed');
  button.disabled = true;
  try {
    const result = await postJSON('/api/queue/retry-failed');
    toast(result.retried ? `Retrying ${result.retried} failed item${result.retried===1?'':'s'}.` : 'No failed items to retry.');
    await refreshState();
  } catch(err) {toast(err.message, true);}
  finally {button.disabled = false;}
});
$('#clearFinished').addEventListener('click', async () => {
  if (!confirm('Clear completed and failed queue entries? Downloaded MP3 files will not be deleted.')) return;
  try {await postJSON('/api/queue/clear');await refreshState();toast('Queue history cleared.');}
  catch(err) {toast(err.message, true);}
});
$('#emptyGoDiscover').addEventListener('click', () => switchTab('discover'));
function fillSettings() {
  const s = ui.queueState?.settings;
  if (!s) return;
  if (document.activeElement !== $('#outputDir')) $('#outputDir').value = s.outputDir || '';
  $('#keepAwake').checked = !!s.keepAwake;
}
$('#browseOutputDir').addEventListener('click', async () => {
  const button = $('#browseOutputDir');
  button.disabled = true;
  try {
    const chosen = await postJSON('/api/settings/browse', {initialDir: $('#outputDir').value});
    if (!chosen.cancelled && chosen.path) {
      $('#outputDir').value = chosen.path;
      toast('Folder selected. Click Save settings to use it.');
    }
  } catch (err) {toast(`Couldn't choose a folder: ${err.message}`, true);}
  finally {button.disabled = false;}
});
$('#settingsForm').addEventListener('submit', async ev => {
  ev.preventDefault();
  try {
    await postJSON('/api/settings',{outputDir: $('#outputDir').value,keepAwake: $('#keepAwake').checked});
    toast('Settings saved.');await refreshState();
  } catch(err) {toast(err.message, true);}
});
// The page listens for the backend to shut down, regardless of which menu
// initiated Quit. Firefox and other browsers usually refuse window.close()
// on ordinary tabs opened by an external program; show a tidy fallback.
let statePollTimer;
let lifecycleEvents;
function closeMixtapePage() {
  if (ui.closed) return;
  ui.closed = true;
  if (statePollTimer) clearInterval(statePollTimer);
  if (lifecycleEvents) lifecycleEvents.close();
  document.title = 'Mixtape — closed';
  try { window.close(); } catch (_) { /* Browser may block closing a normal tab. */ }
  // If the browser does not permit script-based closing, replace the app
  // with a small static page. There is no further connection to the backend.
  document.body.innerHTML = `<main class="exitScreen" role="status">
    <h1>Mixtape is closed</h1>
    <p>You can close this tab. Open Mixtape.exe to start it again.</p>
  </main>`;
}
function connectLifecycle() {
  if (ui.closed || lifecycleEvents || typeof EventSource === 'undefined') return;
  lifecycleEvents = new EventSource('/api/lifecycle');
  lifecycleEvents.addEventListener('shutdown', closeMixtapePage);
}
connectLifecycle();
// sendBeacon tells the backend immediately when this particular tab closes.
// Do not depend on the EventSource socket disconnecting: Firefox can leave
// that connection alive after the tab has gone. A lease timeout is a backup
// for browser crashes where pagehide cannot run.
let pageLeaveSent = false;
function leaveMixtapePage() {
  if (pageLeaveSent || !ui.sessionID || !ui.seenBackend) return;
  pageLeaveSent = true;
  const data = JSON.stringify({pageID: ui.pageID, sessionID: ui.sessionID});
  const payload = new Blob([data], {type: 'application/json'});
  // Firefox can drop either of these during tab teardown. Both are safe:
  // the backend treats a repeated close notification as success.
  try { navigator.sendBeacon?.('/api/page/leave', payload); } catch (_) { /* Best effort. */ }
  fetch('/api/page/leave', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: data,
    keepalive: true}).catch(() => {});
}
// Firefox sometimes misses pagehide during a normal tab close. Send the
// same idempotent beacon from beforeunload as a second opportunity.
window.addEventListener('beforeunload', leaveMixtapePage);
// Reconnect if Firefox restores this page from the back/forward cache.
window.addEventListener('pagehide', () => {
  leaveMixtapePage();
  if (lifecycleEvents) {
    lifecycleEvents.close();
    lifecycleEvents = null;
  }
});
window.addEventListener('pageshow', event => {
  if (event.persisted && !ui.closed) {
    pageLeaveSent = false;
    // A page revived from the back/forward cache is a new visit, not the
    // tab that previously sent its leave beacon.
    ui.pageID = globalThis.crypto?.randomUUID?.() ||
      `${Date.now().toString(16)}-${Math.random().toString(16).slice(2)}-${Math.random().toString(16).slice(2)}`;
    connectLifecycle();
    refreshState();
  }
});
$('#quitApp').addEventListener('click', async () => {
  if (!confirm('Quit Mixtape? Any running download will stop, and resume when you reopen the app.')) return;
  try {await postJSON('/api/quit');closeMixtapePage();}
  catch(err) {toast(err.message, true);}
});
async function refreshState() {
  if (ui.closed) return;
  try {
    const s = await getJSON('/api/state');
    if (ui.closed) return;
    // The old Mixtape has gone away if another EXE took over this port.
    if (ui.sessionID && s.sessionID !== ui.sessionID) {
      closeMixtapePage();
      return;
    }
    ui.sessionID = s.sessionID;
    ui.seenBackend = true;
    ui.failedPolls = 0;
    ui.queueState = s;
    renderSetup(s.setup, s.toolUpdate);
    renderQueue();
    renderAppUpdate(s.appUpdate, s);
    if (ui.tab === 'settings') fillSettings();
  } catch(err) {
    if (ui.closed) return;
    if (ui.seenBackend && ++ui.failedPolls >= 4) {
      // Fallback for crashes and browsers without EventSource. Allow a few
      // consecutive failures so a transient connection error doesn't close UI.
      closeMixtapePage();
      return;
    }
    $('#topStatus').textContent = 'Trying to reconnect…';
  }
}
refreshState();
statePollTimer = setInterval(refreshState, 1900);
