/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /chat/user/:userId (1 対 1 の DM) をブラウザで操作する (#2441)。
//
// 既存の chat spec は `/chat/room/:roomId` (グループ) しか触っていない。DM は
// 同じ `room.vue` を使うが **API が別系統**で、`props.userId` の分岐から
// `chat/messages/user-timeline` を読み、送信は `chat/messages/create-to-user`
// を叩く。room 側が通っても DM が通る保証はない。
//
// textarea への入力は value setter + input イベントで行う。Vue の v-model は
// `fill()` の合成入力を拾わないことがあるため、既存の chat_send_to_room spec と
// 同じ方式に揃える。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonWithIcon } from '../../../fixtures/ui_click';

/**
 * Create a peer that root is allowed to chat with.
 *
 * `chatScope` defaults to `mutual`, so a DM to a stranger is rejected with
 * RECIPIENT_CANNOT_CHAT (403). Establish a mutual follow instead of relaxing
 * the setting, since that is the relationship real DMs happen under.
 */
async function createChattablePeer(
  request: import('@playwright/test').APIRequestContext,
  root: RootFixture,
  prefix: string,
): Promise<{ id: string; token: string; username: string }> {
  const peer = await signupUser(request, randomUsername(prefix), DEFAULT_TEST_PASSWORD);
  const a = await callApi(request, 'following/create', { i: root.token, userId: peer.id });
  expect(a.status()).toBe(200);
  const b = await callApi(request, 'following/create', { i: peer.token, userId: root.id });
  expect(b.status()).toBe(200);
  return peer;
}

/** Type text into the chat form the way Vue's v-model actually observes. */
async function fillMessage(page: import('@playwright/test').Page, text: string): Promise<void> {
  await page.waitForFunction(() => document.querySelectorAll('textarea').length >= 1, {
    timeout: 20_000,
  });
  await page.evaluate((m) => {
    const ta = document.querySelector('textarea') as HTMLTextAreaElement | null;
    if (!ta) return;
    ta.focus();
    const setter = Object.getOwnPropertyDescriptor(
      window.HTMLTextAreaElement.prototype,
      'value',
    )?.set;
    setter?.call(ta, m);
    ta.dispatchEvent(new Event('input', { bubbles: true }));
  }, text);

  await page.waitForFunction(
    () => {
      const btn = document.querySelector('button i.ti-send')?.closest('button') as
        | HTMLButtonElement
        | null;
      return btn != null && !btn.disabled;
    },
    { timeout: 10_000 },
  );
}

test.describe('UI: /chat/user/:userId direct message', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('相手の名前と過去のメッセージが表示される', async ({ page, baseURL, request }) => {
    const peer = await createChattablePeer(request, root, 'dmpeer');
    const text = `pwdm-${Date.now().toString().slice(-9)}`;
    const sent = await callApi(request, 'chat/messages/create-to-user', {
      i: root.token,
      toUserId: peer.id,
      text,
    });
    expect(sent.status()).toBe(200);

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/chat/user/${peer.id}`, { waitUntil: 'domcontentloaded' });

    // 相手が誰か分からない DM 画面は使い物にならない (users/show 経路)。
    await expect(page.getByText(peer.username, { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
    // 過去ログは chat/messages/user-timeline 経路。room 側とは別 API。
    await expect(page.getByText(text, { exact: false }).first()).toBeVisible({ timeout: 20_000 });
  });

  test('画面から送信すると chat/messages/create-to-user が呼ばれる', async ({
    page,
    baseURL,
    request,
  }) => {
    const peer = await createChattablePeer(request, root, 'dmsend');

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/chat/user/${peer.id}`, { waitUntil: 'domcontentloaded' });

    const text = `pwdmsend-${Date.now().toString().slice(-9)}`;
    await fillMessage(page, text);

    const sendResp = page.waitForResponse(
      (r) => r.url().includes('/api/chat/messages/create-to-user') && r.status() < 400,
      { timeout: 20_000 },
    );
    await clickButtonWithIcon(page, 'i.ti-send');
    await sendResp;

    // 送信直後に自分の画面へ出る (楽観描画でなく実データ)。
    await expect(page.getByText(text, { exact: false }).first()).toBeVisible({ timeout: 20_000 });

    // 相手側の履歴にも入っている。画面にだけ出て届いていないと気付けない。
    await expect(async () => {
      const timeline = await callApi(request, 'chat/messages/user-timeline', {
        i: peer.token,
        userId: root.id,
        limit: 20,
      });
      expect(timeline.status()).toBe(200);
      const body = (await timeline.json()) as Array<{ text: string | null }>;
      expect(body.some((m) => m.text === text)).toBe(true);
    }).toPass({ timeout: 15_000 });
  });
});
