/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ストリーミングが切れていた間のノートが、再接続したときに埋まることを
// 確かめる (Elythia 独自、#3132)。
//
// ストリーミングは pub/sub なので、切れていた間のノートは再送されない。Elythia は
// 再接続 (`_connected_`) のときに、手持ちの最新より新しいものを `sinceId` で取りに
// 行く (utility/reconnect-resync.ts)。本家はリロードするまで抜けたままになる。
//
// 切断は `context.setOffline` で起こし、WebSocket は spec の中継で閉じる
// (setOffline は張ってある WebSocket を閉じない。fixtures/ui_timeline.ts の
// routeSwitchableStream)。張り直したことは、新しい接続で購読し直したことで見る。
// 埋めた取得の `sinceId` は、切れる前に出ていた最新のノートの id でなければならない。

import { expect, test } from '@playwright/test';
import { isTsBackend } from '../../../fixtures/backend';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { expectRoutedPage } from '../../../fixtures/ui_route';
import {
  expectNotesInBody,
  follow,
  freshUser,
  postNote,
  recordApiBodies,
  routeSwitchableStream,
  runTag,
  waitForApiBody,
  waitForStreamChannel,
} from '../../../fixtures/ui_timeline';

// reconnect-resync.ts の DEFAULT_INITIAL_DELAY。一覧を作ってからこの間に来た接続は、
// mount 時のものとして穴埋めしない。待つのは画面の仕様の値で、通信の待ちではない
const RESYNC_INITIAL_DELAY_MS = 5_000;

test.describe('UI: timeline backfills the notes missed while disconnected', () => {
  test.skip(isTsBackend, '再接続の穴埋めは Elythia 独自 (#3132)');
  test.setTimeout(120_000);

  test('notes posted while offline appear after reconnecting', async ({ page, baseURL, request }) => {
    const viewer = await freshUser(request, 'pwrcv');
    const followee = await freshUser(request, 'pwrcf');
    await follow(request, viewer, followee);

    const tag = runTag('rc');
    const before = `${tag}-before`;
    const beforeId = await postNote(request, followee, before);

    const stream = await routeSwitchableStream(page);
    await uiSigninAs(page, request, baseURL!, viewer);
    await expectRoutedPage(page, '/');
    await expectNotesInBody(page, [before], []);
    await waitForStreamChannel(stream, 'homeTimeline');
    // 購読は一覧を作った後なので、購読した時刻から数えれば期限を過ぎている
    const subscribedAt = Date.now();
    await page.waitForFunction(({ since, delay }) => Date.now() - since > delay, { since: subscribedAt, delay: RESYNC_INITIAL_DELAY_MS }, { timeout: RESYNC_INITIAL_DELAY_MS + 5_000 });

    const timelineFetches = recordApiBodies(page, 'notes/timeline');
    await page.context().setOffline(true);
    await stream.cut();

    const gap = `${tag}-while-offline`;
    await postNote(request, followee, gap);

    const resync = waitForApiBody(page, 'notes/timeline', (b) => b.sinceId !== undefined, 40_000);
    await page.context().setOffline(false);
    stream.restore();
    // 新しい接続で home を購読し直す
    await waitForStreamChannel(stream, 'homeTimeline', 2);
    expect(stream.connections).toBeGreaterThanOrEqual(2);

    expect(await resync, '穴埋めの起点が、切れる前に出ていた最新のノートでない').toMatchObject({ sinceId: beforeId });
    await expectNotesInBody(page, [gap, before], []);
    // 埋めたのは差分の取得で、一覧の取り直しではない
    expect(timelineFetches.filter((b) => b.sinceId === undefined), '一覧を取り直して埋めた').toEqual([]);
  });
});
