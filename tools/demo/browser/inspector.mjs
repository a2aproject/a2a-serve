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

// Opens the inspector with a login link, answers the pending approval and
// opens the conversation. Params: url (the login link).
export default async function ({ page, params, pause }) {
  await page.goto(params.url);
  await page.getByRole('heading', { name: 'Pending approvals' }).waitFor();
  await pause(3);
  await page.getByRole('button', { name: /^allow/i }).first().click();
  await page.waitForLoadState();
  await pause(2);
  await page.locator('h2:has-text("Conversations") + table a').first().click();
  await page.getByRole('heading', { name: /^Conversation/ }).waitFor();
  await pause(4);
}
