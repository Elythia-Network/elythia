/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /chat/room/:roomId で 自分の message の "..." menu (ti-dots-circle-horizontal)
// → "Delete" item (ti-fw ti-trash) → /api/chat/messages/delete 直接
// round-trip する write-flow spec。
//
// XMessage.vue:26 の menu button は ti-dots-circle-horizontal。click で
// showMenu → popupMenu。自分 (isMe=true) の message には Delete item
// (ti-fw ti-trash, danger) が表示され、click で chat/messages/delete を
// 直接叩く (line 173)。confirm 無し最短 flow。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonWithIcon, clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /chat/room message delete via menu flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('open message menu → Delete → /api/chat/messages/delete', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. test 用 chat room を create
    const roomName = `pw-room-${Date.now().toString().slice(-9)}`;
    const roomResp = await callApi(request, 'chat/rooms/create', {
      i: root.token,
      name: roomName,
    });
    expect(roomResp.status()).toBe(200);
    const roomId = (await roomResp.json()).id;
    expect(roomId).toBeTruthy();

    // 2. メッセージを送信 (= 自分の message として登録)
    const messageText = `pw-msg-del-${Date.now()}`;
    const msgResp = await callApi(request, 'chat/messages/create-to-room', {
      i: root.token,
      toRoomId: roomId,
      text: messageText,
    });
    expect(msgResp.status()).toBe(200);
    const messageId = (await msgResp.json()).id;
    expect(messageId).toBeTruthy();

    // 3. /chat/room/:id を開く
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/chat/room/${roomId}`, {
      waitUntil: 'domcontentloaded',
    });

    // message text が body に出るまで待つ
    await page.waitForFunction(
      (t) => document.body.textContent?.includes(t) ?? false,
      messageText,
      { timeout: 20_000 },
    );

    // 4. 該当 message の menu button (ti-dots-circle-horizontal) を click。
    // 同 page 上の他 message 用 button もある場合があるので、message text を
    // 含む section 内の button を絞る。
    await page.waitForFunction(
      (t) => {
        const els = Array.from(document.querySelectorAll('div')) as HTMLDivElement[];
        return els.some((el) => {
          if (!(el.textContent ?? '').includes(t)) return false;
          return el.querySelector('button i.ti-dots-circle-horizontal') !== null;
        });
      },
      messageText,
      { timeout: 15_000 },
    );

    await clickWhenReady(page, 'i.ti-dots-circle-horizontal を持つボタン', (t) => {
      const els = Array.from(document.querySelectorAll('div')) as HTMLDivElement[];
      const target = els.find((el) => {
        if (!(el.textContent ?? '').includes(t)) return false;
        return el.querySelector('button i.ti-dots-circle-horizontal') !== null;
      });
      if (!target) return;
      const btn = Array.from(target.querySelectorAll('button')).find(
        (b) => b.querySelector('i.ti-dots-circle-horizontal') !== null,
      ) as HTMLButtonElement | undefined;
      return btn;
    }, messageText);

    // 5. popupMenu の "Delete" item (ti-fw ti-trash) を click
    const deleteResp = page.waitForResponse(
      (r) => r.url().includes('/api/chat/messages/delete') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickButtonWithIcon(page, 'i.ti-fw.ti-trash');
    await deleteResp;
  });
});
