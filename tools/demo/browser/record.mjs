// Copyright 2026 The A2A Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Records a browser scenario as a video. A scenario module exports a default
// async function that gets { page, params, pause, type }.
//
//   node record.mjs <scenario.mjs> --out <file.webm> [key=value...]
//   node record.mjs --login <url>
//
// --login opens the browser profile without recording, to sign in to a site
// such as Telegram Web once; recordings reuse the profile.

import { chromium } from 'playwright';
import { copyFile, mkdtemp, rm } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const home = process.env.DEMO_REAL_HOME || os.homedir();
const scale = Number(process.env.DEMO_PAUSE_SCALE || 1);

const opts = {
  profile: process.env.DEMO_BROWSER_PROFILE || path.join(home, '.cache', 'a2a-demo', 'browser-profile'),
  size: process.env.DEMO_BROWSER_SIZE || '1280x800',
  headless: process.env.DEMO_BROWSER_HEADLESS === '1',
};
const params = {};
let scenario;
const args = process.argv.slice(2);
for (let i = 0; i < args.length; i++) {
  const a = args[i];
  if (a === '--out') opts.out = args[++i];
  else if (a === '--login') opts.login = args[++i];
  else if (a === '--size') opts.size = args[++i];
  else if (a === '--profile') opts.profile = args[++i];
  else if (a === '--headless') opts.headless = true;
  else if (a.includes('=')) {
    const [k, ...v] = a.split('=');
    params[k] = v.join('=');
  } else scenario = a;
}

const [width, height] = opts.size.split('x').map(Number);
const viewport = { width, height };

if (opts.login) {
  const ctx = await chromium.launchPersistentContext(opts.profile, { headless: false, viewport });
  const page = ctx.pages()[0] ?? (await ctx.newPage());
  await page.goto(opts.login);
  console.error(`[demo] sign in, then close the browser window (profile ${opts.profile})`);
  await new Promise((resolve) => ctx.on('close', resolve));
  process.exit(0);
}

if (!scenario || !opts.out) {
  console.error('usage: node record.mjs <scenario.mjs> --out <file.webm> [key=value...] | --login <url>');
  process.exit(2);
}

const pause = (s) => new Promise((r) => setTimeout(r, s * 1000 * scale));
const type = (locator, text) => locator.pressSequentially(text, { delay: 45 * scale });

const videoDir = await mkdtemp(path.join(os.tmpdir(), 'a2a-demo-video-'));
const ctx = await chromium.launchPersistentContext(opts.profile, {
  headless: opts.headless,
  viewport,
  recordVideo: { dir: videoDir, size: viewport },
});
const page = ctx.pages()[0] ?? (await ctx.newPage());
let failed;
try {
  const { default: run } = await import(pathToFileURL(path.resolve(scenario)).href);
  await run({ page, params, pause, type });
} catch (err) {
  failed = err;
  const shot = opts.out.replace(/\.[^.]+$/, '') + '-failure.png';
  await page.screenshot({ path: shot }).catch(() => {});
  console.error(`[demo] browser scenario failed; screenshot ${shot}`);
}
const video = page.video();
await ctx.close();
await copyFile(await video.path(), opts.out);
await rm(videoDir, { recursive: true, force: true });
if (failed) throw failed;
console.error(`[demo] wrote ${opts.out}`);
