/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// オブジェクトストレージの設定の画面の、ローカルのファイルの移行の部品 (#4184) を通す。
// ボタンを押せる・押せない条件は status の件数と実行中かで決まるので、status の応答を差し替えて
// 決まった形で確かめる。開始の API も差し替え、送ったかどうかと本文だけを見る (実際に流す経路は
// api/drive_storage_migration の spec が MinIO で通す)。
//
// ローカルを消す操作は DB のバックアップと同じ再認証を求めるので、2FA の無い管理者は押せない。
// root に 2FA を付けると他の spec の入り方が変わるので、2FA と管理者のロールは、このテストで
// 作った利用者に付ける。

import { expect, test, type APIRequestContext, type Page, type Route } from '@playwright/test';
import { authenticator } from 'otplib';
import { assignRole, cleanupApi, expectApiOk, rootFixture, signinAdmin } from '../../../fixtures/admin_table';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { signinWithState } from '../../../fixtures/settings_table';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

type Status = {
  objectStorageEnabled: boolean;
  running: 'migrate' | 'verify' | 'purge' | 'restore' | 'cleanup' | null;
  localFiles: number;
  migratedFiles: number;
  failedFiles: number;
  failures: { fileId: string; error: string; failedAt: string }[];
  verifiedFiles: number;
  verifyFailedFiles: number;
  verifyFailures: { fileId: string; error: string; failedAt: string }[];
  purgeableFiles: number;
  localDeletedFiles: number;
  purgeFailedFiles: number;
  purgeFailures: { fileId: string; error: string; failedAt: string }[];
  restoreFailedFiles: number;
  restoreFailures: { fileId: string; error: string; failedAt: string }[];
  restoredCopies: number;
  cleanupFailedCopies: number;
  cleanupFailures: { key: string; fileId: string; error: string; failedAt: string }[];
};

function status(over: Partial<Status>): Status {
  return {
    objectStorageEnabled: true, running: null, localFiles: 0, migratedFiles: 0, failedFiles: 0, failures: [],
    verifiedFiles: 0, verifyFailedFiles: 0, verifyFailures: [], purgeableFiles: 0, localDeletedFiles: 0,
    purgeFailedFiles: 0, purgeFailures: [], restoreFailedFiles: 0, restoreFailures: [],
    restoredCopies: 0, cleanupFailedCopies: 0, cleanupFailures: [], ...over,
  };
}

const START = {
  migrate: 'admin/drive/object-storage-migration/start',
  verify: 'admin/drive/object-storage-migration/verify',
  purge: 'admin/drive/object-storage-migration/purge-local',
  restore: 'admin/drive/object-storage-migration/restore',
  cleanup: 'admin/drive/object-storage-migration/cleanup-copies',
} as const;

/**
 * Serves `body` for the status and 204 for the starts, and returns the
 * bodies of the starts sent, by endpoint.
 */
async function stub(page: Page, body: Status): Promise<Record<string, unknown[]>> {
  const sent: Record<string, unknown[]> = {};
  await page.route('**/api/admin/drive/object-storage-migration/status', (route) => route.fulfill({ json: body }));
  for (const endpoint of Object.values(START)) {
    await page.route(`**/api/${endpoint}`, (route) => {
      (sent[endpoint] ??= []).push(route.request().postDataJSON());
      return route.fulfill({ status: 204 });
    });
  }
  return sent;
}

function button(page: Page, name: keyof typeof START) {
  return page.getByTestId(`object-storage-migration-${name}`);
}

/** Waits until `sent` holds `n` requests to endpoint. */
async function expectSent(sent: Record<string, unknown[]>, endpoint: string, n: number): Promise<void> {
  // request イベントと route の handler のどちらが先に走るかは決まっていないので、待って読む
  await expect.poll(() => sent[endpoint]?.length ?? 0).toBe(n);
}

/** Opens the folder of the migration panel. */
async function openPanel(page: Page): Promise<void> {
  const panel = page.getByTestId('object-storage-migration');
  if (!(await panel.isVisible())) {
    await page.getByText('Moving local files').click();
  }
  await expect(panel).toBeVisible();
}

test.describe('admin: object storage migration panel', () => {
  test.skip(isTsBackend, '移行の部品は Elythia だけにある');
  test.setTimeout(90_000);

  // 時間切れで落ちても、作ったロールを残さない (test の中の finally は時間切れで走らない)
  const roleIds: string[] = [];
  test.afterEach(async ({ request }) => {
    const root = rootFixture();
    while (roleIds.length > 0) {
      await cleanupApi(request, 'admin/roles/delete', { i: root.token, roleId: roleIds.pop()! });
    }
  });

  /** Signs up an administrator with TOTP registered, and returns it with the secret. */
  async function twoFactorAdmin(request: APIRequestContext) {
    const admin = await signupUser(request, randomUsername('pwosm'), DEFAULT_TEST_PASSWORD);
    roleIds.push(await assignRole(request, rootFixture(), admin.id, { isAdministrator: true }));
    const { secret } = await expectApiOk(request, 'i/2fa/register', { i: admin.token, password: DEFAULT_TEST_PASSWORD }) as { secret: string };
    await expectApiOk(request, 'i/2fa/done', { i: admin.token, token: authenticator.generate(secret) });
    return { admin, secret };
  }

  /** Signs admin in on page and opens the panel. */
  async function openAs(page: Page, request: APIRequestContext, baseURL: string, admin: Awaited<ReturnType<typeof signupUser>>): Promise<void> {
    await page.addInitScript(() => {
      window.localStorage.setItem('modifiedVersionMustProminentlyOfferInAgplV3Section13Read', 'true');
    });
    await signinWithState(page, request, baseURL, admin);
    await gotoRoute(page, baseURL, '/admin/object-storage');
    await openPanel(page);
  }

  for (const tc of [
    { name: 'object storage is not configured', body: status({ objectStorageEnabled: false, localFiles: 3, migratedFiles: 3, purgeableFiles: 3, restoredCopies: 3 }), enabled: [] },
    { name: 'an operation runs', body: status({ running: 'verify', localFiles: 3, migratedFiles: 3, purgeableFiles: 3, restoredCopies: 3 }), enabled: [] },
    { name: 'only local files', body: status({ localFiles: 3 }), enabled: ['migrate'] },
    { name: 'everything moved', body: status({ migratedFiles: 3 }), enabled: ['verify', 'restore'] },
    { name: 'copies left after moving back', body: status({ localFiles: 3, restoredCopies: 3 }), enabled: ['migrate', 'cleanup'] },
  ] as const) {
    test(`buttons: ${tc.name}`, coversRoutes('/admin/object-storage'), async ({ page, baseURL }) => {
      await stub(page, tc.body);
      await signinAdmin(page, baseURL!, rootFixture());
      await gotoRoute(page, baseURL!, '/admin/object-storage');
      await openPanel(page);
      for (const name of Object.keys(START) as (keyof typeof START)[]) {
        if ((tc.enabled as readonly string[]).includes(name)) {
          await expect(button(page, name), name).toBeEnabled();
        } else {
          await expect(button(page, name), name).toBeDisabled();
        }
      }
    });
  }

  test('an admin without 2FA cannot delete the local copies', coversRoutes('/admin/object-storage'), async ({ page, baseURL }) => {
    await stub(page, status({ migratedFiles: 3, verifiedFiles: 3, purgeableFiles: 3 }));
    await signinAdmin(page, baseURL!, rootFixture());
    await gotoRoute(page, baseURL!, '/admin/object-storage');
    await openPanel(page);
    await expect(button(page, 'purge')).toBeDisabled();
    await expect(page.getByText('To delete the local copies, register two-factor authentication')).toBeVisible();
  });

  test('moving asks first and warns about going back to the TS version', coversRoutes('/admin/object-storage'), async ({ page, baseURL }) => {
    const sent = await stub(page, status({ localFiles: 3 }));
    await signinAdmin(page, baseURL!, rootFixture());
    await gotoRoute(page, baseURL!, '/admin/object-storage');
    await openPanel(page);

    await button(page, 'migrate').click();
    await expect(page.getByText('Moved files cannot be read once you go back to the TS version')).toBeVisible();
    await page.getByTestId('modal-dialog-cancel').click();
    await expect(page.getByText('Moved files cannot be read once you go back to the TS version')).toHaveCount(0);
    expect(sent[START.migrate], 'the move started although it was cancelled').toBeUndefined();

    await button(page, 'migrate').click();
    const started = page.waitForRequest((r) => new URL(r.url()).pathname === `/api/${START.migrate}`);
    await page.getByTestId('modal-dialog-ok').click();
    await started;
    await expectSent(sent, START.migrate, 1);
    // ボタンが戻るのは応答の後なので、取り消した側が送っていれば、ここまでに数えられている
    await expect(button(page, 'migrate')).toBeEnabled();
    expect(sent[START.migrate], 'the move started although it was cancelled').toHaveLength(1);
  });

  test('deleting the copies left asks first and lists the failures by key', coversRoutes('/admin/object-storage'), async ({ page, baseURL }) => {
    const sent = await stub(page, status({
      localFiles: 3, restoredCopies: 3, cleanupFailedCopies: 1,
      cleanupFailures: [{ key: 'pw-copy-key', fileId: 'pw-copy-file', error: 'settings changed', failedAt: '2026-11-15T00:00:00.000Z' }],
    }));
    await signinAdmin(page, baseURL!, rootFixture());
    await gotoRoute(page, baseURL!, '/admin/object-storage');
    await openPanel(page);

    // 写しの失敗はキーで出す (ファイルは消されていることがある)
    await page.getByText('Copies that could not be deleted').click();
    await expect(page.getByText('pw-copy-key')).toBeVisible();
    await expect(page.getByText('pw-copy-file')).toHaveCount(0);

    const confirm = 'This deletes the copies the files moved back left in object storage';
    await button(page, 'cleanup').click();
    await expect(page.getByText(confirm)).toBeVisible();
    await page.getByTestId('modal-dialog-cancel').click();
    await expect(page.getByText(confirm)).toHaveCount(0);

    await button(page, 'cleanup').click();
    const started = page.waitForRequest((r) => new URL(r.url()).pathname === `/api/${START.cleanup}`);
    await page.getByTestId('modal-dialog-ok').click();
    await started;
    await expectSent(sent, START.cleanup, 1);
    // ボタンが戻るのは応答の後なので、取り消した側が送っていれば、ここまでに数えられている
    await expect(button(page, 'cleanup')).toBeEnabled();
    expect(sent[START.cleanup], 'the cleanup started although it was cancelled').toHaveLength(1);
  });

  test('a refused start shows why', coversRoutes('/admin/object-storage'), async ({ page, baseURL }) => {
    await stub(page, status({ localFiles: 3 }));
    // 後に登録した route が先に当たる
    await page.route(`**/api/${START.migrate}`, (route) => route.fulfill({
      status: 409,
      json: { error: { message: 'running', code: 'STORAGE_MIGRATION_RUNNING', id: '5f833225-1667-48ac-bc86-10ca1dfd6817' } },
    }));
    await signinAdmin(page, baseURL!, rootFixture());
    await gotoRoute(page, baseURL!, '/admin/object-storage');
    await openPanel(page);
    await button(page, 'migrate').click();
    await page.getByTestId('modal-dialog-ok').click();
    await expect(page.getByText('Another operation is running. Try again after it ends.')).toBeVisible();
  });

  // 本家は、知らない口への POST に fastify の既定の 404 (error が文字列) を、GET に
  // UNKNOWN_API_ENDPOINT を返す
  for (const tc of [
    { name: 'fastify 404', body: { message: 'Route POST:/api/admin/drive/object-storage-migration/status not found', error: 'Not Found', statusCode: 404 } },
    { name: 'UNKNOWN_API_ENDPOINT', body: { error: { message: 'Unknown API endpoint.', code: 'UNKNOWN_API_ENDPOINT', id: '2ca3b769-540a-4f08-9dd5-b5a825b6d0f1' } } },
  ]) {
    test(`the panel hides on a backend without the API: ${tc.name}`, coversRoutes('/admin/object-storage'), async ({ page, baseURL }) => {
      await page.route('**/api/admin/drive/object-storage-migration/status', (route) => route.fulfill({ status: 404, json: tc.body }));
      await signinAdmin(page, baseURL!, rootFixture());
      const answered = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/drive/object-storage-migration/status');
      await gotoRoute(page, baseURL!, '/admin/object-storage');
      await answered;
      // 同じ画面の本家の部品が出てから見る
      await expect(page.getByText('Use object storage').first()).toBeVisible();
      await expect(page.getByText('Moving local files', { exact: true })).toHaveCount(0);
    });
  }

  for (const tc of [
    { name: '500', fail: (route: Route) => route.fulfill({ status: 500, json: { error: { message: 'internal', code: 'INTERNAL_ERROR', id: '5d37dbcb-891e-41ca-a3d6-e690c97775ac' } } }) },
    { name: 'HTML 502', fail: (route: Route) => route.fulfill({ status: 502, contentType: 'text/html', body: '<html><body>502 Bad Gateway</body></html>' }) },
    { name: 'network error', fail: (route: Route) => route.abort('connectionreset') },
  ]) {
    test(`the panel stays and reads again after a transient failure: ${tc.name}`, coversRoutes('/admin/object-storage'), async ({ page, baseURL }) => {
      await stub(page, status({ localFiles: 3 }));
      let failed = false;
      await page.route('**/api/admin/drive/object-storage-migration/status', (route) => {
        if (failed) return route.fallback();
        failed = true;
        return tc.fail(route);
      });
      await signinAdmin(page, baseURL!, rootFixture());
      await gotoRoute(page, baseURL!, '/admin/object-storage');
      await openPanel(page);
      // 10 秒ごとの読み直しで件数が出て、移すボタンが押せるようになる
      await expect(button(page, 'migrate')).toBeEnabled({ timeout: 30_000 });
    });
  }

  test('a failed read after a start keeps the buttons off until it reads again', coversRoutes('/admin/object-storage'), async ({ page, baseURL }) => {
    await stub(page, status({ localFiles: 3 }));
    let reads = 0;
    // 開いたときの 1 回は通し、開始の後の読み直しと、続く poller の最初の 1 回 (start は
    // すぐに読む) を落とす。次に読めるのは 10 秒後になる
    await page.route('**/api/admin/drive/object-storage-migration/status', (route) => {
      reads++;
      if (reads !== 2 && reads !== 3) return route.fallback();
      return route.fulfill({ status: 500, json: { error: { message: 'internal', code: 'INTERNAL_ERROR', id: '5d37dbcb-891e-41ca-a3d6-e690c97775ac' } } });
    });
    await signinAdmin(page, baseURL!, rootFixture());
    await gotoRoute(page, baseURL!, '/admin/object-storage');
    await openPanel(page);
    await button(page, 'migrate').click();
    const failedRead = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/drive/object-storage-migration/status' && r.status() === 500);
    await page.getByTestId('modal-dialog-ok').click();
    await failedRead;
    // 古い status を捨てたことを、件数の表示が消えたことで見る (ボタンは busy の間も押せない
    // ので、ボタンだけでは古い status が残ったのか区別できない)
    await expect(page.getByTestId('object-storage-migration-local')).toHaveCount(0);
    await expect(button(page, 'migrate')).toBeDisabled();
    await expect(button(page, 'migrate')).toBeEnabled({ timeout: 30_000 });
  });

  test('an admin with 2FA cannot delete the local copies before verifying', coversRoutes('/admin/object-storage'), async ({ page, request, baseURL }) => {
    const { admin } = await twoFactorAdmin(request);
    await stub(page, status({ migratedFiles: 3, verifiedFiles: 0, purgeableFiles: 0 }));
    await openAs(page, request, baseURL!, admin);
    await expect(button(page, 'verify')).toBeEnabled();
    await expect(button(page, 'purge')).toBeDisabled();
  });

  test('deleting the local copies warns and re-authenticates', coversRoutes('/admin/object-storage'), async ({ page, request, baseURL }) => {
    const { admin, secret } = await twoFactorAdmin(request);
    const sent = await stub(page, status({ migratedFiles: 3, verifiedFiles: 3, purgeableFiles: 3 }));
    await openAs(page, request, baseURL!, admin);

    // 再認証のダイアログを閉じたら送らない
    await button(page, 'purge').click();
    await expect(page.getByText('Deleted local copies cannot be recovered')).toBeVisible();
    await page.getByTestId('modal-dialog-ok').click();
    await expect(page.getByTestId('password-dialog-password')).toBeVisible();
    await page.getByTestId('modal-window-close').click();
    await expect(page.getByTestId('password-dialog-password')).toHaveCount(0);
    await expect(button(page, 'purge')).toBeEnabled();

    await button(page, 'purge').click();
    await page.getByTestId('modal-dialog-ok').click();
    await page.getByTestId('password-dialog-password').locator('input').fill(DEFAULT_TEST_PASSWORD);
    // 登録で使ったコードは使用済みになる。サーバーは前後 1 ステップを許すので、次のステップの
    // コードを作れば待たずに通る (ここでは差し替えた口へ送るだけだが、本物と同じ形にする)
    const next = authenticator.clone({ epoch: Date.now() + 30_000 }).generate(secret);
    await page.getByTestId('password-dialog-token').locator('input').fill(next);
    const started = page.waitForRequest((r) => new URL(r.url()).pathname === `/api/${START.purge}`);
    await page.getByTestId('password-dialog-submit').click();
    await started;
    await expectSent(sent, START.purge, 1);
    // ボタンが戻るのは応答の後なので、閉じた側が送っていれば、ここまでに数えられている
    await expect(button(page, 'purge')).toBeEnabled();
    expect(sent[START.purge], 'the purge was sent although the re-authentication was cancelled').toHaveLength(1);
    expect(sent[START.purge][0]).toMatchObject({ password: DEFAULT_TEST_PASSWORD, token: next });
  });
});
