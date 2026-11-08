/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// カスタム絵文字の登録の申請 (#2934) を画面で通す (#4072)。利用者は /emoji-request でドライブの
// 画像を選んで申請し、自分の申請の一覧で状態を見る。管理者は /custom-emojis-manager の申請の
// タブで承認か却下 (理由付き) をする。利用者ページの申請のタブでは、上限に達した利用者の
// 枠をリセットする。
//
// 部品は data-testid で取る。申請・ロール・承認で増えた絵文字は、テストの終わりに消す。

import { type APIRequestContext, type Browser, expect, type Locator, type Page, test } from '@playwright/test';
import { assignRole, cleanupApi, expectApiOk, rootFixture, signinAdmin } from '../../../fixtures/admin_table';
import { callApi } from '../../../fixtures/api';
import { type Principal, randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { signinWithState } from '../../../fixtures/settings_table';
import { clickDriveItem, distinctPNG, uploadPNG } from '../../../fixtures/ui_drive';
import { clickMenuItem } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

/** Applies on /emoji-request with an image chosen from the drive. Returns the application id. */
async function applyInUi(page: Page, request: APIRequestContext, baseURL: string, user: Principal, name: string): Promise<string> {
  const file = await uploadPNG(request, baseURL, user.token, `${name}.png`, distinctPNG());
  await signinWithState(page, request, baseURL, user);
  await gotoRoute(page, baseURL, '/emoji-request');
  await page.getByTestId('emoji-application-choose-file').click();
  await clickMenuItem(page, 'From Drive');
  await clickDriveItem(page, 'drive-file', file.name);
  await page.getByTestId('modal-window-ok').click();
  await expect(page.getByTestId('emoji-application-preview'), '選んだ画像がプレビューに出ない').toBeVisible();
  await page.getByTestId('emoji-application-name').locator('input').fill(name);
  // ライセンスは申請に必須
  await page.getByTestId('emoji-application-license').locator('textarea').fill('pw license');
  const created = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/emoji-application/create', { timeout: 15_000 });
  await page.getByTestId('emoji-application-submit').click();
  const resp = await created;
  expect(resp.ok(), `emoji-application/create が失敗した (${resp.status()})`).toBe(true);
  const id = (await resp.json() as { id: string }).id;
  // 申請を受け付けた知らせのダイアログを閉じる。閉じきる前に次の確認のダイアログが開くと、
  // OK のボタンが 2 つ並ぶので、消えきるのを待つ
  await page.getByTestId('modal-dialog-ok').click();
  await expect(page.getByTestId('modal-dialog-ok')).toHaveCount(0);
  await page.getByTestId('emoji-request-tab-mine').click();
  await expect(page.locator(`[data-testid="emoji-application-mine"][data-application-id="${id}"]`), '自分の申請の一覧に出ない').toHaveAttribute('data-status', 'pending');
  return id;
}

/** Opens a folder (MkFolder) when it is closed. */
async function openFolder(folder: Locator): Promise<void> {
  if ((await folder.getAttribute('aria-expanded')) !== 'true') await folder.getByTestId('folder-header').first().click();
  await expect(folder).toHaveAttribute('aria-expanded', 'true');
}

/** A page of its own for root (the applicant keeps the test's page). */
async function adminPage(browser: Browser): Promise<Page> {
  const context = await browser.newContext({ ignoreHTTPSErrors: true, locale: 'en-US' });
  return context.newPage();
}

/** Opens the applications tab of /custom-emojis-manager as root and returns the application's folder. */
async function openReview(page: Page, baseURL: string, id: string) {
  await signinAdmin(page, baseURL, rootFixture());
  await gotoRoute(page, baseURL, '/custom-emojis-manager');
  await page.getByTestId('custom-emojis-tab-applications').click();
  const item = page.locator(`[data-testid="emoji-application-item"][data-application-id="${id}"]`);
  await expect(item, '審査の一覧に申請が出ない').toHaveCount(1, { timeout: 15_000 });
  return item;
}

async function statusOf(request: APIRequestContext, user: Principal, id: string): Promise<string | undefined> {
  const mine = await expectApiOk(request, 'emoji-application/list-mine', { i: user.token, limit: 100 }) as { id: string; status: string }[];
  return mine.find((a) => a.id === id)?.status;
}

test.describe('emoji application', () => {
  test.skip(isTsBackend, '絵文字の登録の申請は Elythia だけにある');
  test.setTimeout(90_000);

  test('apply from the drive, then cancel it', coversRoutes('/emoji-request'), async ({ page, request, baseURL }) => {
    const user = await signupUser(request, randomUsername('pwea'));
    const id = await applyInUi(page, request, baseURL!, user, `pwea${Date.now().toString(36)}`);
    const mine = page.locator(`[data-testid="emoji-application-mine"][data-application-id="${id}"]`);
    await openFolder(mine);
    await mine.getByTestId('emoji-application-cancel').click();
    await page.getByTestId('modal-dialog-ok').click();
    await expect.poll(() => statusOf(request, user, id), { message: '取り下げても審査待ちのまま' }).toBe('canceled');
  });

  test('approve: the emoji is registered and the applicant sees it approved', coversRoutes('/emoji-request', '/custom-emojis-manager'), async ({ browser, page, request, baseURL }) => {
    const user = await signupUser(request, randomUsername('pwea'));
    const name = `pwea${Date.now().toString(36)}`;
    const id = await applyInUi(page, request, baseURL!, user, name);
    const root = rootFixture();
    const admin = await adminPage(browser);
    try {
      const item = await openReview(admin, baseURL!, id);
      const approved = admin.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/emoji-application/approve', { timeout: 15_000 });
      // 名前が重ならなければ、確認を挟まずに承認する
      await item.getByTestId('emoji-application-approve').click();
      expect((await approved).ok(), 'admin/emoji-application/approve が失敗した').toBe(true);
      await expect.poll(() => statusOf(request, user, id), { message: '承認しても状態が変わらない' }).toBe('approved');
      const emoji = await expectApiOk(request, 'emoji', { name }) as { name: string };
      expect(emoji.name, '承認した絵文字が登録されない').toBe(name);

      // 申請した人の一覧でも承認済みになる
      await gotoRoute(page, baseURL!, '/emoji-request');
      await page.getByTestId('emoji-request-tab-mine').click();
      await expect(page.locator(`[data-testid="emoji-application-mine"][data-application-id="${id}"]`)).toHaveAttribute('data-status', 'approved');
    } finally {
      await admin.context().close();
      // 後片付けの失敗で、本来の失敗を覆い隠さない
      const listed = await callApi(request, 'admin/emoji/list', { i: root.token, query: name, limit: 10 });
      const emojis = listed.ok() ? await listed.json() as { id: string; name: string }[] : [];
      expect.soft(listed.ok(), '承認した絵文字を探せず、消せなかった').toBe(true);
      for (const e of emojis.filter((x) => x.name === name)) await cleanupApi(request, 'admin/emoji/delete', { i: root.token, id: e.id });
    }
  });

  test('reject with a reason: the applicant sees the reason', coversRoutes('/emoji-request', '/custom-emojis-manager'), async ({ browser, page, request, baseURL }) => {
    const user = await signupUser(request, randomUsername('pwea'));
    const id = await applyInUi(page, request, baseURL!, user, `pwea${Date.now().toString(36)}`);
    const reason = `pw reason ${Date.now().toString(36)}`;
    const admin = await adminPage(browser);
    try {
      const item = await openReview(admin, baseURL!, id);
      await item.getByTestId('emoji-application-reject-reason').locator('textarea').fill(reason);
      const rejected = admin.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/emoji-application/reject', { timeout: 15_000 });
      // 却下は確認を挟まない (理由が空のときだけ知らせが出る)
      await item.getByTestId('emoji-application-reject').click();
      expect((await rejected).ok(), 'admin/emoji-application/reject が失敗した').toBe(true);
    } finally {
      await admin.context().close();
    }
    await expect.poll(() => statusOf(request, user, id), { message: '却下しても状態が変わらない' }).toBe('rejected');

    // 申請した人の一覧で、却下の理由が読める (却下された申請は開いた状態で出る)
    await gotoRoute(page, baseURL!, '/emoji-request');
    await page.getByTestId('emoji-request-tab-mine').click();
    const mine = page.locator(`[data-testid="emoji-application-mine"][data-application-id="${id}"]`);
    await expect(mine).toHaveAttribute('data-status', 'rejected');
    await expect(mine, '却下の理由が出ない').toContainText(reason);
  });

  test('reset the quota of a user who reached the limit', coversRoutes('/admin/user/:userId'), async ({ page, request, baseURL }) => {
    const root = rootFixture();
    const user = await signupUser(request, randomUsername('pwea'));
    const roleId = await assignRole(request, root, user.id, { policies: { emojiApplicationMaxPerDay: 1 } });
    const applied: string[] = [];
    try {
      const file = await uploadPNG(request, baseURL!, user.token, 'pw.png', distinctPNG());
      const first = await expectApiOk(request, 'emoji-application/create', { i: user.token, kind: 'own', name: `pwea${Date.now().toString(36)}`, license: 'pw', fileId: file.id }) as { id: string };
      applied.push(first.id);
      // 上限に達している (2 件目は断られる)
      const second = await uploadPNG(request, baseURL!, user.token, 'pw2.png', distinctPNG());
      const refused = await request.post(`${baseURL}/api/emoji-application/create`, { data: { i: user.token, kind: 'own', name: `pweb${Date.now().toString(36)}`, license: 'pw', fileId: second.id }, failOnStatusCode: false });
      expect(refused.status(), '上限に達する前提が成り立たない').toBe(429);

      await signinAdmin(page, baseURL!, root);
      await gotoRoute(page, baseURL!, `/admin/user/${user.id}`);
      await page.getByTestId('admin-user-tab-emoji-applications').click();
      await page.getByTestId('emoji-application-reset-quota').click();
      await page.getByTestId('modal-dialog-input').locator('input').fill('pw reset');
      const reset = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/emoji-application/reset-user-quota', { timeout: 15_000 });
      await page.getByTestId('modal-dialog-ok').click();
      expect((await reset).ok(), 'admin/emoji-application/reset-user-quota が失敗した').toBe(true);

      // リセットの後は、同じ日でももう 1 件申請できる
      const again = await request.post(`${baseURL}/api/emoji-application/create`, { data: { i: user.token, kind: 'own', name: `pwec${Date.now().toString(36)}`, license: 'pw', fileId: second.id }, failOnStatusCode: false });
      expect(again.ok(), `リセットの後も申請できない (${again.status()})`).toBe(true);
      applied.push((await again.json() as { id: string }).id);
    } finally {
      for (const applicationId of applied) await cleanupApi(request, 'emoji-application/cancel', { i: user.token, applicationId });
      await cleanupApi(request, 'admin/roles/delete', { i: root.token, roleId });
    }
  });
});
