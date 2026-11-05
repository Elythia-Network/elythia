/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// フォロー申請の通知の中の「承認」「拒否」のボタンで、`following/requests/accept` /
// `reject` が、その申請を出した人の id で送られ、関係がそのとおりになることを
// 確かめる (#4046)。
//
// 申請を 2 人から受け、片方を承認、もう片方を拒否する。ボタンはそれぞれの通知の
// 行の中から取る (別の行のボタンを押していたら、送られる userId で分かる)。
// 申請の一覧のページ (/my/follow-requests) の同じボタンは follow_request_*.spec.ts。

import { expect, test, type Page } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import type { Principal } from '../../../fixtures/auth';
import { pollForNotification } from '../../../fixtures/notifications';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, freshUser, pageBody, waitForApiBody } from '../../../fixtures/ui_timeline';

// 申請の通知の行の中の、文字がちょうど `label` のボタンを押す。本体の
// `._pageContainer` に絞る (右のウィジェット列の通知にも同じボタンが出る)
async function clickRequestButton(page: Page, username: string, label: string): Promise<void> {
  await clickWhenReady(
    page,
    `${username} の申請の「${label}」`,
    ({ u, l }: { u: string; l: string }) => {
      const bodies = document.querySelectorAll('._pageContainer');
      const rows = Array.from(bodies[bodies.length - 1]?.querySelectorAll('[data-testid="notification-receiveFollowRequest"]') ?? []);
      const row = rows.find((r) => (r.textContent ?? '').includes(u));
      return Array.from(row?.querySelectorAll('button') ?? []).find((b) => (b.textContent ?? '').trim() === l);
    },
    { u: username, l: label },
  );
}

async function isFollowing(request: Parameters<typeof callApi>[0], from: Principal, to: Principal): Promise<boolean> {
  const resp = await callApi(request, 'users/relation', { i: from.token, userId: to.id });
  expect(resp.status()).toBe(200);
  const [rel] = await resp.json();
  return rel.isFollowing as boolean;
}

test.describe('UI: follow request buttons in notifications', () => {
  test.setTimeout(90_000);

  test('accept and reject in the notification send the requester id', coversRoutes('/my/notifications'), async ({ page, baseURL, request }) => {
    const viewer = await freshUser(request, 'pwnrv');
    const accepted = await freshUser(request, 'pwnra');
    const rejected = await freshUser(request, 'pwnrr');
    await apiOk(request, 'i/update', { i: viewer.token, isLocked: true });
    await apiOk(request, 'following/create', { i: accepted.token, userId: viewer.id });
    await apiOk(request, 'following/create', { i: rejected.token, userId: viewer.id });
    await pollForNotification(request, viewer.token, (n) => n.type === 'receiveFollowRequest' && n.userId === accepted.id);
    await pollForNotification(request, viewer.token, (n) => n.type === 'receiveFollowRequest' && n.userId === rejected.id);

    await uiSigninAs(page, request, baseURL!, viewer);
    await gotoRoute(page, baseURL!, '/my/notifications');
    await expect(pageBody(page).locator('[data-testid="notification-receiveFollowRequest"]')).toHaveCount(2, { timeout: 20_000 });

    const acceptSent = waitForApiBody(page, 'following/requests/accept');
    await clickRequestButton(page, accepted.username, 'Accept');
    expect(await acceptSent).toMatchObject({ userId: accepted.id });

    const rejectSent = waitForApiBody(page, 'following/requests/reject');
    await clickRequestButton(page, rejected.username, 'Reject');
    expect(await rejectSent).toMatchObject({ userId: rejected.id });

    await expect.poll(() => isFollowing(request, accepted, viewer), { message: '承認したのにフォローになっていない' }).toBe(true);
    await expect.poll(() => isFollowing(request, rejected, viewer)).toBe(false);
    const pending = (await apiOk(request, 'following/requests/list', { i: viewer.token })) as unknown[];
    expect(pending, '拒否した申請が残っている').toEqual([]);
  });
});
