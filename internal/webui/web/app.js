'use strict';
const $ = id => document.getElementById(id);
let archive, albumId = null, shown = 80;
function element(tag, className, text) { const node = document.createElement(tag); if (className) node.className = className; if (text !== undefined) node.textContent = text; return node; }
function setStatus(text, failure = false) { $('status').textContent = text; $('status').classList.toggle('failure', failure); }
function render() {
  if (!archive) return;
  const query = $('search').value.toLocaleLowerCase(), state = $('metadata').value;
  const selected = archive.files.filter(file => (!albumId || file.albumIds?.includes(albumId)) && file.entryPath.toLocaleLowerCase().includes(query) && (state === 'all' || (state === 'matched' ? file.metadataStatus === 'matched' : file.metadataStatus !== 'matched')));
  $('visible-count').textContent = `${selected.length.toLocaleString()} occurrences`;
  $('grid').replaceChildren();
  for (const file of selected.slice(0, shown)) {
    const card = element('article', 'card'), preview = element('div', 'preview');
    if (['image/jpeg', 'image/png'].includes(file.mime)) { const img = element('img'); img.src = `/api/media/${encodeURIComponent(file.id)}`; img.alt = file.entryPath.split('/').at(-1); img.loading = 'lazy'; img.addEventListener('error', () => { img.remove(); preview.append(element('span', 'file-icon', 'Preview unavailable')); }); preview.append(img); }
    else preview.append(element('span', 'file-icon', file.mime?.startsWith('video/') ? 'VIDEO' : 'ORIGINAL'));
    card.append(preview);
    const name = element('h3', '', file.entryPath.split('/').at(-1)); name.title = file.entryPath; card.append(name);
    card.append(element('p', 'date', file.date ? file.date.slice(0, 10) : 'Date unresolved'));
    card.append(element('span', file.metadataStatus === 'matched' ? 'badge matched' : 'badge', file.metadataStatus === 'matched' ? 'Metadata matched' : `Metadata: ${file.metadataStatus}`));
    const link = element('a', 'download', 'Download original'); link.href = `/api/media/${encodeURIComponent(file.id)}?download=1`; card.append(link); $('grid').append(card);
  }
  if (!selected.length) $('grid').append(element('p', 'empty', 'No media matches these filters.'));
  $('more').hidden = selected.length <= shown;
}
function selectAlbum(id, title, button) { albumId = id; shown = 80; $('heading').textContent = title; document.querySelectorAll('.nav').forEach(node => node.classList.remove('active')); button.classList.add('active'); render(); }
$('all').addEventListener('click', () => selectAlbum(null, 'All media', $('all')));
$('search').addEventListener('input', () => { shown = 80; render(); });
$('metadata').addEventListener('change', () => { shown = 80; render(); });
$('more').addEventListener('click', () => { shown += 80; render(); });
$('verify').addEventListener('click', async () => {
  $('verify').disabled = true; setStatus('Checking stored originals and sidecars…');
  try { const response = await fetch('/api/verify', { cache: 'no-store' }); const report = await response.json(); if (!response.ok || report.status !== 'ok') throw new Error('Verification did not pass. Run archivebridge verify for details.'); setStatus(`Verified ${report.filesChecked} media and ${report.sidecarsChecked} sidecar references against this manifest.`); }
  catch (error) { setStatus(error.message, true); }
  finally { $('verify').disabled = false; }
});
(async () => {
  try {
    const response = await fetch('/api/manifest', { cache: 'no-store' }); if (!response.ok) throw new Error('The archive manifest could not be opened.'); archive = await response.json();
    for (const field of ['files', 'sidecars', 'albums', 'sources', 'issues']) { if (archive[field] === null) archive[field] = []; if (!Array.isArray(archive[field])) throw new Error('The archive manifest has an unsupported collection.'); }
    $('total').textContent = archive.files.length.toLocaleString(); $('media-count').textContent = archive.files.length.toLocaleString(); $('album-count').textContent = archive.albums.length.toLocaleString(); $('source-count').textContent = archive.sources.length.toLocaleString(); $('issue-count').textContent = archive.issues.length.toLocaleString();
    for (const album of archive.albums) { const button = element('button', 'nav', album.title); button.append(element('span', '', album.mediaIds.length.toLocaleString())); button.addEventListener('click', () => selectAlbum(album.id, album.title, button)); $('albums').append(button); }
    $('issues-label').textContent = `Transfer notes (${archive.issues.length})`;
    for (const issue of archive.issues) { const item = element('div', 'issue'); item.append(element('strong', '', issue.code), element('p', '', issue.details), element('code', '', issue.entryPath || 'Source part')); $('issues').append(item); }
    setStatus('Manifest loaded. Use Verify archive to check stored bytes before relying on their integrity.'); render();
  } catch (error) { setStatus(error.message, true); }
})();
