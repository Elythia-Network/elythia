/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 承認制の登録 (#2554 / #2568) を画面で通す (#4072)。申請者は未ログインの画面で申請してコードを
// 受け取り、管理者は別の画面の /admin/signup-applications で承認か却下をする。申請者はコードで
// 戻ってきて、承認なら登録し、却下ならその表示を見る。
//
// 部品は data-testid で取る。文言は日本語の直書きで、#4070 で i18n に移ると変わるため。
// 登録の受け付け方と申請フォームは instance meta なので、始める前の値を控えて終わったら戻す
// (この spec は admin/update-meta を書くので meta の project で流れる)。

import { type Browser, expect, type Page, test } from '@playwright/test';
import { expectApiOk, fieldByLabel, rootFixture, signinAdmin } from '../../../fixtures/admin_table';
import { callApi } from '../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

const KEYS = ['registrationClosed', 'approvalRequiredForSignup', 'disableRegistration', 'signupApplicationForm'] as const;
const FIELD = 'pw reason';

/** A page signed out, in a context of its own (the applicant). */
async function visitorPage(browser: Browser): Promise<Page> {
  const context = await browser.newContext({ ignoreHTTPSErrors: true, locale: 'en-US' });
  const page = await context.newPage();
  await page.addInitScript(() => {
    window.localStorage.setItem('modifiedVersionMustProminentlyOfferInAgplV3Section13Read', 'true');
  });
  return page;
}

/** Applies on /signup-application with `answer` and returns the claim code shown once. */
async function apply(page: Page, baseURL: string, answer: string): Promise<string> {
  await gotoRoute(page, baseURL, '/signup-application');
  await (await fieldByLabel(page, FIELD)).locator('input').fill(answer);
  // フォームを開いた直後は送れない (form-token の待ち)。押せるようになるまで待つ
  const button = page.getByTestId('signup-application-apply');
  await expect(button).toBeEnabled({ timeout: 15_000 });
  await button.click();
  const code = page.getByTestId('signup-application-issued-code').locator('input');
  await expect(code, 'クレームコードが出ない').not.toHaveValue('', { timeout: 15_000 });
  const value = await code.inputValue();
  await page.getByTestId('signup-application-ack').click();
  await expect(page.getByTestId('signup-application-status')).toHaveAttribute('data-status', 'pending');
  return value;
}

/** Looks the application up by its claim code, as an applicant coming back. */
async function lookup(page: Page, baseURL: string, code: string): Promise<void> {
  await gotoRoute(page, baseURL, '/signup-application');
  const folder = page.getByTestId('signup-application-lookup-folder');
  if ((await folder.getAttribute('aria-expanded')) !== 'true') await folder.getByTestId('folder-header').first().click();
  await page.getByTestId('signup-application-lookup-code').locator('input').fill(code);
  await page.getByTestId('signup-application-lookup').click();
}

/** Approves or rejects the application with `answer` on /admin/signup-applications, as root. */
async function review(page: Page, baseURL: string, answer: string, action: 'approve' | 'reject'): Promise<void> {
  const root = rootFixture();
  const list = await expectApiOk(page.request, 'admin/signup-application/list', { i: root.token, filter: 'pending', limit: 100 }) as { applications: { id: string; answers: { value: string }[] }[] };
  const app = list.applications.find((a) => a.answers.some((x) => x.value === answer));
  expect(app, '申請が審査待ちの一覧に無い').toBeTruthy();
  await signinAdmin(page, baseURL, root);
  await gotoRoute(page, baseURL, '/admin/signup-applications');
  const item = page.locator(`[data-testid="signup-application-item"][data-application-id="${app!.id}"]`);
  await expect(item).toHaveCount(1);
  const done = page.waitForResponse((r) => new URL(r.url()).pathname === `/api/admin/signup-application/${action}`, { timeout: 15_000 });
  await item.getByTestId(`signup-application-${action}`).click();
  await page.getByTestId('modal-dialog-ok').click();
  expect((await done).ok(), `admin/signup-application/${action} が失敗した`).toBe(true);
}

test.describe('signup application (approval required)', () => {
  test.skip(isTsBackend, '承認制の登録は Elythia だけにある');
  test.setTimeout(120_000);

  let original: Record<string, unknown> | undefined;
  test.beforeEach(async ({ request }) => {
    // 前のテストの値が残ると、控える前に落ちたときに afterEach がその値で書き戻す
    original = undefined;
    const root = rootFixture();
    const meta = await expectApiOk(request, 'admin/meta', { i: root.token });
    original = Object.fromEntries(KEYS.map((k) => [k, meta[k]]));
    await expectApiOk(request, 'admin/update-meta', {
      i: root.token, registrationClosed: false, approvalRequiredForSignup: true, disableRegistration: false,
      signupApplicationForm: [{ label: FIELD, type: 'text', required: false }],
    });
  });
  test.afterEach(async ({ request }) => {
    if (original === undefined) return;
    const restored = await callApi(request, 'admin/update-meta', { i: rootFixture().token, ...original });
    expect.soft(restored.ok(), '登録の受け付け方を元に戻せなかった').toBe(true);
  });

  test('apply → approve → sign up with the claim code', coversRoutes('/signup-application', '/admin/signup-applications'), async ({ browser, page, baseURL, request }) => {
    const visitor = await visitorPage(browser);
    try {
      const answer = `pw-approve-${Date.now().toString(36)}`;
      const code = await apply(visitor, baseURL!, answer);
      await review(page, baseURL!, answer, 'approve');

      await lookup(visitor, baseURL!, code);
      await expect(visitor.getByTestId('signup-application-status')).toHaveAttribute('data-status', 'approved');
      const username = randomUsername('pwapp');
      await visitor.getByTestId('signup-application-username').locator('input').fill(username);
      await visitor.getByTestId('signup-application-password').locator('input').fill(DEFAULT_TEST_PASSWORD);
      const signedUp = visitor.waitForResponse((r) => new URL(r.url()).pathname === '/api/signup-application/register', { timeout: 15_000 });
      await visitor.getByTestId('signup-application-register').click();
      expect((await signedUp).ok(), '承認された申請で登録できない').toBe(true);
      const shown = await callApi(request, 'users/show', { username });
      expect(shown.ok(), '登録したアカウントが無い').toBe(true);

      // 登録を済ませた申請は、もう一度照会すると「完了」になる
      const again = await visitorPage(browser);
      try {
        await lookup(again, baseURL!, code);
        await expect(again.getByTestId('signup-application-status')).toHaveAttribute('data-status', 'completed');
      } finally {
        await again.context().close();
      }
    } finally {
      await visitor.context().close();
    }
  });

  test('apply → reject → the applicant sees it was not approved', coversRoutes('/signup-application', '/admin/signup-applications'), async ({ browser, page, baseURL }) => {
    const visitor = await visitorPage(browser);
    try {
      const answer = `pw-reject-${Date.now().toString(36)}`;
      const code = await apply(visitor, baseURL!, answer);
      await review(page, baseURL!, answer, 'reject');
      await lookup(visitor, baseURL!, code);
      await expect(visitor.getByTestId('signup-application-status')).toHaveAttribute('data-status', 'rejected');
    } finally {
      await visitor.context().close();
    }
  });
});
