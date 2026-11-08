/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 他人のプロフィール (/@:acct) の各タブを押して、そのタブの API が対象の利用者で呼ばれ、
// API で作っておいたものが出ることを確かめる (#4072)。
//
// タブは testId を持たないので、見出しのタブのボタンを文言で押す。各タブの中身は、作ったものへの
// リンク (クリップ・リスト・ページ・Play・ギャラリー)、リアクションしたノートの本文、実績の名前で見る。
// ファイルのタブはサムネイルだけを描くので、応答に投稿が入ることで見る。アクティビティは
// グラフなので、API が対象の利用者で呼ばれることまで。リアクションのタブは、利用者が
// 公開している (publicReactions、既定は公開) ときだけ他人に出る。

import { expect, type Page, test } from '@playwright/test';
import { uploadTinyPNG } from '../../../fixtures/files';
import { pageBody } from '../../../fixtures/route_check';
import { signinWithState } from '../../../fixtures/settings_table';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';
import { apiOk, freshUser, postNote } from '../../../fixtures/ui_timeline';
import { type Principal } from '../../../fixtures/auth';

/** The header tab whose text is exactly `label`. */
function tab(page: Page, label: string) {
  return page.locator('button').filter({ hasText: new RegExp(`^\\s*${label}\\s*$`) });
}

/** Clicks the header tab `label` and resolves with the response of `endpoint` for `userId`. */
async function openTab(page: Page, label: string, endpoint: string, userId: string) {
  const resp = page.waitForResponse((r) => new URL(r.url()).pathname === `/api/${endpoint}` && r.request().postDataJSON()?.userId === userId, { timeout: 15_000 });
  await tab(page, label).first().click();
  const r = await resp;
  expect(r.ok(), `${endpoint} が失敗した`).toBe(true);
  return r;
}

test.describe('UI: profile tabs', () => {
  test.setTimeout(60_000);

  let viewer: Principal;
  let target: Principal;
  test.beforeEach(async ({ page, request, baseURL }) => {
    viewer = await freshUser(request, 'pwptv');
    target = await freshUser(request, 'pwptt');
    await signinWithState(page, request, baseURL!, viewer);
  });

  test('files', coversRoutes('/@:acct/:page?'), async ({ page, request, baseURL }) => {
    const file = await uploadTinyPNG(request, baseURL!, target.token, 'pw-tab.png');
    const noteId = await postNote(request, target, null, { fileIds: [file.id] });
    await gotoRoute(page, baseURL!, `/@${target.username}`);
    const r = await openTab(page, 'Files', 'users/notes', target.id);
    expect((r.request().postDataJSON() as { withFiles?: boolean }).withFiles, 'ファイル付きに絞っていない').toBe(true);
    expect((await r.json() as { id: string }[]).map((n) => n.id), 'ファイル付きの投稿が無い').toContain(noteId);
  });

  test('activity', coversRoutes('/@:acct/:page?'), async ({ page, baseURL }) => {
    await gotoRoute(page, baseURL!, `/@${target.username}`);
    await openTab(page, 'Activity', 'charts/user/notes', target.id);
  });

  test('achievements', coversRoutes('/@:acct/:page?'), async ({ page, request, baseURL }) => {
    await apiOk(request, 'i/claim-achievement', { i: target.token, name: 'clickedClickHere' });
    await gotoRoute(page, baseURL!, `/@${target.username}`);
    await openTab(page, 'Achievements', 'users/achievements', target.id);
    await expect(pageBody(page).getByText('Click here', { exact: true }).first(), '取った実績が出ない').toBeVisible();
  });

  test('reactions: shown only when public', coversRoutes('/@:acct/:page?'), async ({ page, request, baseURL }) => {
    const author = await freshUser(request, 'pwpta');
    const text = `pw reacted ${Date.now().toString(36)}`;
    const noteId = await postNote(request, author, text);
    await apiOk(request, 'notes/reactions/create', { i: target.token, noteId, reaction: '\u{1F44D}' });

    // 公開していなければ、他人にはタブが出ない (対照)。既定は公開なので、先に閉じる
    await apiOk(request, 'i/update', { i: target.token, publicReactions: false });
    await gotoRoute(page, baseURL!, `/@${target.username}`);
    await expect(tab(page, 'Files').first()).toBeVisible();
    await expect(tab(page, 'Reactions'), '公開していないのにタブが出る').toHaveCount(0);

    await apiOk(request, 'i/update', { i: target.token, publicReactions: true });
    await gotoRoute(page, baseURL!, `/@${target.username}`);
    await openTab(page, 'Reactions', 'users/reactions', target.id);
    await expect(pageBody(page).getByText(text).first(), 'リアクションしたノートが出ない').toBeVisible();
  });

  test('clips', coversRoutes('/@:acct/:page?'), async ({ page, request, baseURL }) => {
    const clip = await apiOk(request, 'clips/create', { i: target.token, name: 'pw tab clip', isPublic: true }) as { id: string };
    await gotoRoute(page, baseURL!, `/@${target.username}`);
    await openTab(page, 'Clips', 'users/clips', target.id);
    await expect(pageBody(page).locator(`a[href="/clips/${clip.id}"]`).first()).toBeVisible();
  });

  test('lists', coversRoutes('/@:acct/:page?'), async ({ page, request, baseURL }) => {
    const list = await apiOk(request, 'users/lists/create', { i: target.token, name: 'pw tab list' }) as { id: string };
    await apiOk(request, 'users/lists/update', { i: target.token, listId: list.id, isPublic: true });
    await gotoRoute(page, baseURL!, `/@${target.username}`);
    await openTab(page, 'Lists', 'users/lists/list', target.id);
    await expect(pageBody(page).locator(`a[href="/list/${list.id}"]`).first()).toBeVisible();
  });

  test('pages', coversRoutes('/@:acct/:page?'), async ({ page, request, baseURL }) => {
    const name = `pwtab${Date.now().toString(36)}`;
    await apiOk(request, 'pages/create', {
      i: target.token, title: 'pw tab page', name, summary: '', content: [{ type: 'text', text: 'body' }], variables: [],
      script: '', eyeCatchingImageId: null, font: 'sans-serif', alignCenter: false, hideTitleWhenPinned: false,
    });
    await gotoRoute(page, baseURL!, `/@${target.username}`);
    await openTab(page, 'Pages', 'users/pages', target.id);
    await expect(pageBody(page).locator(`a[href="/@${target.username}/pages/${name}"]`).first()).toBeVisible();
  });

  test('play', coversRoutes('/@:acct/:page?'), async ({ page, request, baseURL }) => {
    const flash = await apiOk(request, 'flash/create', { i: target.token, title: 'pw tab play', summary: '', script: '/// @ 1.0.0', permissions: [], visibility: 'public' }) as { id: string };
    await gotoRoute(page, baseURL!, `/@${target.username}`);
    await openTab(page, 'Play', 'users/flashs', target.id);
    await expect(pageBody(page).locator(`a[href="/play/${flash.id}"]`).first()).toBeVisible();
  });

  test('gallery', coversRoutes('/@:acct/:page?'), async ({ page, request, baseURL }) => {
    const file = await uploadTinyPNG(request, baseURL!, target.token, 'pw-gallery.png');
    const post = await apiOk(request, 'gallery/posts/create', { i: target.token, title: 'pw tab gallery', description: '', fileIds: [file.id], isSensitive: false }) as { id: string };
    await gotoRoute(page, baseURL!, `/@${target.username}`);
    await openTab(page, 'Gallery', 'users/gallery/posts', target.id);
    await expect(pageBody(page).locator(`a[href="/gallery/${post.id}"]`).first()).toBeVisible();
  });
});
