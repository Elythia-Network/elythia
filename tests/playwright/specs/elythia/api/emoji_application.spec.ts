/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 絵文字の申請 (#2934) の独自の API 10 件を叩き、応答を /api.json の宣言で照合する (#4093)。
// 画面の spec (emoji_application_flow) で 2xx までは通っているが、応答の型は見ていなかった。

import { type APIRequestContext, expect, test } from '@playwright/test';
import { baseURL, callApi } from '../../../fixtures/api';
import { callChecked } from '../../../fixtures/api_schema';
import { cleanupApi, rootFixture } from '../../../fixtures/admin_table';
import { type Principal, randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { resetRateLimit } from '../../../fixtures/rate_limit';
import { distinctPNG, uploadPNG } from '../../../fixtures/ui_drive';

interface Application { id: string; status: string; emojiId?: string }

/** A name of an emoji no other spec uses. */
function emojiName(prefix: string): string {
  return `${prefix}_${Date.now().toString(36)}${Math.floor(Math.random() * 1e6).toString(36)}`;
}

/** Applies for an emoji named `name` of `user`'s own image, and returns the application. */
async function apply(request: APIRequestContext, user: Principal, name: string): Promise<Application> {
  const file = await uploadPNG(request, baseURL(), user.token, `${name}.png`, distinctPNG());
  return callChecked<Application>(request, 'emoji-application/create', { i: user.token, name, fileId: file.id, license: 'pw license', kind: 'own' });
}

test.describe('emoji applications: the Elythia endpoints answer as declared', () => {
  test.skip(isTsBackend, '絵文字の申請は Elythia 独自');

  test.beforeAll(() => {
    resetRateLimit();
  });

  test('apply, list mine and cancel', async ({ request }) => {
    const user = await signupUser(request, randomUsername('eaapp'));
    const app = await apply(request, user, emojiName('pw_cancel'));
    expect(app.status).toBe('pending');
    expect((await callChecked<Application[]>(request, 'emoji-application/list-mine', { i: user.token })).map((a) => a.id)).toContain(app.id);
    await callChecked(request, 'emoji-application/cancel', { i: user.token, applicationId: app.id });
    const mine = await callChecked<Application[]>(request, 'emoji-application/list-mine', { i: user.token });
    expect(mine.find((a) => a.id === app.id)?.status).toBe('canceled');
  });

  test('review: list, look at the user, approve, reject and reset the quota', async ({ request }) => {
    const { token } = rootFixture();
    const user = await signupUser(request, randomUsername('eaadm'));
    // 同じ名前の取り下げた申請は、承認する申請の「関係する申請」に出る
    const name = emojiName('pw_approve');
    const earlier = await apply(request, user, name);
    await callChecked(request, 'emoji-application/cancel', { i: user.token, applicationId: earlier.id });
    const toApprove = await apply(request, user, name);
    const toReject = await apply(request, user, emojiName('pw_reject'));

    const pending = await callChecked<Application[]>(request, 'admin/emoji-application/list', { i: token, filter: 'pending', limit: 100 });
    expect(pending.map((a) => a.id)).toEqual(expect.arrayContaining([toApprove.id, toReject.id]));
    const related = await callChecked<{ items: { id: string; matchedBy: string[] }[] }>(request, 'admin/emoji-application/related', { i: token, applicationId: toApprove.id });
    expect(related.items.find((a) => a.id === earlier.id)?.matchedBy).toContain('name');
    const byUser = await callChecked<{ items: Application[] }>(request, 'admin/emoji-application/list-by-user', { i: token, userId: user.id });
    expect(byUser.items.map((a) => a.id).sort()).toEqual([earlier.id, toApprove.id, toReject.id].sort());
    const summary = await callChecked<{ counts: { total: number; pending: number } }>(request, 'admin/emoji-application/user-summary', { i: token, userId: user.id });
    expect(summary.counts).toMatchObject({ total: 3, pending: 2 });

    try {
      const approved = await callChecked<Application>(request, 'admin/emoji-application/approve', { i: token, applicationId: toApprove.id });
      expect(approved.status).toBe('approved');
      expect(approved.emojiId, 'approving makes the emoji').toBeTruthy();
      const rejected = await callChecked<Application>(request, 'admin/emoji-application/reject', { i: token, applicationId: toReject.id, reason: 'pw reason' });
      expect(rejected.status).toBe('rejected');
      await callChecked(request, 'admin/emoji-application/reset-user-quota', { i: token, userId: user.id, reason: 'pw reset' });
      // リセットの後の集計は、記録 (lastReset) の型の側も照合する
      const reset = await callChecked<{ lastReset: { reason: string } | null }>(request, 'admin/emoji-application/user-summary', { i: token, userId: user.id });
      expect(reset.lastReset?.reason).toBe('pw reset');
    } finally {
      // 承認で作られた絵文字は、ほかの spec の絵文字の一覧に出るので消す。照合が承認の
      // 後に落ちても消えるよう、名前で探す。後片付けの失敗で、本来の失敗を覆い隠さない
      const listed = await callApi(request, 'admin/emoji/list', { i: token, query: name, limit: 10 });
      const emojis = listed.ok() ? (await listed.json()) as { id: string; name: string }[] : [];
      for (const e of emojis.filter((x) => x.name === name)) await cleanupApi(request, 'admin/emoji/delete', { i: token, id: e.id });
    }
  });
});
