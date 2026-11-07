/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ロールのポリシーの表 (#4066): 個数の上限。上限を 1 (予約投稿は 0) にして、上限を超える
// 操作が決まったエラーで拒否されることを見る。既定値ならどれも通る。

import { test } from '@playwright/test';
import { type AdminRowContext, coversPolicies, expectApiError, expectApiOk, type PolicyRow, policyTitle, runPolicyRow } from '../../../../fixtures/admin_table';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { coversRoutes } from '../../../../fixtures/ui_route';

/** Creates twice with the same body; the second must fail with `code`. */
function secondFails(endpoint: string, body: Record<string, unknown>, code: string): (ctx: AdminRowContext) => Promise<void> {
  return async ({ request, user }) => {
    await expectApiOk(request, endpoint, { i: user.token, ...body });
    await expectApiError(request, endpoint, { i: user.token, ...body }, code);
  };
}

const ANTENNA = { name: 'pw', src: 'all', keywords: [['pw']], excludeKeywords: [], users: [], caseSensitive: false, localOnly: false, excludeBots: false, withReplies: false, withFile: false, excludeNotesInSensitiveChannel: false };

const ROWS: PolicyRow[] = [
  {
    key: 'pinLimit', control: { kind: 'number', value: 1 }, value: 1,
    effect: async ({ request, user }) => {
      const a = await expectApiOk(request, 'notes/create', { i: user.token, text: 'pw pin a' });
      const b = await expectApiOk(request, 'notes/create', { i: user.token, text: 'pw pin b' });
      await expectApiOk(request, 'i/pin', { i: user.token, noteId: a.createdNote.id });
      await expectApiError(request, 'i/pin', { i: user.token, noteId: b.createdNote.id }, 'PIN_LIMIT_EXCEEDED');
    },
  },
  { key: 'antennaLimit', control: { kind: 'number', value: 1 }, value: 1, effect: secondFails('antennas/create', ANTENNA, 'TOO_MANY_ANTENNAS') },
  {
    key: 'wordMuteLimit', control: { kind: 'number', value: 5 }, value: 5,
    // 上限は語の文字数の合計
    effect: ({ request, user }) => expectApiError(request, 'i/update', { i: user.token, mutedWords: [['pwabcdefgh']] }, 'TOO_MANY_MUTED_WORDS'),
  },
  {
    key: 'webhookLimit', control: { kind: 'number', value: 1 }, value: 1,
    effect: secondFails('i/webhooks/create', { name: 'pw', url: 'https://remote.example/hook', secret: 'pw', on: ['note'] }, 'TOO_MANY_WEBHOOKS'),
  },
  { key: 'clipLimit', control: { kind: 'number', value: 1 }, value: 1, effect: secondFails('clips/create', { name: 'pw' }, 'TOO_MANY_CLIPS') },
  {
    key: 'noteEachClipsLimit', control: { kind: 'number', value: 1 }, value: 1,
    // 1 つのクリップに入れられるノートの数
    effect: async ({ request, user }) => {
      const clip = await expectApiOk(request, 'clips/create', { i: user.token, name: 'pw' });
      const a = await expectApiOk(request, 'notes/create', { i: user.token, text: 'pw clip a' });
      const b = await expectApiOk(request, 'notes/create', { i: user.token, text: 'pw clip b' });
      await expectApiOk(request, 'clips/add-note', { i: user.token, clipId: clip.id, noteId: a.createdNote.id });
      await expectApiError(request, 'clips/add-note', { i: user.token, clipId: clip.id, noteId: b.createdNote.id }, 'TOO_MANY_CLIP_NOTES');
    },
  },
  { key: 'userListLimit', control: { kind: 'number', value: 1 }, value: 1, effect: secondFails('users/lists/create', { name: 'pw' }, 'TOO_MANY_USERLISTS') },
  {
    key: 'userEachUserListsLimit', control: { kind: 'number', value: 1 }, value: 1,
    setup: async ({ request }) => [await signupUser(request, randomUsername('pwl')), await signupUser(request, randomUsername('pwl'))],
    effect: async ({ request, user, data }) => {
      const [a, b] = data as { id: string }[];
      const list = await expectApiOk(request, 'users/lists/create', { i: user.token, name: 'pw' });
      await expectApiOk(request, 'users/lists/push', { i: user.token, listId: list.id, userId: a.id });
      await expectApiError(request, 'users/lists/push', { i: user.token, listId: list.id, userId: b.id }, 'TOO_MANY_USERS');
    },
  },
  { key: 'noteDraftLimit', control: { kind: 'number', value: 1 }, value: 1, effect: secondFails('notes/drafts/create', { text: 'pw draft' }, 'TOO_MANY_DRAFTS') },
  {
    key: 'scheduledNoteLimit', control: { kind: 'number', value: 0 }, value: 0,
    effect: ({ request, user }) => expectApiError(request, 'notes/drafts/create', { i: user.token, text: 'pw scheduled', scheduledAt: Date.now() + 3_600_000, isActuallyScheduled: true }, 'TOO_MANY_SCHEDULED_NOTES'),
  },
];

test.describe('admin: role policies (limits)', () => {
  test.setTimeout(60_000);
  for (const row of ROWS) {
    test(policyTitle(row), { annotation: [...coversRoutes('/admin/roles/:id/edit').annotation, ...coversPolicies(row.key).annotation] }, ({ page, request, baseURL }) => runPolicyRow({ page, request, baseURL }, row));
  }
});
