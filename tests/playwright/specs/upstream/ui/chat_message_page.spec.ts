/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /chat/messages/:messageId (単一メッセージのページ) をブラウザで開く (#2441)。
//
// 通知やメンションから 1 通だけを開く導線がこのページ。`chat/messages/show` を
// 叩く **単独の経路**で、room / DM のタイムライン (`*-timeline`) とは別 API。
// タイムラインが出ても、このページが壊れていれば通知から辿れない。
//
// chatScope の既定は mutual なので、DM を成立させるには相互フォローが要る
// (chat_user_room.spec.ts と同じ前提)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

test.describe('UI: /chat/messages/:messageId', () => {
  let root: RootFixture;

  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });

  test.setTimeout(60_000);

  test('単一メッセージのページに本文が表示される', async ({ page, baseURL, request }) => {
    const peer = await signupUser(request, randomUsername('msgpeer'), DEFAULT_TEST_PASSWORD);
    expect((await callApi(request, 'following/create', { i: root.token, userId: peer.id })).status()).toBe(200);
    expect((await callApi(request, 'following/create', { i: peer.token, userId: root.id })).status()).toBe(200);

    const text = `pwmsgpage-${Date.now().toString().slice(-9)}`;
    const sent = await callApi(request, 'chat/messages/create-to-user', {
      i: root.token,
      toUserId: peer.id,
      text,
    });
    expect(sent.status()).toBe(200);
    const message = (await sent.json()) as { id: string };

    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/chat/messages/${message.id}`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    await expect(page.getByText(text, { exact: false }).first()).toBeVisible({ timeout: 20_000 });
  });

  test('存在しないメッセージ ID でも画面が壊れない', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    const resp = await page.goto(`${baseURL}/chat/messages/aaaaaaaaaaaaaaaa`, {
      waitUntil: 'domcontentloaded',
    });
    expect(resp!.status()).toBe(200);

    // 削除済みメッセージへの古いリンクを踏む経路。白画面のまま止まると
    // 利用者は何が起きたか分からない。ナビゲーションが出ていれば SPA は
    // 生きており、少なくとも他の画面へ移動できる。
    await expect(page.getByText('Timeline', { exact: false }).first()).toBeVisible({
      timeout: 20_000,
    });
  });
});
