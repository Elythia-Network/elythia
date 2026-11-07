/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ロールのポリシーの表 (#4066): 招待・インポート・管理の権限・アカウントの削除・絵文字の申請。

import { expect, test } from '@playwright/test';
import { type AdminRowContext, assignRole, cleanupApi, coversPolicies, expectApiError, expectApiOk, type PolicyRow, policyTitle, runPolicyRow, setSwitch, switchIn } from '../../../../fixtures/admin_table';
import { callApi } from '../../../../fixtures/api';
import { DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from '../../../../fixtures/auth';
import { distinctPNG, uploadPNG } from '../../../../fixtures/ui_drive';
import { coversRoutes } from '../../../../fixtures/ui_route';

/** Gives the row's user a second role with `policies`; the role id is `ctx.data`, removed by `dropHelperRole`. */
function helperRole(policies: Record<string, unknown>, isModerator = false): (ctx: AdminRowContext) => Promise<unknown> {
  return ({ request, root, user }) => assignRole(request, root, user.id, { isModerator, policies });
}

async function dropHelperRole({ request, root, data }: AdminRowContext): Promise<void> {
  if (typeof data === 'string') await cleanupApi(request, 'admin/roles/delete', { i: root.token, roleId: data });
}

/**
 * Cancels the pending emoji applications `token` made, so that they do not
 * pile up for later specs. It is a clean-up: failures are soft (cleanupApi),
 * so it does not hide the row's own failure.
 */
async function cancelApplications(request: AdminRowContext['request'], token: string): Promise<void> {
  const resp = await callApi(request, 'emoji-application/list-mine', { i: token, limit: 100 });
  expect.soft(resp.ok(), '後片付けで申請の一覧を読めなかった').toBe(true);
  if (!resp.ok()) return;
  // 取り消せるのは審査待ちだけ
  for (const app of await resp.json() as { id: string; status?: string }[]) {
    if (app.status === 'pending') await cleanupApi(request, 'emoji-application/cancel', { i: token, applicationId: app.id });
  }
}

/** An import must pass the role check (204). The file is a one-line CSV of the user's own name. */
function importAllowed(endpoint: string): (ctx: AdminRowContext) => Promise<void> {
  return async ({ request, baseURL, user }) => {
    const resp = await request.post(`${baseURL}/api/drive/files/create`, {
      multipart: { i: user.token, file: { name: 'pw.csv', mimeType: 'text/csv', buffer: Buffer.from(`${user.username}\n`) } },
    });
    expect(resp.ok(), 'CSV を上げられない').toBe(true);
    await expectApiOk(request, endpoint, { i: user.token, fileId: (await resp.json()).id });
  };
}

/** Applies for a new emoji twice; the second must fail with `code` (and `period` for a quota). */
function secondApplicationFails(code: string, period?: string): (ctx: AdminRowContext) => Promise<void> {
  return async ({ request, baseURL, user }) => {
    const apply = async () => callApi(request, 'emoji-application/create', {
      i: user.token, kind: 'own', name: `pw${Date.now().toString(36)}${Math.floor(Math.random() * 1e6)}`, license: 'pw',
      fileId: (await uploadPNG(request, baseURL, user.token, 'pw.png', distinctPNG())).id,
    });
    const first = await apply();
    expect(first.ok(), `1 件目の申請が通らない (${first.status()})`).toBe(true);
    const json = await (await apply()).json() as { error?: { code?: string; info?: { period?: string } } };
    expect(json.error?.code, `2 件目の申請が ${code} で拒否されない`).toBe(code);
    if (period) expect(json.error?.info?.period, '上限の期間').toBe(period);
  };
}

const ROWS: PolicyRow[] = [
  {
    key: 'canInvite', control: { kind: 'switch', on: true }, value: true,
    effect: async ({ request, user }) => { await expectApiOk(request, 'invite/create', { i: user.token }); },
  },
  {
    key: 'inviteLimit', control: { kind: 'number', value: 1 }, value: 1,
    setup: helperRole({ canInvite: true }), cleanup: dropHelperRole,
    effect: async ({ request, user }) => {
      await expectApiOk(request, 'invite/create', { i: user.token });
      await expectApiError(request, 'invite/create', { i: user.token }, 'EXCEEDED_LIMIT_OF_CREATE_INVITE_CODE');
    },
  },
  {
    key: 'inviteLimitCycle', control: { kind: 'number', value: 60 }, value: 60,
    effect: { skip: '上限を数える期間で、効き目は期間が過ぎて数え直されるまで見えない。行の時間内に待てない' },
  },
  {
    key: 'inviteExpirationTime', control: { kind: 'number', value: 60 }, value: 60,
    setup: helperRole({ canInvite: true }), cleanup: dropHelperRole,
    effect: async ({ request, user }) => {
      const invite = await expectApiOk(request, 'invite/create', { i: user.token }) as { expiresAt: string | null };
      expect(invite.expiresAt, '招待コードに有効期限が付かない').not.toBeNull();
      const minutes = (Date.parse(invite.expiresAt!) - Date.now()) / 60_000;
      expect(minutes, '有効期限が 60 分後でない').toBeGreaterThan(55);
      expect(minutes).toBeLessThanOrEqual(60);
    },
  },
  { key: 'canImportAntennas', control: { kind: 'switch', on: true }, value: true, effect: importAllowed('i/import-antennas') },
  { key: 'canImportBlocking', control: { kind: 'switch', on: true }, value: true, effect: importAllowed('i/import-blocking') },
  { key: 'canImportFollowing', control: { kind: 'switch', on: true }, value: true, effect: importAllowed('i/import-following') },
  { key: 'canImportMuting', control: { kind: 'switch', on: true }, value: true, effect: importAllowed('i/import-muting') },
  { key: 'canImportUserLists', control: { kind: 'switch', on: true }, value: true, effect: importAllowed('i/import-user-lists') },
  {
    key: 'canManageAvatarDecorations', control: { kind: 'switch', on: true }, value: true,
    effect: async ({ request, user }) => { await expectApiOk(request, 'admin/avatar-decorations/list', { i: user.token }); },
  },
  {
    key: 'canManageCustomEmojis', control: { kind: 'switch', on: true }, value: true,
    effect: async ({ request, user }) => { await expectApiOk(request, 'admin/emoji/list', { i: user.token }); },
  },
  {
    key: 'canSearchIpHistory', control: { kind: 'switch', on: true }, value: true,
    // IP の照会はモデレーターだけの API なので、モデレーターのロールも足す
    setup: helperRole({}, true), cleanup: dropHelperRole,
    effect: async ({ request, user }) => { await expectApiOk(request, 'admin/ip/accounts', { i: user.token, ip: '127.0.0.1' }); },
  },
  {
    key: 'canCreateChannel', control: { kind: 'switch', on: false }, value: false,
    effect: ({ request, user }) => expectApiError(request, 'channels/create', { i: user.token, name: 'pw' }, 'ROLE_PERMISSION_DENIED'),
  },
  {
    key: 'canDeleteAccount', control: { kind: 'switch', on: false }, value: false,
    effect: ({ request, user }) => expectApiError(request, 'i/delete-account', { i: user.token, password: DEFAULT_TEST_PASSWORD }, 'ROLE_PERMISSION_DENIED'),
  },
  {
    key: 'canPurgeAccount', control: { kind: 'switch', on: false }, value: false,
    // 完全に消せないと、削除した後もアカウントの行が残る (管理者には見える)。既定では消える
    effect: async ({ request, root, user }) => {
      // 削除の処理 (後から走る job) は、行を残すかどうかに関わらずノートを消す。この利用者のノートが
      // 消えるのを待てば、この利用者の job が終わったと分かるので、その後で行が残っているかを見る
      const note = await expectApiOk(request, 'notes/create', { i: user.token, text: 'pw purge' });
      await expectApiOk(request, 'i/delete-account', { i: user.token, password: DEFAULT_TEST_PASSWORD });
      await expect.poll(async () => (await callApi(request, 'notes/show', { noteId: note.createdNote.id })).status(), { message: '削除の処理が終わらない (ノートが残っている)', timeout: 20_000 }).toBe(400);
      expect((await callApi(request, 'admin/show-user', { i: root.token, userId: user.id })).status(), 'アカウントの行が消えた').toBe(200);
    },
  },
  {
    key: 'canRequestCustomEmojis', control: { kind: 'switch', on: false }, value: false,
    effect: async ({ request, baseURL, user }) => {
      const file = await uploadPNG(request, baseURL, user.token, 'pw.png', distinctPNG());
      await expectApiError(request, 'emoji-application/create', { i: user.token, kind: 'own', name: `pw${Date.now().toString(36)}`, license: 'pw', fileId: file.id }, 'ROLE_PERMISSION_DENIED');
    },
  },
  { key: 'emojiApplicationMaxPerDay', control: { kind: 'number', value: 1 }, value: 1, effect: secondApplicationFails('EMOJI_APPLICATION_QUOTA_EXCEEDED', 'day'), cleanup: ({ request, user }) => cancelApplications(request, user.token) },
  { key: 'emojiApplicationMaxPerWeek', control: { kind: 'number', value: 1 }, value: 1, effect: secondApplicationFails('EMOJI_APPLICATION_QUOTA_EXCEEDED', 'week'), cleanup: ({ request, user }) => cancelApplications(request, user.token) },
  { key: 'emojiApplicationMaxPerMonth', control: { kind: 'number', value: 1 }, value: 1, effect: secondApplicationFails('EMOJI_APPLICATION_QUOTA_EXCEEDED', 'month'), cleanup: ({ request, user }) => cancelApplications(request, user.token) },
  { key: 'emojiApplicationMaxPending', control: { kind: 'number', value: 1 }, value: 1, effect: secondApplicationFails('EMOJI_APPLICATION_PENDING_LIMIT_EXCEEDED'), cleanup: ({ request, user }) => cancelApplications(request, user.token) },
  {
    key: 'optOutNotificationTypes',
    control: {
      kind: 'custom',
      set: async (folder) => setSwitch(await switchIn(folder, 'Emoji registration requests'), true),
      shows: async (folder) => { await expect(await switchIn(folder, 'Emoji registration requests')).toHaveAttribute('data-checked', 'true'); },
    },
    value: ['emojiApplicationReceived'],
    // 絵文字の申請の通知は、カスタム絵文字を管理できる人に届く。その権限を足し、別の利用者が申請しても届かないことを見る
    setup: helperRole({ canManageCustomEmojis: true }), cleanup: dropHelperRole,
    effect: async ({ request, baseURL, root, user }) => {
      // 届かないことは即座にも成り立つので、同じ権限でオプトアウトしていない審査役 (対照) に届くのを待ってから見る
      const reviewer = await signupUser(request, randomUsername('pwrev'));
      const reviewerRole = await assignRole(request, root, reviewer.id, { policies: { canManageCustomEmojis: true } });
      const applicant = await signupUser(request, randomUsername('pwapp'));
      try {
        const file = await uploadPNG(request, baseURL, applicant.token, 'pw.png', distinctPNG());
        await expectApiOk(request, 'emoji-application/create', { i: applicant.token, kind: 'own', name: `pw${Date.now().toString(36)}`, license: 'pw', fileId: file.id });
        const received = async (token: string) => ((await expectApiOk(request, 'i/notifications', { i: token })) as { type: string }[]).filter((n) => n.type === 'emojiApplicationReceived').length;
        await expect.poll(() => received(reviewer.token), { message: '対照の審査役に通知が届かない' }).toBeGreaterThan(0);
        expect(await received(user.token), '絵文字の申請の通知が届く').toBe(0);
      } finally {
        await cancelApplications(request, applicant.token);
        await cleanupApi(request, 'admin/roles/delete', { i: root.token, roleId: reviewerRole });
      }
    },
  },
];

test.describe('admin: role policies (invite, import, moderation, account, emoji)', () => {
  test.setTimeout(60_000);
  for (const row of ROWS) {
    test(policyTitle(row), { annotation: [...coversRoutes('/admin/roles/:id/edit').annotation, ...coversPolicies(row.key).annotation] }, ({ page, request, baseURL }) => runPolicyRow({ page, request, baseURL }, row));
  }
});
