/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 2 人の間のチャットを、2 つの画面で通す (#4072)。既存の chat_* の spec は送る側だけを見ている。
//
//   - A が画面から送ったメッセージが、開いている B の画面に再読み込みなしで届く (ストリーミング)
//   - B が A のメッセージにリアクションを付ける (相手のメッセージにだけ付けられる)
//   - 履歴が 20 件を超えると「Load more」で古いものを読む
//   - B がルームに招待され、ルームを開いて参加し、見出しのメニューから退出する
//
// DM は既定の chatScope (mutual) に合わせて相互フォローで送る。リアクションの絵文字は、管理者の API で
// 作ったカスタム絵文字をピッカーの検索で選ぶ。

import { type Browser, expect, type Page, test } from '@playwright/test';
import { cleanupApi, expectApiOk, rootFixture } from '../../../fixtures/admin_table';
import { type Principal } from '../../../fixtures/auth';
import { uploadTinyPNG } from '../../../fixtures/files';
import { signinWithState } from '../../../fixtures/settings_table';
import { clickButtonContainingText, clickButtonWithIcon, clickWhenReady } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, clickMenuItem, follow, freshUser } from '../../../fixtures/ui_timeline';

/** A second signed-in page in a context of its own. */
async function otherPage(browser: Browser, request: import('@playwright/test').APIRequestContext, baseURL: string, user: Principal): Promise<Page> {
  const context = await browser.newContext({ ignoreHTTPSErrors: true, locale: 'en-US' });
  const page = await context.newPage();
  await signinWithState(page, request, baseURL, user);
  return page;
}

/** Types into the chat form the way v-model observes it (chat_user_room.spec.ts). */
async function fillMessage(page: Page, text: string): Promise<void> {
  await expect(page.locator('textarea')).toHaveCount(1, { timeout: 20_000 });
  await page.evaluate((m) => {
    const ta = document.querySelector('textarea') as HTMLTextAreaElement;
    ta.focus();
    Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value')?.set?.call(ta, m);
    ta.dispatchEvent(new Event('input', { bubbles: true }));
  }, text);
}

test.describe('UI: chat between two people', () => {
  test.setTimeout(90_000);

  let a: Principal;
  let b: Principal;
  test.beforeEach(async ({ request }) => {
    a = await freshUser(request, 'pwcha');
    b = await freshUser(request, 'pwchb');
    await follow(request, a, b);
    await follow(request, b, a);
  });

  test('a message sent by A appears on the open page of B', coversRoutes('/chat/user/:userId'), async ({ browser, page, request, baseURL }) => {
    const pageB = await otherPage(browser, request, baseURL!, b);
    try {
      await gotoRoute(pageB, baseURL!, `/chat/user/${a.id}`);
      await expect(pageB.locator('textarea')).toHaveCount(1, { timeout: 20_000 });

      await signinWithState(page, request, baseURL!, a);
      await gotoRoute(page, baseURL!, `/chat/user/${b.id}`);
      const text = `pw live ${Date.now().toString(36)}`;
      await fillMessage(page, text);
      const sent = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/chat/messages/create-to-user', { timeout: 15_000 });
      await clickButtonWithIcon(page, 'i.ti-send');
      expect((await sent).ok(), 'chat/messages/create-to-user が失敗した').toBe(true);

      // B は読み込み直さずに受け取る
      await expect(pageB.getByText(text).first(), '開いている相手の画面に届かない').toBeVisible({ timeout: 15_000 });
    } finally {
      await pageB.context().close();
    }
  });

  test('react to a message of the other person', coversRoutes('/chat/user/:userId'), async ({ page, request, baseURL }) => {
    const root = rootFixture();
    const file = await uploadTinyPNG(request, baseURL!, root.token, 'pw-chat-emoji.png');
    const emoji = `pwchat${Date.now().toString(36)}`;
    const added = await expectApiOk(request, 'admin/emoji/add', {
      i: root.token, name: emoji, fileId: file.id, category: null, aliases: [], license: null,
      isSensitive: false, localOnly: false, roleIdsThatCanBeUsedThisEmojiAsReaction: [],
    }) as { id: string };
    try {
      const message = await apiOk(request, 'chat/messages/create-to-user', { i: a.token, toUserId: b.id, text: `pw react ${Date.now().toString(36)}` }) as { id: string; text: string };
      await signinWithState(page, request, baseURL!, b);
      await gotoRoute(page, baseURL!, `/chat/user/${a.id}`);
      await expect(page.getByText(message.text).first()).toBeVisible({ timeout: 20_000 });

      await clickButtonWithIcon(page, 'i.ti-dots-circle-horizontal');
      await clickMenuItem(page, 'Reactions');
      await page.getByTestId('emoji-picker-search').fill(emoji);
      const reacted = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/chat/messages/react', { timeout: 15_000 });
      await clickWhenReady(page, `検索の結果の絵文字「${emoji}」`, (n: string) => Array.from(document.querySelectorAll('[data-testid="emoji-picker-search-result"]')).find((el) => (el.getAttribute('title') ?? '').includes(n)), emoji);
      expect((await reacted).ok(), 'chat/messages/react が失敗した').toBe(true);

      const shown = await apiOk(request, 'chat/messages/show', { i: a.token, messageId: message.id }) as { reactions: { reaction: string; user: { id: string } }[] };
      expect(shown.reactions.some((r) => r.reaction.includes(emoji) && r.user.id === b.id), 'リアクションが付かない').toBe(true);
    } finally {
      await cleanupApi(request, 'admin/emoji/delete', { i: root.token, id: added.id });
    }
  });

  test('load older messages', coversRoutes('/chat/user/:userId'), async ({ page, request, baseURL }) => {
    // 最初に読むのは 20 件。21 件目 (いちばん古いもの) は「Load more」で読む
    const oldest = `pw oldest ${Date.now().toString(36)}`;
    await apiOk(request, 'chat/messages/create-to-user', { i: a.token, toUserId: b.id, text: oldest });
    for (let n = 0; n < 20; n++) await apiOk(request, 'chat/messages/create-to-user', { i: a.token, toUserId: b.id, text: `pw filler ${n}` });

    await signinWithState(page, request, baseURL!, b);
    await gotoRoute(page, baseURL!, `/chat/user/${a.id}`);
    await expect(page.getByText('pw filler 19').first()).toBeVisible({ timeout: 20_000 });
    await expect(page.getByText(oldest), '最初から 21 件目が出ている (前提)').toHaveCount(0);

    const older = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/chat/messages/user-timeline' && r.request().postDataJSON()?.untilId != null, { timeout: 15_000 });
    await clickButtonContainingText(page, 'Load more');
    expect((await older).ok(), '古いメッセージを読めない').toBe(true);
    await expect(page.getByText(oldest).first(), '古いメッセージが出ない').toBeVisible();
  });

  test('join a room by invitation and leave it', coversRoutes('/chat/room/:roomId'), async ({ page, request, baseURL }) => {
    const room = await apiOk(request, 'chat/rooms/create', { i: a.token, name: `pw room ${Date.now().toString(36)}` }) as { id: string };
    await apiOk(request, 'chat/rooms/invitations/create', { i: a.token, roomId: room.id, userId: b.id });
    const joining = async () => (await apiOk(request, 'chat/rooms/joining', { i: b.token, limit: 100 }) as { room: { id: string } }[]).map((x) => x.room.id);

    await signinWithState(page, request, baseURL!, b);
    await gotoRoute(page, baseURL!, `/chat/room/${room.id}`);
    // メンバーでなく招待があるときは、参加するかを尋ねる
    const joined = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/chat/rooms/join', { timeout: 15_000 });
    await page.getByTestId('modal-dialog-ok').click();
    expect((await joined).ok(), 'chat/rooms/join が失敗した').toBe(true);
    expect(await joining(), '参加しても参加中の一覧に無い').toContain(room.id);
    // 参加の確認のダイアログが閉じきってから、退出の確認を開く
    await expect(page.getByTestId('modal-dialog-ok')).toHaveCount(0);

    await clickButtonWithIcon(page, 'i.ti-dots');
    await clickMenuItem(page, 'Leave room');
    const left = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/chat/rooms/leave', { timeout: 15_000 });
    await page.getByTestId('modal-dialog-ok').click();
    expect((await left).ok(), 'chat/rooms/leave が失敗した').toBe(true);
    expect(await joining(), '退出しても参加中の一覧に残る').not.toContain(room.id);
  });
});
