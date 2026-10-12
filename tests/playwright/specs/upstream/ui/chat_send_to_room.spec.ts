/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /chat/room/:id で textarea に message 入力 → send button (ti-send icon) →
// /api/chat/messages/create-to-room round-trip する **真の write-flow** spec。
//
// API setup: chat/rooms/create で room を作る (alice owned)。chat/room.vue
// は room.form.vue を inner で持ち、textarea + ti-send button を提供する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonWithIcon } from '../../../fixtures/ui_click';

test.describe('UI: /chat/room/:id send message flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('create room via API → type → send → /api/chat/messages/create-to-room', async ({
    page,
    baseURL,
    request,
  }) => {
    const roomName = `pwroom-${Date.now().toString().slice(-9)}`;
    const createResp = await callApi(request, 'chat/rooms/create', {
      i: root.token,
      name: roomName,
    });
    expect(createResp.status()).toBe(200);
    const roomId = (await createResp.json()).id;
    expect(roomId).toBeTruthy();

    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/chat/room/${roomId}`, { waitUntil: 'domcontentloaded' });

    // room form の textarea が hydrate
    await page.waitForFunction(
      () => document.querySelectorAll('textarea').length >= 1,
      { timeout: 20_000 },
    );

    const messageText = `pwmsg-${Date.now().toString().slice(-9)}`;
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
    }, messageText);

    // send button (ti-send icon) が enabled になるまで待つ
    await page.waitForFunction(
      () => {
        const btn = (document.querySelector('button i.ti-send')?.closest('button')) as
          | HTMLButtonElement
          | null;
        return btn != null && !btn.disabled;
      },
      { timeout: 5_000 },
    );

    const sendResp = page.waitForResponse(
      (r) =>
        r.url().includes('/api/chat/messages/create-to-room') && r.status() < 400,
      { timeout: 15_000 },
    );
    await clickButtonWithIcon(page, 'i.ti-send');
    const resp = await sendResp;
    expect(resp.status()).toBeLessThan(400);
  });
});
