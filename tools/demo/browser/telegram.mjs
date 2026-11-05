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

// Chats with the agent's bot in Telegram Web (the K client). Sign in to
// Telegram Web once with `node record.mjs --login https://web.telegram.org/k/`.
//
// Params:
//   link     a t.me start link to open and press Start on, such as the owner link
//   bot      the bot's username, to open its chat when there is no link
//   message  a message to send
//   approve  "1" to press the first "Allow" button of an approval request
//   wait     a regular expression for the reply to wait for (default: any new message)
//   settle   seconds without new messages to wait for, for replies that arrive later
export default async function ({ page, params, pause, type }) {
  // Hides the chat list, which shows the signed-in account's other chats.
  await page.addInitScript(() => {
    document.addEventListener('DOMContentLoaded', () => {
      const style = document.createElement('style');
      style.textContent = '#column-left { display: none !important; }';
      document.head.append(style);
    });
  });
  if (params.link) {
    const url = new URL(params.link);
    const domain = url.pathname.replace(/^\//, '');
    const start = url.searchParams.get('start');
    const tg = `tg://resolve?domain=${domain}${start ? `&start=${start}` : ''}`;
    await page.goto(`https://web.telegram.org/k/#?tgaddr=${encodeURIComponent(tg)}`);
    // Telegram Web sends /start by itself when the chat already exists, and
    // shows a Start button only for a new chat.
    const startButton = page.getByText(/^start$/i).filter({ visible: true }).last();
    const linked = page.getByText(/Linked as owner/).last();
    await startButton.or(linked).first().waitFor({ timeout: 60_000 });
    if (await startButton.isVisible()) {
      await pause(2);
      await startButton.click();
    }
    await linked.waitFor({ timeout: 60_000 });
    await pause(3);
  } else if (params.bot) {
    await page.goto(`https://web.telegram.org/k/#@${params.bot.replace(/^@/, '')}`);
  }

  const input = page.locator('.input-message-input[contenteditable="true"]').last();
  await input.waitFor({ timeout: 60_000 });
  if (!params.message) return;

  const bubbles = page.locator('.bubble');
  const before = await bubbles.count();
  await input.click();
  await type(input, params.message);
  await pause(1);
  await page.keyboard.press('Enter');

  if (params.approve === '1') {
    const allow = page.locator('.reply-markup-button', { hasText: /allow/i }).first();
    await allow.waitFor({ timeout: 300_000 });
    await pause(3);
    await allow.click();
  }

  if (params.settle) {
    const quiet = Number(params.settle) * 1000;
    const deadline = Date.now() + 900_000;
    let count = await bubbles.count();
    let changed = Date.now();
    while (Date.now() - changed < quiet && Date.now() < deadline) {
      await page.waitForTimeout(2000);
      const n = await bubbles.count();
      if (n !== count) {
        count = n;
        changed = Date.now();
      }
    }
  } else if (params.wait) {
    await page.getByText(new RegExp(params.wait)).last().waitFor({ timeout: 600_000 });
  } else {
    await page.waitForFunction((n) => document.querySelectorAll('.bubble').length > n + 1, before, {
      timeout: 600_000,
    });
  }
  await pause(5);
}
