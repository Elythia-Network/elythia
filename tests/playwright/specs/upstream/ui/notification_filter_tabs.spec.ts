/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /my/notifications の種類のフィルタと、メンション・ダイレクトのタブで、通知が
// 絞られることを確かめる (#4046)。
//
// フィルタは選んだ種類以外を `excludeTypes` にして取り直す (notifications.vue)。
// UI が送った `excludeTypes` と、出た通知の種類の両方を見る。タブは
// `notes/mentions` を取り、ダイレクトのタブだけ `visibility: 'specified'` を足す。
// 返信は本文に相手を書かなくてもメンションに数える (本家の NoteCreateService が
// 返信先の人を mentions に足す) ので、メンションのタブに出ないものの例には、
// 通知は起こすがメンションではない引用を使う。

import { expect, test, type APIRequestContext, type Page } from '@playwright/test';
import type { Principal } from '../../../fixtures/auth';
import { pollForNotification } from '../../../fixtures/notifications';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, clickMenuItem, clickPageTab, expectNotesInBody, freshUser, pageBody, postNote, runTag, waitForApiBody } from '../../../fixtures/ui_timeline';

interface Seeded {
  viewer: Principal;
  mention: string;
  reply: string;
  quote: string;
  direct: string;
  target: string;
}

async function seed(request: APIRequestContext): Promise<Seeded> {
  const viewer = await freshUser(request, 'pwnfv');
  const actor = await freshUser(request, 'pwnfa');
  const tag = runTag('nf');
  const target = `${tag}-viewer-note`;
  const targetId = await postNote(request, viewer, target);
  const mention = `${tag}-mention`;
  const reply = `${tag}-reply`;
  const quote = `${tag}-quote`;
  const direct = `${tag}-direct`;
  await postNote(request, actor, `@${viewer.username} ${mention}`);
  await postNote(request, actor, reply, { replyId: targetId });
  await postNote(request, actor, quote, { renoteId: targetId });
  await postNote(request, actor, `@${viewer.username} ${direct}`, { visibility: 'specified', visibleUserIds: [viewer.id] });
  await apiOk(request, 'notes/reactions/create', { i: actor.token, noteId: targetId, reaction: '👍' });
  for (const type of ['mention', 'reply', 'quote', 'reaction']) {
    await pollForNotification(request, viewer.token, (n) => n.type === type);
  }
  return { viewer, mention, reply, quote, direct, target };
}

async function openNotifications(page: Page, request: APIRequestContext, baseURL: string, viewer: Principal): Promise<void> {
  await uiSigninAs(page, request, baseURL, viewer);
  await gotoRoute(page, baseURL, '/my/notifications');
}

test.describe('UI: notification filter and tabs', () => {
  test.setTimeout(90_000);

  test('the type filter keeps only the chosen type', coversRoutes('/my/notifications'), async ({ page, baseURL, request }) => {
    const s = await seed(request);
    await openNotifications(page, request, baseURL!, s.viewer);
    const item = (type: string) => pageBody(page).locator(`[data-testid="notification-${type}"]`);
    // ダイレクトもメンションの通知になるので、メンションは 2 件
    await expect(item('mention')).toHaveCount(2, { timeout: 20_000 });
    await expect(item('reply')).toHaveCount(1);
    await expect(item('quote')).toHaveCount(1);
    await expect(item('reaction')).toHaveCount(1);

    await pageBody(page).locator('button:has(> i.ti-filter)').click();
    const sent = waitForApiBody(page, 'i/notifications-grouped', (b) => Array.isArray(b.excludeTypes));
    await clickMenuItem(page, 'Reactions');
    const excluded = (await sent).excludeTypes as string[];
    expect(excluded, 'reaction を除いてしまった').not.toContain('reaction');
    expect(excluded).toEqual(expect.arrayContaining(['mention', 'reply', 'follow']));

    await expect(item('reaction').filter({ hasText: s.target })).toHaveCount(1, { timeout: 20_000 });
    for (const type of ['mention', 'reply', 'quote']) {
      await expect(item(type), `${type} が絞られていない`).toHaveCount(0);
    }
  });

  test('the mentions and direct tabs list only their notes', coversRoutes('/my/notifications'), async ({ page, baseURL, request }) => {
    const s = await seed(request);
    await openNotifications(page, request, baseURL!, s.viewer);

    const mentionsSent = waitForApiBody(page, 'notes/mentions');
    await clickPageTab(page, 'notifications-tab-mentions');
    expect(await mentionsSent).not.toHaveProperty('visibility');
    await expectNotesInBody(page, [s.mention, s.reply, s.direct], [s.quote]);

    const directSent = waitForApiBody(page, 'notes/mentions');
    await clickPageTab(page, 'notifications-tab-directNotes');
    expect(await directSent).toMatchObject({ visibility: 'specified' });
    await expectNotesInBody(page, [s.direct], [s.mention, s.reply, s.quote]);
  });
});
