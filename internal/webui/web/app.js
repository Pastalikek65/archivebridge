'use strict';
const $ = id => document.getElementById(id);
let archive, albumId = null, repeatedOnly = false, shown = 80, verification;
const contentCounts = new Map();
function sourceName(file) { return archive.sources[file.sourceIndex]?.name || 'Unknown source part'; }
function element(tag, className, text) { const node = document.createElement(tag); if (className) node.className = className; if (text !== undefined) node.textContent = text; return node; }
function setStatus(text, failure = false) { $('status').textContent = text; $('status').classList.toggle('failure', failure); }
function render() {
  if (!archive) return;
  const query = $('search').value.toLocaleLowerCase(), state = $('metadata').value;
  const selected = archive.files.filter(file => (!albumId || file.albumIds?.includes(albumId)) && (!repeatedOnly || contentCounts.get(file.sha256) > 1) && file.entryPath.toLocaleLowerCase().includes(query) && (state === 'all' || (state === 'matched' ? file.metadataStatus === 'matched' : file.metadataStatus !== 'matched')));
  $('visible-count').textContent = `${selected.length.toLocaleString()} occurrences`;
  $('grid').replaceChildren();
  for (const file of selected.slice(0, shown)) {
    const card = element('article', 'card'), preview = element('div', 'preview');
    if (['image/jpeg', 'image/png'].includes(file.mime)) { const img = element('img'); img.src = `/api/media/${encodeURIComponent(file.id)}`; img.alt = file.entryPath.split('/').at(-1); img.loading = 'lazy'; img.addEventListener('error', () => { img.remove(); preview.append(element('span', 'file-icon', 'Preview unavailable')); }); preview.append(img); }
    else preview.append(element('span', 'file-icon', file.mime?.startsWith('video/') ? 'VIDEO' : 'ORIGINAL'));
    card.append(preview);
    const name = element('h3', '', file.entryPath.split('/').at(-1)); name.title = file.entryPath; card.append(name);
    const date = element('p', 'date', file.date ? file.date.slice(0, 10) : 'Date unresolved'); date.title = file.date ? `${file.date} (UTC)` : 'No reliable date was matched'; card.append(date);
    const source = element('p', 'source', sourceName(file)); source.title = `${sourceName(file)} · ${file.entryPath}`; card.append(source);
    if (contentCounts.get(file.sha256) > 1) card.append(element('p', 'repeat', `${contentCounts.get(file.sha256)} occurrences share these bytes`));
    card.append(element('span', file.metadataStatus === 'matched' ? 'badge matched' : 'badge', file.metadataStatus === 'matched' ? 'Metadata matched' : `Metadata: ${file.metadataStatus}`));
    const link = element('a', 'download', 'Download original'); link.href = `/api/media/${encodeURIComponent(file.id)}?download=1`; card.append(link); $('grid').append(card);
  }
  if (!selected.length) $('grid').append(element('p', 'empty', 'No media matches these filters.'));
  $('more').hidden = selected.length <= shown;
}
function selectAlbum(id, title, button) { albumId = id; repeatedOnly = button.id === 'repeated'; shown = 80; $('heading').textContent = title; $('repeat-note').hidden = !repeatedOnly; document.querySelectorAll('.nav').forEach(node => { node.classList.remove('active'); node.setAttribute('aria-pressed', 'false'); }); button.classList.add('active'); button.setAttribute('aria-pressed', 'true'); render(); }
$('all').addEventListener('click', () => selectAlbum(null, 'All media', $('all')));
$('repeated').addEventListener('click', () => selectAlbum(null, 'Repeated content', $('repeated')));
$('search').addEventListener('input', () => { shown = 80; render(); });
$('metadata').addEventListener('change', () => { shown = 80; render(); });
$('more').addEventListener('click', () => { shown += 80; render(); });
$('verify').addEventListener('click', async () => {
  verification = new AbortController(); $('verify').disabled = true; $('cancel-verify').hidden = false; setStatus('Checking stored originals and sidecars…');
  try { const response = await fetch('/api/verify', { cache: 'no-store', signal: verification.signal }); const report = await response.json(); if (!response.ok || report.status !== 'ok') throw new Error('Verification did not pass. Run archivebridge verify for details.'); setStatus(`Verified ${report.filesChecked} media and ${report.sidecarsChecked} sidecar references against this manifest.`); }
  catch (error) { setStatus(error.name === 'AbortError' ? 'Verification cancelled. No integrity result was recorded.' : error.message, error.name !== 'AbortError'); }
  finally { verification = undefined; $('verify').disabled = false; $('cancel-verify').hidden = true; }
});
$('cancel-verify').addEventListener('click', () => verification?.abort());
(async () => {
  try {
    const response = await fetch('/api/manifest', { cache: 'no-store' }); if (!response.ok) throw new Error('The archive manifest could not be opened.'); archive = await response.json();
    for (const field of ['files', 'sidecars', 'albums', 'sources', 'issues']) { if (archive[field] === null) archive[field] = []; if (!Array.isArray(archive[field])) throw new Error('The archive manifest has an unsupported collection.'); }
    $('total').textContent = archive.files.length.toLocaleString(); $('media-count').textContent = archive.files.length.toLocaleString(); $('album-count').textContent = archive.albums.length.toLocaleString(); $('source-count').textContent = archive.sources.length.toLocaleString(); $('issue-count').textContent = archive.issues.length.toLocaleString();
    for (const file of archive.files) contentCounts.set(file.sha256, (contentCounts.get(file.sha256) || 0) + 1);
    $('repeated-count').textContent = archive.files.filter(file => contentCounts.get(file.sha256) > 1).length.toLocaleString();
    const mediaByID = new Map(archive.files.map(file => [file.id, file]));
    for (const album of archive.albums) {
      const button = element('button', 'nav album'); button.setAttribute('aria-pressed', 'false');
      const label = element('span', 'album-label', album.title);
      const member = mediaByID.get(album.mediaIds[0]);
      const provenance = member ? sourceName(member) : 'Source part not identified';
      label.append(element('small', 'album-source', provenance)); button.append(label, element('span', 'album-count', album.mediaIds.length.toLocaleString()));
      button.title = `${album.title} · ${provenance} · ${album.folder}`;
      button.addEventListener('click', () => selectAlbum(album.id, album.title, button)); $('albums').append(button);
    }
    $('issues-label').textContent = `Transfer notes (${archive.issues.length})`;
    for (const issue of archive.issues) { const item = element('div', 'issue'); item.append(element('strong', '', issue.code), element('p', '', issue.details), element('code', '', issue.entryPath || 'Source part')); $('issues').append(item); }
    setStatus('Manifest loaded. Use Verify archive to check stored bytes before relying on their integrity.'); render();
  } catch (error) { setStatus(error.message, true); }
})();
