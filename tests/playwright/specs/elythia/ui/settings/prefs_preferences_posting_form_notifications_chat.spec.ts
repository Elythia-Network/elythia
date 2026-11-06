/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定の表 (#4056): /settings/preferences の「Posting form」「Notifications」
// 「Chat with user」の見出しの端末の設定を、部品の操作 → 保存 → 再読み込みの後の
// 表示 → 効き目、の順に確かめる。どれも行が少ないので 1 つのファイルにまとめる。
//
// 効き目は、設定を変えていない (既定の値の) ときには必ず落ちる形で書く
// (`PW_SETTINGS_MUTATION=1` の変異の確かめ)。「無い」ことを見るときは、本来
// それがある画面にいることを先に確かめる。

import { type APIRequestContext, expect, type Page, test } from '@playwright/test';
import { type Principal, randomUsername, signupUser } from '../../../../fixtures/auth';
import { pageBody } from '../../../../fixtures/route_check';
import { coversPrefs, expectHappens, type PrefRow, prefTitle, runPrefRow } from '../../../../fixtures/settings_table';
import { clickOnlyTestId, openPostForm, setPostText, submitPostForm, waitForPostForm } from '../../../../fixtures/ui_post_form';
import { gotoRoute } from '../../../../fixtures/ui_route';
import { apiOk, follow, postNote, runTag, waitForApiBody } from '../../../../fixtures/ui_timeline';

const ROUTE = '/settings/preferences';

/** Clicks "Check notification appearance" (in the Notifications folder) and waits for `count` toasts. */
async function showTestToasts(page: Page, count: number): Promise<void> {
  const button = pageBody(page).getByRole('button', { name: 'Check notification appearance' });
  for (let i = 0; i < count; i++) await button.click();
  await expect(page.getByTestId('toast-notification')).toHaveCount(count);
}

/** Signs up a user who follows `user` back and forth (chat needs a mutual follow by default). */
async function chatPartner(request: APIRequestContext, user: Principal, name: string): Promise<Principal> {
  const other = await signupUser(request, randomUsername('pwchat'));
  await apiOk(request, 'i/update', { i: other.token, name });
  await follow(request, user, other);
  await follow(request, other, user);
  return other;
}

test.describe('settings: preferences (Posting form, Notifications, Chat with user)', () => {
  test.setTimeout(60_000);
  const ROWS: PrefRow[] = [
    // Posting form
    {
      key: 'keepCw', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page, request, baseURL, user }) => {
        const noteId = await postNote(request, user, 'pwbody', { cw: runTag('pwcw') });
        await gotoRoute(page, baseURL, `/notes/${noteId}`);
        await pageBody(page).getByTestId('note-detailed').getByTestId('note-reply').click();
        await waitForPostForm(page);
        // 既定では、返信のフォームに元のノートの注釈が入る (MkPostForm.vue)
        await expect(page.getByTestId('post-form-cw')).toHaveValue('');
      },
    },
    {
      key: 'rememberNoteVisibility', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page }) => {
        await openPostForm(page);
        await setPostText(page, runTag('pwremember'));
        await clickOnlyTestId(page, 'post-form-visibility');
        await clickOnlyTestId(page, 'visibility-home');
        await expect(page.getByTestId('post-form-visibility')).toContainText('Home');
        // 閉じるだけだと、公開範囲は下書きとして残り、設定に関係なく戻る。投稿して下書きを消す
        await submitPostForm(page);
        await expect(page.getByTestId('post-form-text')).toHaveCount(0);
        // 開き直したフォームが、前に選んだ公開範囲のまま (既定では Public に戻る)
        await openPostForm(page);
        await expect(page.getByTestId('post-form-visibility')).toContainText('Home');
      },
    },
    {
      key: 'enableQuickAddMfmFunction', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page }) => {
        await openPostForm(page);
        // 設定のメニューにもパレットのアイコン (テーマ) があるので、投稿フォームの中に絞る
        const form = page.getByTestId('post-form-text').locator('xpath=ancestor::*[.//*[@data-testid="post-form-submit"]][1]');
        await expect(form.locator('button:has(> i.ti-palette)')).toHaveCount(1);
      },
    },
    {
      key: 'defaultNoteVisibility', route: ROUTE, control: { kind: 'select', label: 'Home' }, value: 'home',
      pre: { rememberNoteVisibility: false },
      effect: async ({ page }) => {
        await openPostForm(page);
        await expect(page.getByTestId('post-form-visibility')).toContainText('Home');
      },
    },
    {
      key: 'defaultNoteLocalOnly', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      // 「公開範囲を記憶する」が入っている間は、既定の公開範囲の欄が操作できない
      pre: { rememberNoteVisibility: false },
      effect: async ({ page }) => {
        await openPostForm(page);
        await expect(page.getByTestId('post-form-local-only').locator('i.ti-rocket-off')).toHaveCount(1);
      },
    },
    // Notifications
    {
      key: 'useGroupedNotifications', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page, baseURL }) => {
        // まとめない通知の一覧は i/notifications を読む (既定は i/notifications-grouped)
        const plain = page.waitForRequest((r) => new URL(r.url()).pathname === '/api/i/notifications', { timeout: 15_000 });
        await gotoRoute(page, baseURL, '/my/notifications');
        await expectHappens(plain, 'まとめない通知の一覧 (i/notifications) を読まない');
      },
    },
    {
      key: 'notificationPosition', route: ROUTE, control: { kind: 'radio', label: 'Top left' }, value: 'leftTop',
      effect: async ({ page }) => {
        await showTestToasts(page, 1);
        const viewport = page.viewportSize()!;
        await expect.poll(async () => {
          const box = await page.getByTestId('toast-notification').boundingBox();
          return box != null && box.x + box.width / 2 < viewport.width / 2 && box.y + box.height / 2 < viewport.height / 2;
        }, { message: '通知が左上に出ない' }).toBe(true);
      },
    },
    {
      key: 'notificationStackAxis', route: ROUTE, control: { kind: 'radio', label: 'Vertical' }, value: 'vertical',
      effect: async ({ page }) => {
        await showTestToasts(page, 2);
        const toasts = page.getByTestId('toast-notification');
        // 縦に積むと、2 つの通知の左端がそろい、上下にずれる
        await expect.poll(async () => {
          const a = await toasts.nth(0).boundingBox();
          const b = await toasts.nth(1).boundingBox();
          return a != null && b != null && Math.abs(a.x - b.x) < 4 && Math.abs(a.y - b.y) > 20;
        }, { message: '通知が縦に並ばない' }).toBe(true);
      },
    },
    // Chat with user
    {
      key: 'chat.showSenderName', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, request, baseURL, user }) => {
        const name = runTag('PwSender');
        const other = await chatPartner(request, user, name);
        const text = runTag('pwmsg');
        await apiOk(request, 'chat/messages/create-to-user', { i: other.token, toUserId: user.id, text });
        await gotoRoute(page, baseURL, `/chat/user/${other.id}`);
        // 相手の名前はページの見出しにも出るので、メッセージ (アバターを含む XMessage の root) の中に絞る
        const message = pageBody(page).getByText(text, { exact: true }).locator('xpath=ancestor::div[.//*[contains(concat(" ", @class, " "), " _noSelect ")]][1]');
        await expect(message).toBeVisible({ timeout: 20_000 });
        await expect(message.getByText(name, { exact: true })).toBeVisible();
      },
    },
    {
      key: 'chat.sendOnEnter', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, request, baseURL, user }) => {
        const other = await chatPartner(request, user, runTag('PwPeer'));
        await gotoRoute(page, baseURL, `/chat/user/${other.id}`);
        const input = pageBody(page).getByPlaceholder('Enter message here');
        await input.fill(runTag('pwenter'));
        // 既定では Enter は改行で、送るのは Ctrl + Enter
        const sent = waitForApiBody(page, 'chat/messages/create-to-user', () => true, 10_000);
        await input.press('Enter');
        expect((await expectHappens(sent, 'Enter で送らない')).toUserId).toBe(other.id);
      },
    },
  ];
  for (const row of ROWS) test(prefTitle(row), coversPrefs(row.key), ({ page, request, baseURL }) => runPrefRow({ page, request, baseURL }, row));
});
