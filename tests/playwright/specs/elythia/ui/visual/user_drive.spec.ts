/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 見た目の差分 (#4054): 通知、プロフィール、ユーザーのポップアップ、ドライブ。
// 種まきと固定のしかたは fixtures/visual.ts。

import { type APIRequestContext, expect, test } from '@playwright/test';
import { isTsBackend } from '../../../../fixtures/backend';
import { pageBody } from '../../../../fixtures/route_check';
import { coversRoutes, gotoRoute } from '../../../../fixtures/ui_route';
import { apiOk } from '../../../../fixtures/ui_timeline';
import { clearUnread, prepareVisualPage, regionAbove, SCHEMES, snap, startAsAlice, visualSignin, type VisualWorld, visualWorld } from '../../../../fixtures/visual';

// alice に主な種類の通知を 1 件ずつ届ける。送るのは alice がフォローしていない
// dave なので、作ったノートは alice のホームに出ない。返信・リノート・引用・
// リアクションの相手は、リノートの数が写る画面に出ない alice の通知用のノート
// (fixtures/visual.ts の notifyTargetNoteId)。撮った後に、返した片付けで消す。
// 片付けは送り始める前に `register` で渡す (送っている途中でテストが時間切れに
// なっても、afterEach が片付けられるように)
async function sendNotifications(request: APIRequestContext, w: VisualWorld, register: (undo: () => Promise<void>) => void): Promise<void> {
  const { alice, dave } = w;
  const notes: string[] = [];
  const undo = async () => {
    // 途中で落ちても残りを片付ける (残ると、他の画面の件数が変わる)
    const steps = [
      () => apiOk(request, 'notes/reactions/delete', { i: dave.token, noteId: w.notifyTargetNoteId }),
      ...notes.reverse().map((id) => () => apiOk(request, 'notes/delete', { i: dave.token, noteId: id })),
      () => apiOk(request, 'following/delete', { i: dave.token, userId: alice.id }),
      () => apiOk(request, 'notifications/flush', { i: alice.token }),
    ];
    for (const step of steps) await step().catch(() => undefined);
  };
  register(undo);
  try {
    await apiOk(request, 'notifications/flush', { i: alice.token });
    const note = async (body: Record<string, unknown>) => {
      notes.push((await apiOk(request, 'notes/create', { i: dave.token, ...body })).createdNote.id as string);
    };
    await apiOk(request, 'following/create', { i: dave.token, userId: alice.id });
    await note({ text: `@${alice.username} are you coming tonight?` });
    await note({ text: 'Count me in.', replyId: w.notifyTargetNoteId });
    await note({ renoteId: w.notifyTargetNoteId });
    await note({ text: 'This is worth reading.', renoteId: w.notifyTargetNoteId });
    await apiOk(request, 'notes/reactions/create', { i: dave.token, noteId: w.notifyTargetNoteId, reaction: '🎉' });
    // 通知はジョブで遅れて作られる。6 種類がそろうまで待ってから既読にする
    await expect.poll(async () => {
      const got = await apiOk(request, 'i/notifications', { i: alice.token, limit: 100 }) as { type: string }[];
      return [...new Set(got.map((n) => n.type))].sort();
    }, { timeout: 20_000 }).toEqual(['follow', 'mention', 'quote', 'reaction', 'renote', 'reply']);
    await clearUnread(request, alice);
  } catch (e) {
    await undo();
    throw e;
  }
}

test.describe('visual: notifications, users and drive', () => {
  test.skip(isTsBackend, '見た目の差分は Elythia の frontend だけを撮る');
  test.setTimeout(90_000);

  // 送った通知の後片付けは afterEach で行う。テストの中の finally だと、時間切れの
  // 後に request が閉じられていて失敗し、dave のノートが残る (デッキのメンションの
  // 列などに出て、次の回の絵が変わる)
  let undo: (() => Promise<void>) | null = null;
  test.afterEach(async () => {
    await undo?.();
    undo = null;
  });

  for (const scheme of SCHEMES) {
    test(`notifications (${scheme})`, coversRoutes('/my/notifications'), async ({ page, baseURL, request }) => {
      // visualWorld は通知を空にするので、それより後に送る
      const w = await visualWorld(request, baseURL!);
      await sendNotifications(request, w, (u) => { undo = u; });
      await prepareVisualPage(page, scheme);
      await visualSignin(page, request, baseURL!, w.alice, { width: 1280, height: 1200 });
      await gotoRoute(page, baseURL!, '/my/notifications');
      const body = pageBody(page);
      await body.getByText('are you coming tonight?').waitFor();
      await body.getByText('This is worth reading.').waitFor();
      await body.getByText('Any plans for the weekend?').first().waitFor();
      await snap(page, `notifications-${scheme}`);
    });

    test(`profile (${scheme})`, coversRoutes('/@:acct/:page?'), async ({ page, baseURL, request }) => {
      const w = await startAsAlice(page, request, baseURL!, scheme, { viewport: { width: 1280, height: 1200 } });
      await gotoRoute(page, baseURL!, `/@${w.alice.username}`);
      const body = pageBody(page);
      await body.getByText('Likes tea and quiet evenings.').waitFor();
      await body.getByText('Visual moderators').first().waitFor();
      // 下の「Activity」はグラフで、日付と JS のアニメーションで描かれる。その見出しより上を撮る
      const activity = body.getByText('Activity', { exact: true }).last();
      await snap(page, `profile-${scheme}`, { clip: await regionAbove(page.locator('body'), activity) });
    });

    test(`user popup (${scheme})`, coversRoutes('/'), async ({ page, baseURL, request }) => {
      const w = await startAsAlice(page, request, baseURL!, scheme);
      await gotoRoute(page, baseURL!, '/');
      const body = pageBody(page);
      await body.getByText('One picture.').waitFor();
      // ノートの名前にポインタを載せると、その人のポップアップが出る
      await body.getByText(w.bob.name).first().hover();
      const popup = page.locator('div').filter({ hasText: /^.*Bob Example.*Followers/s }).filter({ hasText: 'Following' }).last();
      await popup.waitFor();
      await snap(page, `user-popup-${scheme}`, { keepPointer: true });
    });

    test(`drive (${scheme})`, coversRoutes('/my/drive'), async ({ page, baseURL, request }) => {
      const w = await startAsAlice(page, request, baseURL!, scheme);
      await gotoRoute(page, baseURL!, '/my/drive');
      const body = pageBody(page);
      for (const name of w.driveFolders) await body.getByText(name, { exact: true }).waitFor();
      for (const name of w.driveFiles) await body.getByText(`${name}.png`).waitFor();
      await snap(page, `drive-${scheme}`);
    });
  }
});
