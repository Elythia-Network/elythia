/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /settings/mute-block をブラウザで操作する (#2441)。
//
// ミュート / ブロックは **一覧が出ないと解除できない**。API が 200 を返していても
// 画面に出ないと利用者は詰むので、API 検証だけでは足りない。
//
// このページの一覧は `MkFolder` の中にあり **既定で畳まれている**。開くには
// `data-testid="folder-header"` の button を click する必要がある。中身は
// `MkPagination` で遅延取得されるため、開いた後に一覧の描画を待つ。
//
// 文言は playwright.config.ts が browser locale を en-US に固定しているので
// 実文言で照合する。"Muted users" のフォルダは **2 つある** (通常のミュートと
// リノートミュート) ため、`getByText` の部分一致ではなく exact 一致で取る。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

/**
 * Open the collapsed MkFolder whose header label matches exactly.
 *
 * MkFolder renders its body only while opened (`v-if="opened"`), so the list
 * inside is absent from the DOM until the header is clicked.
 */
async function openFolder(page: import('@playwright/test').Page, label: string): Promise<void> {
  const header = page
    .locator('[data-testid="folder-header"]')
    .filter({ has: page.getByText(label, { exact: true }) })
    .first();
  await expect(header).toBeVisible({ timeout: 20_000 });
  await header.click();
}

test.describe('UI: /settings/mute-block', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('ミュートしたユーザーが一覧に表示される', async ({ page, baseURL, request }) => {
    const target = await signupUser(request, randomUsername('mutetgt'), DEFAULT_TEST_PASSWORD);
    const created = await callApi(request, 'mute/create', { i: root.token, userId: target.id });
    expect(created.status()).toBe(204);

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/mute-block`, { waitUntil: 'domcontentloaded' });

    await openFolder(page, 'Muted users');

    // MkUserCardMini が username を出す。ここに出ないとユーザーは自分が誰を
    // ミュートしているか確認できない。
    await expect(page.getByText(target.username, { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });

  test('ブロックしたユーザーが一覧に表示され、× で解除できる', async ({
    page,
    baseURL,
    request,
  }) => {
    const target = await signupUser(request, randomUsername('blocktgt'), DEFAULT_TEST_PASSWORD);
    const created = await callApi(request, 'blocking/create', { i: root.token, userId: target.id });
    expect(created.status()).toBe(200);

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/mute-block`, { waitUntil: 'domcontentloaded' });

    await openFolder(page, 'Blocked users');
    await expect(page.getByText(target.username, { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });

    // × は **popup menu を開くだけ**で、その中の "Unblock" が blocking/delete を
    // 呼ぶ (`unblock()` は `os.popupMenu` を出している)。× を押して API を待つと
    // 何も飛ばずに timeout する。
    //
    // 対象行の特定は「username と × の両方を含む div の最も内側」で行う。
    // 過去の spec が残したブロックが一覧に混ざりうるので、位置ではなく中身で選ぶ。
    const row = page
      .locator('div')
      .filter({ has: page.getByText(target.username, { exact: false }) })
      .filter({ has: page.locator('button:has(i.ti-x)') })
      .last();
    await row.locator('button:has(i.ti-x)').click();

    const deleted = page.waitForResponse(
      (r) => r.url().includes('/api/blocking/delete') && r.status() === 200,
      { timeout: 20_000 },
    );
    await page.getByText('Unblock', { exact: true }).first().click();
    await deleted;

    // サーバー側でも解除されている (画面だけ消えて実体が残ると誤解を招く)。
    await expect(async () => {
      const list = await callApi(request, 'blocking/list', { i: root.token, limit: 100 });
      const body = (await list.json()) as Array<{ blockee: { id: string } }>;
      expect(body.some((b) => b.blockee.id === target.id)).toBe(false);
    }).toPass({ timeout: 15_000 });
  });

  test('ワードミュートの設定欄が表示される', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/settings/mute-block`, { waitUntil: 'domcontentloaded' });

    // soft / hard は別物 (前者は隠すだけ、後者は取得自体しない)。両方の入口が
    // 出ていないと利用者はどちらを設定しているか分からない。
    await expect(page.getByText('Word mute', { exact: true }).first()).toBeVisible({
      timeout: 20_000,
    });
    await expect(page.getByText('Hard word mute', { exact: true }).first()).toBeVisible({
      timeout: 20_000,
    });
  });
});
