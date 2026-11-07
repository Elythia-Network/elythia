/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ロールのポリシーの表 (#4066): タイムライン・投稿・検索・チャット。行ごとに手動のロールを
// 作って新しい利用者に割り当て、ロールの編集画面で値を保存し、その利用者で効き目を見る
// (fixtures/admin_table.ts)。値は既定値と逆にし、設定しなければ効き目の確かめが落ちる形にする。

import { expect, test } from '@playwright/test';
import { coversPolicies, expectApiError, expectApiOk, type PolicyRow, policyTitle, rangeToMax, runPolicyRow } from '../../../../fixtures/admin_table';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { coversRoutes } from '../../../../fixtures/ui_route';

const ROWS: PolicyRow[] = [
  {
    key: 'gtlAvailable', control: { kind: 'switch', on: false }, value: false,
    effect: ({ request, user }) => expectApiError(request, 'notes/global-timeline', { i: user.token }, 'GTL_DISABLED'),
  },
  {
    key: 'ltlAvailable', control: { kind: 'switch', on: false }, value: false,
    effect: ({ request, user }) => expectApiError(request, 'notes/local-timeline', { i: user.token }, 'LTL_DISABLED'),
  },
  {
    key: 'canPublicNote', control: { kind: 'switch', on: false }, value: false,
    effect: async ({ request, user }) => {
      // 本家と同じく拒否はせず、公開範囲を「ホーム」に下げる
      const note = await expectApiOk(request, 'notes/create', { i: user.token, text: 'pw public', visibility: 'public' });
      expect(note.createdNote.visibility, '公開で投稿してもホームに下がらない').toBe('home');
    },
  },
  {
    key: 'mentionLimit', control: { kind: 'number', value: 1 }, value: 1,
    setup: async ({ request }) => [await signupUser(request, randomUsername('pwm')), await signupUser(request, randomUsername('pwm'))],
    effect: async ({ request, user, data }) => {
      const [a, b] = data as { username: string }[];
      await expectApiError(request, 'notes/create', { i: user.token, text: `@${a.username} @${b.username}` }, 'CONTAINS_TOO_MANY_MENTIONS');
    },
  },
  {
    key: 'chatAvailability', control: { kind: 'select', label: 'Disabled' }, value: 'unavailable',
    setup: ({ request }) => signupUser(request, randomUsername('pwchat')),
    effect: ({ request, user, data }) => expectApiError(request, 'chat/messages/create-to-user', { i: user.token, toUserId: (data as { id: string }).id, text: 'pw' }, 'ROLE_PERMISSION_DENIED'),
  },
  {
    key: 'canSearchNotes', control: { kind: 'switch', on: true }, value: true,
    effect: async ({ request, user }) => { await expectApiOk(request, 'notes/search', { i: user.token, query: 'pw' }); },
  },
  {
    key: 'canSearchUsers', control: { kind: 'switch', on: false }, value: false,
    effect: ({ request, user }) => expectApiError(request, 'users/search', { i: user.token, query: 'pw' }, 'ROLE_PERMISSION_DENIED'),
  },
  {
    key: 'canUseTranslator', control: { kind: 'switch', on: false }, value: false,
    effect: { skip: '翻訳は DeepL の鍵が要る。鍵の無いこのスタックでは、許可されていても notes/translate が拒否と同じ UNAVAILABLE を返し、区別できない' },
  },
  {
    key: 'rateLimitFactor',
    control: {
      kind: 'custom',
      // 値の MkRange は枠の最初の範囲 (最後は優先度)。右端は 300%
      set: (folder, { page }) => rangeToMax(page, folder.locator('.timctyfi').first()),
      shows: async (folder) => { await expect(folder.getByTestId('folder-header')).toContainText('300%'); },
    },
    value: 3,
    effect: { skip: 'Playwright のスタックは instance.yml の disableEndpointRateLimits でレート制限を切っているので、倍率の効き目が出ない' },
  },
];

test.describe('admin: role policies (timeline, note, search, chat)', () => {
  test.setTimeout(60_000);
  for (const row of ROWS) {
    test(policyTitle(row), { annotation: [...coversRoutes('/admin/roles/:id/edit').annotation, ...coversPolicies(row.key).annotation] }, ({ page, request, baseURL }) => runPolicyRow({ page, request, baseURL }, row));
  }
});
