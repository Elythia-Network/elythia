/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /chat/room/:id の "Members" tab → "Invite user" button →
// MkUserSelectDialog → username 入力 + user 選択 + OK →
// /api/chat/rooms/invitations/create round-trip する write-flow spec。
//
// MkPageHeader tab ti-users (= members) → room.members.vue の
// "Invite user" button (ti-plus + "Invite user") → emit inviteUser →
// room.vue:369 の inviteUser() で os.selectUser → MkUserSelectDialog
// (MkModalWindow + username input + user list + OK)。
// OK button は MkModalWindow の `withOkButton`、ti-check icon を持つ
// (= avatar_decoration_attach_via_dialog で確立 pattern)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonWithIcon, clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /chat/room invite via selectUser flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(90_000);

  test('Invite user → username search → select + OK → /api/chat/rooms/invitations/create', async ({
    page,
    baseURL,
    request,
  }) => {
    // 1. invitee を signup + 検索 hit させるため fresh username
    const invitee = await signupUser(request, `pwinv${Date.now().toString().slice(-9)}`);
    expect(invitee.id).toBeTruthy();

    // MkUserSelectDialog は users/search-by-username-and-host を引く。upstream
    // の UserSearchService は 4 query の UNION で、`updatedAt IS NULL` を拾う
    // のは「フォロー済み」分岐だけ (未フォローの inactive 分岐は
    // `updatedAt <= threshold` で NULL を除外する)。signup 直後の user は
    // updatedAt=NULL なので、root が follow していないと TS では検索に一切
    // 出てこない。mk-go は NULL も拾うため mk-go 単体では通っていた (#2276)。
    const followResp = await callApi(request, 'following/create', {
      i: root.token,
      userId: invitee.id,
    });
    expect(followResp.status()).toBe(200);

    // 2. test 用 chat room を create
    const roomName = `pw-invite-room-${Date.now().toString().slice(-9)}`;
    const roomResp = await callApi(request, 'chat/rooms/create', {
      i: root.token,
      name: roomName,
    });
    expect(roomResp.status()).toBe(200);
    const roomId = (await roomResp.json()).id;

    // 3. /chat/room/:id を root として開く
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/chat/room/${roomId}`, {
      waitUntil: 'domcontentloaded',
    });

    // 4. Members tab (= ti-users icon を持つ tab button) を click。
    // MkPageHeader の tab button は通常 page header section にある。
    await clickButtonWithIcon(page, 'i.ti-users');

    // 5. "Invite user" button (= room.members.vue:8、ti-plus icon、
    // ti-fw 修飾無し = page top の primary button) を待って click
    await clickWhenReady(page, '「invite」のボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) =>
          b.querySelector('i.ti-plus') !== null &&
          !b.querySelector('i.ti-fw') &&
          (b.textContent ?? '').toLowerCase().includes('invite'),
      ),
    );

    // 6. MkUserSelectDialog の username input が現れる (autofocus)。
    // 同 dialog は localOnly=true mode なので host input は無く、username
    // input 1 個だけ。type は default (text)。
    await page.waitForFunction(
      () => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.filter((i) => i.type === 'text').length >= 1;
      },
      { timeout: 10_000 },
    );

    // 7. invitee.username を type → search 結果が出る。
    // native `dispatchEvent(new Event('input'))` だと MkInput の
    // `@update:modelValue` → search() chain が走らないケースがあるため、
    // keystroke emulation + input event を自動 dispatch する
    // `locator.fill()` を使う。`.last()` で dialog 内の最新 mount input を取る。
    //
    // selector は `input[type="text"]` ではなく `input`。MkInput は type を
    // 明示しないので DOM に **type 属性が付かず**、CSS の
    // `[type="text"]` は 1 件も match しない (JS の `input.type` は既定値
    // "text" を返すので DOM 走査では気付けない)。ここで locator が解決せず
    // fill が 90s 待ち続けていた。
    await page.locator('input').last().fill(invitee.username);

    // 8. 検索結果リストに invitee が出るまで待つ
    await page.waitForFunction(
      (u) => document.body.textContent?.includes(u) ?? false,
      invitee.username,
      { timeout: 15_000 },
    );

    // 9. 検索結果の user item を click (= selected = user)
    // user item は `_button` class + MkAcct で username text を含む。
    await clickWhenReady(page, '検索結果の user item', (u) => {
      const els = Array.from(document.querySelectorAll('div')) as HTMLDivElement[];
      const target = els.find(
        (el) =>
          el.classList.contains('_button') &&
          (el.textContent ?? '').includes(u),
      );
      return target;
    }, invitee.username);

    // 10. MkModalWindow の OK button (ti-check) を click → API
    const inviteResp = page.waitForResponse(
      (r) =>
        r.url().includes('/api/chat/rooms/invitations/create') &&
        r.status() < 300,
      { timeout: 15_000 },
    );
    await clickWhenReady(page, 'i.ti-check を持つボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) => !b.disabled && b.querySelector('i.ti-check') !== null,
      ),
    );
    await inviteResp;
  });
});
