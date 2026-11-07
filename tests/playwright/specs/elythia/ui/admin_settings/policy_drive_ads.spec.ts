/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ロールのポリシーの表 (#4066): ドライブ・アバター・広告・ウォーターマーク。

import { type APIRequestContext, expect, test } from '@playwright/test';
import { type AdminRowContext, cleanupApi, coversPolicies, expectApiError, expectApiOk, type PolicyRow, policyTitle, runPolicyRow } from '../../../../fixtures/admin_table';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { waitForMountedPage } from '../../../../fixtures/route_check';
import { openAllFolders, signinWithState } from '../../../../fixtures/settings_table';
import { distinctPNG, uploadPNG } from '../../../../fixtures/ui_drive';
import { coversRoutes, gotoRoute } from '../../../../fixtures/ui_route';
import { pageBody } from '../../../../fixtures/ui_timeline';

/** Uploads a text file of `bytes` bytes (text/plain is in the default uploadable file types). */
function uploadText(request: APIRequestContext, baseURL: string, token: string, bytes: number) {
  return request.post(`${baseURL}/api/drive/files/create`, {
    multipart: { i: token, file: { name: 'pw.txt', mimeType: 'text/plain', buffer: Buffer.alloc(bytes, 'a') } },
    failOnStatusCode: false,
  });
}

async function expectUploadError({ request, baseURL, user }: AdminRowContext, bytes: number, code: string): Promise<void> {
  const resp = await uploadText(request, baseURL, user.token, bytes);
  const json = await resp.json().catch(() => ({})) as { error?: { code?: string } };
  expect(json.error?.code, `アップロードが ${code} で拒否されない (status ${resp.status()})`).toBe(code);
}

const AD_IMAGE = 'https://remote.example/pw-ad.png';

const CHUNKED_SKIP = '分割アップロードはオブジェクトストレージ (S3) のマルチパートを使う。S3 の無いこのスタックでは、許可されていても CHUNKED_UPLOAD_UNAVAILABLE で拒否され、区別できない';

const ROWS: PolicyRow[] = [
  { key: 'canUseChunkedUpload', control: { kind: 'switch', on: false }, value: false, effect: { skip: CHUNKED_SKIP } },
  { key: 'chunkedUploadMaxConcurrentSessions', control: { kind: 'number', value: 1 }, value: 1, effect: { skip: CHUNKED_SKIP } },
  { key: 'chunkedUploadMaxPendingMb', control: { kind: 'number', value: 10 }, value: 10, effect: { skip: CHUNKED_SKIP } },
  {
    key: 'driveCapacityMb', control: { kind: 'number', value: 1 }, value: 1,
    effect: (ctx) => expectUploadError(ctx, 1_500_000, 'NO_FREE_SPACE'),
  },
  {
    key: 'maxFileSizeMb', control: { kind: 'number', value: 1 }, value: 1,
    effect: (ctx) => expectUploadError(ctx, 1_500_000, 'MAX_FILE_SIZE_EXCEEDED'),
  },
  {
    key: 'uploadableFileTypes', control: { kind: 'textarea', lines: ['image/*'] }, value: ['image/*'],
    effect: (ctx) => expectUploadError(ctx, 100, 'UNALLOWED_FILE_TYPE'),
  },
  {
    key: 'alwaysMarkNsfw', control: { kind: 'switch', on: true }, value: true,
    effect: async ({ request, baseURL, user }) => {
      const file = await uploadPNG(request, baseURL, user.token, 'pw.png', distinctPNG()) as { isSensitive?: boolean };
      expect(file.isSensitive, 'アップロードしたファイルがセンシティブにならない').toBe(true);
    },
  },
  {
    key: 'canUpdateBioMedia', control: { kind: 'switch', on: false }, value: false,
    effect: async ({ request, baseURL, user }) => {
      const file = await uploadPNG(request, baseURL, user.token, 'pw.png', distinctPNG());
      await expectApiError(request, 'i/update', { i: user.token, avatarId: file.id }, 'RESTRICTED_BY_ROLE');
    },
  },
  {
    key: 'avatarDecorationLimit', control: { kind: 'number', value: 0 }, value: 0,
    setup: ({ request, baseURL, root }) => expectApiOk(request, 'admin/avatar-decorations/create', { i: root.token, name: 'pw deco', description: '', url: `${baseURL}/static-assets/badges/info.png` }),
    cleanup: async ({ request, root, data }) => { if (data) await cleanupApi(request, 'admin/avatar-decorations/delete', { i: root.token, id: (data as { id: string }).id }); },
    effect: ({ request, user, data }) => expectApiError(request, 'i/update', { i: user.token, avatarDecorations: [{ id: (data as { id: string }).id }] }, 'TOO_MANY_AVATAR_DECORATIONS'),
  },
  {
    key: 'canUseEmojiAsAvatarDecoration', control: { kind: 'switch', on: false }, value: false,
    setup: async ({ request, baseURL, root }) => {
      const file = await uploadPNG(request, baseURL, root.token, 'pw-emoji.png', distinctPNG());
      const name = `pwdeco${Date.now().toString(36)}`;
      const emoji = await expectApiOk(request, 'admin/emoji/add', { i: root.token, name, fileId: file.id });
      return { ...emoji, fileId: file.id };
    },
    cleanup: async ({ request, root, data }) => {
      if (data == null) return;
      const d = data as { id: string; fileId: string };
      await cleanupApi(request, 'admin/emoji/delete', { i: root.token, id: d.id });
      await cleanupApi(request, 'drive/files/delete', { i: root.token, fileId: d.fileId });
    },
    effect: ({ request, user, data }) => {
      const emoji = data as { id: string; name: string };
      return expectApiError(request, 'i/update', { i: user.token, avatarDecorations: [{ id: emoji.id, emojiName: emoji.name }] }, 'RESTRICTED_BY_ROLE');
    },
  },
  {
    key: 'watermarkAvailable', control: { kind: 'switch', on: false }, value: false,
    // backend は読まず、frontend が $i.policies で読む。ドライブの設定にウォーターマークの枠が出なくなる
    effect: async ({ page, request, baseURL, user }) => {
      await signinWithState(page, request, baseURL, user);
      await gotoRoute(page, baseURL, '/settings/drive');
      await waitForMountedPage(page);
      await openAllFolders(page);
      // 枠が無いことは、描画の前でも成り立つので、同じ画面の別の枠が出てから数える
      const header = (text: string) => pageBody(page).getByTestId('folder-header').filter({ has: page.getByText(text, { exact: true }) });
      await expect(header('Frame'), '同じ節の「Frame」の枠').toHaveCount(1);
      expect(await header('Watermark').count(), 'ウォーターマークの枠が出る').toBe(0);
    },
  },
  {
    key: 'canHideAds', control: { kind: 'switch', on: true }, value: true,
    // backend は読まず、frontend の MkAd が $i.policies で読む。/chat の上の広告が出なくなる
    setup: ({ request, root }) => expectApiOk(request, 'admin/ad/create', {
      i: root.token, url: 'https://remote.example/pw-ad', memo: 'pw canHideAds', place: 'horizontal', priority: 'middle', ratio: 1,
      startsAt: Date.now() - 60_000, expiresAt: Date.now() + 3_600_000, imageUrl: AD_IMAGE, dayOfWeek: 0,
    }),
    cleanup: async ({ request, root, data }) => { if (data) await cleanupApi(request, 'admin/ad/delete', { i: root.token, id: (data as { id: string }).id }); },
    effect: async ({ page, request, baseURL, user }) => {
      // 広告が無いことは描画の前でも成り立つので、先にロールの無い利用者で広告が出ることを確かめ、
      // 同じだけ待ってから数える
      const control = await signupUser(request, randomUsername('pwadc'));
      await signinWithState(page, request, baseURL, control);
      await gotoRoute(page, baseURL, '/chat');
      await waitForMountedPage(page);
      const started = Date.now();
      await expect(pageBody(page).locator(`img[src="${AD_IMAGE}"]`), 'ロールの無い利用者に広告が出ない (対照)').toHaveCount(1);
      const shown = Date.now() - started;
      await signinWithState(page, request, baseURL, user);
      await gotoRoute(page, baseURL, '/chat');
      await waitForMountedPage(page);
      await page.waitForTimeout(Math.max(1_500, shown * 2));
      expect(await pageBody(page).locator(`img[src="${AD_IMAGE}"]`).count(), '広告が出る').toBe(0);
    },
  },
];

test.describe('admin: role policies (drive, avatar, ads, watermark)', () => {
  test.setTimeout(60_000);
  for (const row of ROWS) {
    test(policyTitle(row), { annotation: [...coversRoutes('/admin/roles/:id/edit').annotation, ...coversPolicies(row.key).annotation] }, ({ page, request, baseURL }) => runPolicyRow({ page, request, baseURL }, row));
  }
});
