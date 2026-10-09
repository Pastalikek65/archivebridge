#!/usr/bin/env node

import { createHash } from 'node:crypto';
import { lstat, readdir, readFile, stat, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import path from 'node:path';
import { chromium } from 'playwright';

const require = createRequire(import.meta.url);
const PLAYWRIGHT_VERSION = require('playwright/package.json').version;

function parseArguments(argv) {
  const options = new Map();
  const allowed = new Set(['--url', '--out']);
  for (let i = 0; i < argv.length; i += 1) {
    const key = argv[i];
    if (!allowed.has(key) || options.has(key) || i + 1 >= argv.length || argv[i + 1].startsWith('--')) {
      throw new Error(`invalid or duplicate argument: ${key}`);
    }
    options.set(key, argv[++i]);
  }
  if (!options.has('--url') || !options.has('--out')) {
    throw new Error('usage: acceptance.mjs --url http://127.0.0.1:PORT/ --out DIRECTORY');
  }
  const url = new URL(options.get('--url'));
  if (url.protocol !== 'http:' || url.hostname !== '127.0.0.1' || !url.port || url.pathname !== '/') {
    throw new Error('--url must be a root URL on a literal 127.0.0.1 listener');
  }
  return { url: url.toString(), out: path.resolve(options.get('--out')) };
}

function sha256(data) {
  return createHash('sha256').update(data).digest('hex');
}

function check(report, name, passed, details, evidence = undefined) {
  const item = { name, status: passed ? 'passed' : 'failed', details };
  if (evidence !== undefined) item.evidence = evidence;
  report.checks.push(item);
  return passed;
}

async function main() {
  const report = {
    schemaVersion: 1,
    status: 'failed',
    browser: 'Chromium',
    playwrightVersion: PLAYWRIGHT_VERSION,
    nodeVersion: process.version,
    sandboxRequired: true,
    chromiumSandboxRequested: true,
    checks: [],
    externalRequests: [],
    pageErrors: [],
    screenshots: [],
  };
  let args;
  let browser;
  let context;
  let page;
  let outputAccepted = false;
  const externalSeen = new Set();
  const recordExternal = url => {
    if (!externalSeen.has(url)) {
      externalSeen.add(url);
      report.externalRequests.push(url);
    }
  };
  try {
    args = parseArguments(process.argv.slice(2));
    report.url = args.url;
    check(report, 'node-version', Number(process.versions.node.split('.')[0]) >= 20,
      `Node ${process.version} satisfies Playwright's Node 20+ runtime requirement.`);
    check(report, 'playwright-version', PLAYWRIGHT_VERSION === '1.64.0',
      `Installed Playwright version is ${PLAYWRIGHT_VERSION}; expected 1.64.0.`);
    const outInfo = await stat(args.out);
    const outLinkInfo = await lstat(args.out);
    if (!outInfo.isDirectory() || outLinkInfo.isSymbolicLink()) throw new Error('--out must be a regular directory prepared by the acceptance runner');
    if ((await readdir(args.out)).length !== 0) throw new Error('--out must be empty; existing evidence will not be replaced');
    outputAccepted = true;

    browser = await chromium.launch({ headless: true, chromiumSandbox: true, args: ['--no-proxy-server'] });
    check(report, 'chromium-sandbox-requested', report.chromiumSandboxRequested,
      'Playwright Chromium launch explicitly requested chromiumSandbox=true. This records launch configuration, not an independent measurement of operating-system sandbox state.');

    context = await browser.newContext({ acceptDownloads: true, viewport: { width: 1440, height: 1000 } });
    context.setDefaultTimeout(10000);
    context.setDefaultNavigationTimeout(15000);
    const origin = new URL(args.url).origin;
    await context.route('**/*', async route => {
      const requestURL = route.request().url();
      if (/^https?:/i.test(requestURL) && new URL(requestURL).origin !== origin) {
        recordExternal(requestURL);
        await route.abort('blockedbyclient');
      } else {
        await route.continue();
      }
    });
    context.on('request', request => {
      const requestURL = request.url();
      if (!/^https?:/i.test(requestURL)) return;
      try {
        if (new URL(requestURL).origin !== origin) recordExternal(requestURL);
      } catch {
        recordExternal(requestURL);
      }
    });
    page = await context.newPage();
    page.on('pageerror', error => report.pageErrors.push(error.message));
    const pageResponse = await page.goto(args.url, { waitUntil: 'networkidle' });
    check(report, 'viewer-page-loads', pageResponse?.status() === 200,
      `Viewer root returned HTTP ${pageResponse?.status() ?? 'no response'}.`);
    await page.locator('#status').waitFor({ state: 'visible' });
    await page.waitForFunction(() => document.querySelector('#status')?.textContent?.startsWith('Manifest loaded.'));

    const mediaCount = await page.locator('#media-count').innerText();
    const albumCount = await page.locator('#album-count').innerText();
    const cardCount = await page.locator('.card').count();
    check(report, 'six-media-occurrences', mediaCount === '6' && cardCount === 6,
      `Summary shows ${mediaCount} occurrences and ${cardCount} rendered cards.`);
    check(report, 'three-albums', albumCount === '3' && await page.locator('#albums button.nav').count() === 3,
      `Summary shows ${albumCount} albums and ${await page.locator('#albums button.nav').count()} album controls.`);
    const displayedDates = await page.locator('.card').evaluateAll(cards => cards.map(card => ({
      path: card.querySelector('h3')?.title ?? '',
      date: card.querySelector('.date')?.textContent?.trim() ?? '',
    })));
    const expectedDates = new Map([
      ['Takeout/Google Photos/Photos from 2023/harbor.png', '2023-11-14'],
      ['Takeout/Google Photos/Weekend/harbor.png', '2023-11-14'],
      ['Takeout/Google Photos/Weekend/trail.png', '2023-11-15'],
      ['Takeout/Google Photos/Family/harbor.png', '2023-11-14'],
      ['Takeout/Google Photos/Family/lake.png', '2023-11-14'],
      ['Takeout/Google Photos/Ambiguous/collision.png', 'Date unresolved'],
    ]);
    check(report, 'visible-utc-dates', displayedDates.length === 6 && displayedDates.every(item => expectedDates.get(item.path) === item.date),
      'Visible dates match the pinned UTC dates; ambiguous metadata remains unresolved.', displayedDates);

    const previewImages = page.locator('.card img');
    const previewCount = await previewImages.count();
    const previewResults = [];
    for (let i = 0; i < previewCount; i += 1) {
      const image = previewImages.nth(i);
      await image.scrollIntoViewIfNeeded();
      previewResults.push(await image.evaluate(async node => {
        if (!node.complete) await new Promise(resolve => { node.addEventListener('load', resolve, { once: true }); node.addEventListener('error', resolve, { once: true }); });
        return { alt: node.alt, complete: node.complete, width: node.naturalWidth, height: node.naturalHeight };
      }));
    }
    check(report, 'all-media-previews-render', previewCount === 6 && previewResults.length === 6 && previewResults.every(image => image.complete && image.width > 0 && image.height > 0),
      `Decoded ${previewResults.filter(image => image.width > 0 && image.height > 0).length} of ${previewCount} image previews.`, previewResults);

    const provenanceManifest = await page.evaluate(async () => (await fetch('/api/manifest', { cache: 'no-store' })).json());
    const cardSources = await page.locator('.card').evaluateAll(cards => cards.map(card => ({ path: card.querySelector('h3')?.title, source: card.querySelector('.source')?.textContent })));
    check(report, 'occurrence-source-provenance', cardSources.length === 6 && cardSources.every(card => {
      const occurrence = provenanceManifest.files.find(file => file.entryPath === card.path);
      return occurrence && card.source === provenanceManifest.sources[occurrence.sourceIndex].name;
    }), 'Every media occurrence visibly identifies its original source part.', cardSources);
    const albumSources = await page.locator('#albums .album-source').allTextContents();
    check(report, 'album-source-provenance', albumSources.length === 3 && provenanceManifest.albums.every((album, index) => {
      const member = provenanceManifest.files.find(file => file.albumIds.includes(album.id));
      return member && albumSources[index] === provenanceManifest.sources[member.sourceIndex].name;
    }), 'Album controls retain source-part provenance instead of inferring a merged album identity.', albumSources);
    await page.locator('#repeated').focus();
    await page.keyboard.press('Enter');
    const repeatedCards = await page.locator('.card').count();
    const repeatedNames = await page.locator('.card h3').allTextContents();
    const repeatedSources = await page.locator('.card .source').allTextContents();
    check(report, 'repeated-content-preserves-occurrences', repeatedCards === 3 && repeatedNames.every(name => name === 'harbor.png') && new Set(repeatedSources).size === 2 && await page.locator('#repeat-note').isVisible(),
      'The repeated-content view keeps all three identical originals and both source-part relationships visible.', { repeatedCards, repeatedNames, repeatedSources });
    check(report, 'keyboard-navigation-state', await page.locator('#repeated').getAttribute('aria-pressed') === 'true' && await page.locator('#all').getAttribute('aria-pressed') === 'false' && await page.locator('#heading').innerText() === 'Repeated content',
      'Native Enter activation updates the selected navigation state and heading.');
    await page.locator('#all').click();
    check(report, 'repeated-view-does-not-remove-files', await page.locator('.card').count() === 6 && await page.locator('#all').getAttribute('aria-pressed') === 'true',
      'Returning to All media restores all six occurrences.');

    const familyButton = page.locator('#albums button.nav').filter({ hasText: /^Family\s*2?$/ });
    const familyButtons = page.locator('#albums button.nav').filter({ has: page.locator('span') });
    let familyFound = await familyButton.count();
    if (familyFound !== 1) {
      const albumButtons = await page.locator('#albums button.nav').all();
      for (const button of albumButtons) {
        if ((await button.innerText()).trim().startsWith('Family')) {
          await button.click();
          familyFound = 1;
          break;
        }
      }
    } else {
      await familyButton.click();
    }
    const familyCards = await page.locator('.card').count();
    check(report, 'family-album-filter', familyFound === 1 && familyCards === 2 && await page.locator('#heading').innerText() === 'Family',
      `Family album filter rendered ${familyCards} cards.`, { controlsWithCount: await familyButtons.count() });

    await page.locator('#all').click();
    await page.locator('#metadata').selectOption('unresolved');
    const reviewCards = await page.locator('.card').count();
    const reviewNames = await page.locator('.card h3').allTextContents();
    const reviewDates = await page.locator('.card .date').allTextContents();
    check(report, 'needs-review-filter', reviewCards === 1 && reviewNames.length === 1 && reviewNames[0] === 'collision.png' && reviewDates[0] === 'Date unresolved',
      `Needs review filter rendered ${reviewCards} card(s): ${reviewNames.join(', ') || '(none)'}.`);

    await page.locator('#metadata').selectOption('all');
    await page.locator('#all').click();
    // Delay delivery of a genuine local response so cancellation is observable
    // even for tiny fixtures. This proves UI cancellation, not a timed server
    // load or operating-system cancellation boundary.
    let releaseResponse;
    let markResponseReady;
    let markRouteDone;
    const responseGate = new Promise(resolve => { releaseResponse = resolve; });
    const responseReady = new Promise(resolve => { markResponseReady = resolve; });
    const routeDone = new Promise(resolve => { markRouteDone = resolve; });
    const verificationRoute = async route => {
      try {
        const response = await route.fetch();
        markResponseReady(response.status());
        await responseGate;
        await route.fulfill({ response }).catch(error => {
          if (!route.request().failure()?.errorText?.includes('ERR_ABORTED')) throw error;
        });
      } finally { markRouteDone(); }
    };
    await page.route('**/api/verify', verificationRoute);
    await page.locator('#verify').click();
    const delayedStatus = await responseReady;
    await page.locator('#cancel-verify').click();
    await page.waitForFunction(() => document.querySelector('#status')?.textContent === 'Verification cancelled. No integrity result was recorded.');
    check(report, 'verification-cancellation', delayedStatus === 200 && await page.locator('#verify').isEnabled() && await page.locator('#cancel-verify').isHidden(),
      'Cancelling a verification while a genuine HTTP 200 result was held for controlled delivery produces no success claim and permits retry.');
    releaseResponse();
    await routeDone;
    await page.unroute('**/api/verify', verificationRoute);
    await page.locator('#verify').click();
    await page.waitForFunction(() => document.querySelector('#status')?.textContent?.startsWith('Verified 6 media and 7 sidecar references'));
    const verifiedStatus = await page.locator('#status').innerText();
    check(report, 'viewer-verification', verifiedStatus.includes('Verified 6 media and 7 sidecar references'), verifiedStatus);

    const rootHeaders = await pageResponse.allHeaders();
    const csp = rootHeaders['content-security-policy'] ?? '';
    const corp = rootHeaders['cross-origin-resource-policy'] ?? '';
    const securityHeadersOK = csp.includes("default-src 'none'") && csp.includes("connect-src 'self'") && corp === 'same-origin' && rootHeaders['x-content-type-options'] === 'nosniff';
    check(report, 'viewer-security-headers', securityHeadersOK,
      'Root response includes a restrictive CSP, same-origin CORP, and nosniff.',
      { contentSecurityPolicy: csp, crossOriginResourcePolicy: corp, xContentTypeOptions: rootHeaders['x-content-type-options'] ?? '' });

    const hostileHost = await context.request.get(`${origin}/api/manifest`, { headers: { Host: 'attacker.invalid' } });
    const hostileOrigin = await context.request.get(`${origin}/api/manifest`, { headers: { Origin: 'https://attacker.invalid' } });
    const writeAttempt = await context.request.post(`${origin}/api/manifest`, { data: 'must-not-be-accepted', headers: { Origin: origin } });
    check(report, 'host-origin-post-blocked', hostileHost.status() === 403 && hostileOrigin.status() === 403 && writeAttempt.status() === 405,
      `Foreign Host=${hostileHost.status()}, foreign Origin=${hostileOrigin.status()}, POST=${writeAttempt.status()}.`,
      { foreignHostStatus: hostileHost.status(), foreignOriginStatus: hostileOrigin.status(), postStatus: writeAttempt.status() });

    await writeFile(path.join(args.out, 'desktop.png'), await page.screenshot({ fullPage: true, animations: 'disabled' }), { flag: 'wx' });
    const desktopInfo = await stat(path.join(args.out, 'desktop.png'));
    const desktopBytes = await readFile(path.join(args.out, 'desktop.png'));
    report.screenshots.push({ path: 'desktop.png', bytes: desktopInfo.size, sha256: sha256(desktopBytes), viewport: { width: 1440, height: 1000 } });
    check(report, 'desktop-screenshot', desktopInfo.size > 0, 'Desktop screenshot was saved and hashed.');

    await page.setViewportSize({ width: 390, height: 844 });
    await writeFile(path.join(args.out, 'mobile.png'), await page.screenshot({ fullPage: true, animations: 'disabled' }), { flag: 'wx' });
    const mobileInfo = await stat(path.join(args.out, 'mobile.png'));
    const mobileBytes = await readFile(path.join(args.out, 'mobile.png'));
    report.screenshots.push({ path: 'mobile.png', bytes: mobileInfo.size, sha256: sha256(mobileBytes), viewport: { width: 390, height: 844 } });
    check(report, 'mobile-screenshot', mobileInfo.size > 0, 'Mobile screenshot was saved and hashed.');

    const manifest = await page.evaluate(async () => (await fetch('/api/manifest', { cache: 'no-store' })).json());
    check(report, 'manifest-excludes-local-source-paths', Array.isArray(manifest.sources) && manifest.sources.length === 2 && manifest.sources.every(source => !Object.hasOwn(source, 'path')),
      'The viewer manifest exposes source identities without local filesystem paths.', manifest.sources);
    const firstCard = page.locator('.card').first();
    const downloadLink = firstCard.locator('a.download');
    const linkHref = await downloadLink.getAttribute('href');
    const mediaId = decodeURIComponent(new URL(linkHref, args.url).pathname.split('/').at(-1));
    const expectedFile = manifest.files.find(file => file.id === mediaId);
    const [download] = await Promise.all([
      page.waitForEvent('download'),
      downloadLink.click(),
    ]);
    const stream = await download.createReadStream();
    const chunks = [];
    for await (const chunk of stream) chunks.push(chunk);
    const downloadBytes = Buffer.concat(chunks);
    const downloadHash = sha256(downloadBytes);
    const filename = expectedFile?.entryPath.split('/').at(-1);
    check(report, 'download-original-sha256', Boolean(expectedFile) && downloadHash === expectedFile.sha256 && BigInt(downloadBytes.length) === BigInt(expectedFile.bytes) && download.suggestedFilename() === filename,
      `Downloaded ${download.suggestedFilename()} (${downloadBytes.length} bytes), SHA-256 ${downloadHash}.`,
      { expectedSha256: expectedFile?.sha256, actualSha256: downloadHash, expectedBytes: expectedFile?.bytes, actualBytes: downloadBytes.length });

    check(report, 'no-external-requests', report.externalRequests.length === 0,
      report.externalRequests.length === 0 ? 'The page made no requests outside its own origin.' : 'The page made requests outside its own origin.',
      report.externalRequests.slice());
    check(report, 'no-page-errors', report.pageErrors.length === 0,
      report.pageErrors.length === 0 ? 'The browser recorded no uncaught page errors.' : 'The browser recorded uncaught page errors.', report.pageErrors.slice());
    report.status = report.checks.every(item => item.status === 'passed') ? 'passed' : 'failed';
  } catch (error) {
    const message = error instanceof Error ? `${error.name}: ${error.message}` : String(error);
    check(report, 'browser-harness-exception', false, message);
  } finally {
    if (context) {
      try { await context.close(); } catch (error) { check(report, 'browser-context-cleanup', false, String(error)); }
    }
    if (browser) {
      try { await browser.close(); } catch (error) { check(report, 'browser-cleanup', false, String(error)); }
    }
  }

  report.status = report.checks.length > 0 && report.checks.every(item => item.status === 'passed') ? 'passed' : 'failed';
  if (args && outputAccepted) {
    try { await writeFile(path.join(args.out, 'browser-report.json'), `${JSON.stringify(report, null, 2)}\n`, { flag: 'wx' }); }
    catch (error) { check(report, 'browser-report-persisted', false, `Could not persist browser report: ${error}`); report.status = 'failed'; }
  }
  process.stdout.write(`${JSON.stringify(report)}\n`);
  return report.status === 'passed' ? 0 : 1;
}

main().then(code => { process.exitCode = code; }).catch(error => {
  process.stderr.write(`browser acceptance fatal: ${error instanceof Error ? error.stack : String(error)}\n`);
  process.stdout.write(`${JSON.stringify({ schemaVersion: 1, status: 'failed', checks: [{ name: 'browser-harness-fatal', status: 'failed', details: String(error) }] })}\n`);
  process.exitCode = 1;
});
