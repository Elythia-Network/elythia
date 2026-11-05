/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ストリーミングで通知が届いたときのトーストと、ナビの未読のバッジを確かめる
// (#4046。3 つ目のテストは Elythia 独自)。
//
// 1. 画面を見ている間に届くと、トーストが出る。トーストを出したクライアントは
//    `readNotification` を送って既読にする (ui/_common_/common.vue)。サーバーは
//    通知から 2 秒たっても未読のときだけ `unreadNotification` を送る (本家の
//    NotificationService と同じ) ので、バッジは増えないまま。バッジの文字の変化を
//    MutationObserver で全部記録し、一瞬も出なかったことを見る。
// 2. 画面が裏にあるとき (visibilityState が hidden) に届くと、トーストは出ず、
//    バッジの数が届いた数だけ増える。裏のタブは headless では作れないので、
//    visibilityState を hidden に見せかける。
// 3. ストリーミングが切れていた間に届いた通知は、トーストもバッジの増加も起きない。
//    Elythia は再接続したときに `/api/i` で未読の数を取り直す (boot/main-boot.ts の
//    resyncUnreadNotificationCount) ので、バッジがサーバーの数に揃う。本家は次の通知が
//    届くまで 0 のまま。

import { expect, test, type Page } from '@playwright/test';
import { isTsBackend } from '../../../fixtures/backend';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { apiOk, freshUser, postNote, recordStreamChannels, routeSwitchableStream, runTag, waitForApiBody, waitForStreamChannel } from '../../../fixtures/ui_timeline';

const BADGE = '[data-testid="navbar-indicator-notifications"]';

// boot/main-boot.ts の unreadResyncInterval。起動から 30 秒の間の再接続では未読の数を
// 取り直さない (起動時の取得と重なるため)。待つのは画面の仕様の値で、通信の待ちではない
const UNREAD_RESYNC_INTERVAL_MS = 30_000;

// 既読にして、バッジが消えるのを待つ。サインインの「ログインがありました」などで、
// 作ったばかりの利用者にも未読がある。
//
// **消えなければ既読をやり直す。** 画面が main を購読する frame を送った直後は、
// サーバーがまだ購読を登録していないことがあり、そこで送られた readAllNotifications は
// 画面に届かない (実測で 1 度踏んだ)。API の既読は毎回 readAllNotifications を送る
async function clearBadge(page: Page, request: Parameters<typeof apiOk>[0], token: string): Promise<void> {
  await expect(async () => {
    await apiOk(request, 'notifications/mark-all-as-read', { i: token });
    await expect(page.locator(BADGE)).toHaveCount(0, { timeout: 3_000 });
  }, '既読にしてもバッジが消えない').toPass({ timeout: 20_000 });
}

// バッジの文字の変化を全部記録する。消えたときは '' を積む
async function recordBadge(page: Page): Promise<void> {
  await page.evaluate((selector) => {
    const w = window as unknown as { __pwBadge: string[] };
    w.__pwBadge = [];
    let last: string | null = null;
    const read = () => {
      const el = document.querySelector(selector);
      const value = el === null ? '' : (el.textContent ?? '').trim();
      if (value !== last) {
        w.__pwBadge.push(value);
        last = value;
      }
    };
    read();
    new MutationObserver(read).observe(document.body, { subtree: true, childList: true, characterData: true });
  }, BADGE);
}

function badgeHistory(page: Page): Promise<string[]> {
  return page.evaluate(() => (window as unknown as { __pwBadge: string[] }).__pwBadge);
}

test.describe('UI: notification toast and unread badge', () => {
  test.setTimeout(120_000);

  test('a notification streamed to a visible page shows a toast and is read at once', async ({ page, baseURL, request }) => {
    const viewer = await freshUser(request, 'pwtbv');
    const actor = await freshUser(request, 'pwtba');
    const stream = recordStreamChannels(page);
    await uiSigninAs(page, request, baseURL!, viewer);
    await waitForStreamChannel(stream, 'main');
    // 広い画面では右のウィジェット列の通知も同じ通知を既読にするので、トーストが既読に
    // しなくなっても通ってしまう。ウィジェット列を出さない幅で開き直す (出すかどうかは
    // 読み込みのときに決まるので、幅を変えたら再読み込みが要る)
    await page.setViewportSize({ width: 1000, height: 900 });
    await page.reload();
    await waitForStreamChannel(stream, 'main', 2);
    await clearBadge(page, request, viewer.token);
    await recordBadge(page);

    const tag = runTag('tb');
    const noteId = await postNote(request, actor, `@${viewer.username} ${tag}-mention`);

    const toast = page.getByTestId('toast-notification').filter({ hasText: actor.username });
    await expect(toast, 'トーストが出ない').toBeVisible({ timeout: 20_000 });
    await expect.poll(() => stream.sent.some((f) => f.includes('"readNotification"')), { message: 'readNotification を送らない' }).toBe(true);
    // サーバーの既読の位置がこの通知まで進んだ (= 2 秒後の unreadNotification は出ない)
    await expect.poll(async () => (await apiOk(request, 'i', { i: viewer.token })).unreadNotificationsCount).toBe(0);
    const [latest] = (await apiOk(request, 'i/notifications', { i: viewer.token, limit: 1, markAsRead: false })) as { note?: { id: string } }[];
    expect(latest.note?.id).toBe(noteId);
    // トーストが消える (6 秒) まで待ち、その間にバッジが一度も出なかったことを見る
    await expect(toast).toHaveCount(0, { timeout: 20_000 });
    expect(await badgeHistory(page), '見ている間に届いた通知でバッジが出た').toEqual(['']);
  });

  test('notifications streamed to a hidden page bump the badge without a toast', async ({ page, baseURL, request }) => {
    const viewer = await freshUser(request, 'pwtbv');
    const actor = await freshUser(request, 'pwtba');
    const stream = recordStreamChannels(page);
    await uiSigninAs(page, request, baseURL!, viewer);
    await waitForStreamChannel(stream, 'main');
    await clearBadge(page, request, viewer.token);
    await recordBadge(page);
    await page.evaluate(() => {
      Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
      Object.defineProperty(document, 'hidden', { configurable: true, get: () => true });
      document.dispatchEvent(new Event('visibilitychange'));
    });

    const tag = runTag('tb');
    await postNote(request, actor, `@${viewer.username} ${tag}-first`);
    await expect(page.locator(BADGE), '1 件目でバッジが 1 にならない').toHaveText('1', { timeout: 20_000 });
    await postNote(request, actor, `@${viewer.username} ${tag}-second`);
    await expect(page.locator(BADGE), '2 件目でバッジが 2 にならない').toHaveText('2', { timeout: 20_000 });

    expect(await badgeHistory(page)).toEqual(['', '1', '2']);
    expect(stream.sent.some((f) => f.includes('"readNotification"')), '裏にいるのに既読にした').toBe(false);
    await expect(page.getByTestId('toast-notification')).toHaveCount(0);
    expect((await apiOk(request, 'i', { i: viewer.token })).unreadNotificationsCount).toBe(2);
  });

  test('the badge catches up with notifications missed while disconnected', async ({ page, baseURL, request }) => {
    test.skip(isTsBackend, '再接続での未読の数の取り直しは Elythia 独自');
    const viewer = await freshUser(request, 'pwtbv');
    const actor = await freshUser(request, 'pwtba');
    const stream = await routeSwitchableStream(page);
    await uiSigninAs(page, request, baseURL!, viewer);
    await waitForStreamChannel(stream, 'main');
    await clearBadge(page, request, viewer.token);

    // 起動からの間隔が明けるまで待つ (performance.now() は読み込みの始まりからの時間で、
    // 起動はその後なので、少し足す)
    await page.waitForFunction((ms) => performance.now() > ms, UNREAD_RESYNC_INTERVAL_MS + 3_000, { timeout: UNREAD_RESYNC_INTERVAL_MS + 30_000 });

    await page.context().setOffline(true);
    await stream.cut();
    const tag = runTag('tb');
    await postNote(request, actor, `@${viewer.username} ${tag}-missed`);
    await expect.poll(async () => (await apiOk(request, 'i', { i: viewer.token })).unreadNotificationsCount, { message: 'サーバーの未読が 1 にならない' }).toBe(1);

    const resynced = waitForApiBody(page, 'i', () => true, 40_000);
    await page.context().setOffline(false);
    stream.restore();
    await waitForStreamChannel(stream, 'main', 2);
    await resynced;
    await expect(page.locator(BADGE), '再接続してもバッジが未読の数に揃わない').toHaveText('1', { timeout: 20_000 });
    // 切れている間の通知なので、トーストは出ていない
    await expect(page.getByTestId('toast-notification')).toHaveCount(0);
  });
});
