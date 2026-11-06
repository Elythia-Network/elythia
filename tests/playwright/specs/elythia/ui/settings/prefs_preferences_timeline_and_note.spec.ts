/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定の表 (#4056): /settings/preferences の「Timeline and note」の見出しの端末の
// 設定を、部品の操作 → 保存 → 再読み込みの後の表示 → 効き目、の順に確かめる。
//
// 効き目は、設定を変えていない (既定の値の) ときには必ず落ちる形で書く
// (`PW_SETTINGS_MUTATION=1` の変異の確かめ)。「無い」ことを見るときは、本来
// それがある画面にいることを先に確かめる。
//
// ノートは行の利用者が API で作る。タイムラインの 1 件 (MkNote) で見るものは
// プロフィール (/@<username>) で、詳しい表示 (MkNoteDetailed) でも同じ部品を
// 使うものは /notes/<id> で見る。

import { type APIRequestContext, expect, type Locator, type Page, test } from '@playwright/test';
import { type Principal, randomUsername, signupUser } from '../../../../fixtures/auth';
import type { DriveFile } from '../../../../fixtures/files';
import { pageBody } from '../../../../fixtures/route_check';
import { coversPrefs, type PrefRow, prefTitle, runPrefRow } from '../../../../fixtures/settings_table';
import { distinctPNG, uploadPNG } from '../../../../fixtures/ui_drive';
import { gotoRoute } from '../../../../fixtures/ui_route';
import { apiOk, postNote, runTag } from '../../../../fixtures/ui_timeline';

const ROUTE = '/settings/preferences';

type UploadedFile = DriveFile & { url: string; thumbnailUrl: string | null };

/** Posts a note with `count` PNGs (sensitive when asked) and returns the note id and the files. */
async function imageNote(request: APIRequestContext, baseURL: string, user: Principal, opts: { sensitive?: boolean; count?: number } = {}): Promise<{ noteId: string; files: UploadedFile[] }> {
  const files: UploadedFile[] = [];
  for (let i = 0; i < (opts.count ?? 1); i++) {
    const file = await uploadPNG(request, baseURL, user.token, `pwpref${i}.png`, distinctPNG()) as UploadedFile;
    if (opts.sensitive) await apiOk(request, 'drive/files/update', { i: user.token, fileId: file.id, isSensitive: true });
    files.push(file);
  }
  return { noteId: await postNote(request, user, null, { fileIds: files.map((f) => f.id) }), files };
}

/** Posts a note of `user` that another user reacted to with 👍. */
async function reactedNote(request: APIRequestContext, user: Principal): Promise<string> {
  const noteId = await postNote(request, user, runTag('pwreact'));
  const other = await signupUser(request, randomUsername('pwreac'));
  await apiOk(request, 'notes/reactions/create', { i: other.token, noteId, reaction: '👍' });
  return noteId;
}

/** Opens /notes/<id> and returns the detailed note once its footer is shown. */
async function openNote(page: Page, baseURL: string, noteId: string): Promise<Locator> {
  await gotoRoute(page, baseURL, `/notes/${noteId}`);
  const note = pageBody(page).getByTestId('note-detailed');
  await expect(note.getByTestId('note-menu')).toBeVisible({ timeout: 20_000 });
  return note;
}

/** Opens the profile of `user` and returns the note (MkNote's article) that contains `text`. */
async function profileNote(page: Page, baseURL: string, user: Principal, text: string): Promise<Locator> {
  // プロフィールの概要のタブには固定のノートしか出ないので、ノートのタブで探す
  await gotoRoute(page, baseURL, `/@${user.username}/notes`);
  const article = pageBody(page).locator('article').filter({ hasText: text });
  await expect(article, `「${text}」のノートがプロフィールに出ない`).toHaveCount(1, { timeout: 20_000 });
  return article;
}

test.describe('settings: preferences (Timeline and note)', () => {
  test.setTimeout(60_000);
  const ROWS: PrefRow[] = [
    {
      key: 'showFixedPostForm', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, baseURL }) => {
        // ダイアログを開かずに、タイムラインの上に投稿フォームが出る
        await gotoRoute(page, baseURL, '/');
        await expect(pageBody(page).getByTestId('post-form-text')).toBeVisible();
      },
    },
    {
      key: 'showFixedPostFormInChannel', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, request, baseURL, user }) => {
        const channel = await apiOk(request, 'channels/create', { i: user.token, name: runTag('pwch') });
        await gotoRoute(page, baseURL, `/channels/${channel.id}`);
        // 投稿フォームはチャンネルの「Timeline」のタブに出る (既定のタブは Overview)
        await pageBody(page).locator('button').filter({ hasText: /^\s*Timeline\s*$/ }).first().click();
        await expect(pageBody(page).getByTestId('post-form-text')).toBeVisible();
      },
    },
    {
      key: 'collapseRenotes', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page, request, baseURL, user }) => {
        // 畳まれるのは自分の Renote。畳まれると article が無くなり、本文の 1 行の要約だけになる (MkNote.vue)
        const other = await signupUser(request, randomUsername('pwrn'));
        const text = runTag('pwrenoted');
        const renoteId = await postNote(request, other, text);
        await postNote(request, user, null, { renoteId });
        const article = await profileNote(page, baseURL, user, text);
        await expect(article.getByTestId('note-content')).toBeVisible();
      },
    },
    {
      key: 'showNoteActionsOnlyHover', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, request, baseURL, user }) => {
        const text = runTag('pwhover');
        await postNote(request, user, text);
        const article = await profileNote(page, baseURL, user, text);
        await page.mouse.move(0, 0);
        // フッターは hover するまで visibility: hidden (MkNote.vue の .showActionsOnlyHover)
        await expect.poll(() => article.getByTestId('note-reply').evaluate((el) => getComputedStyle(el).visibility)).toBe('hidden');
      },
    },
    {
      key: 'showClipButtonInNoteFooter', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, request, baseURL, user }) => {
        const note = await openNote(page, baseURL, await postNote(request, user, runTag('pwclip')));
        await expect(note.locator('button:has(> i.ti-paperclip)')).toHaveCount(1);
      },
    },
    {
      key: 'showReactionsCount', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, request, baseURL, user }) => {
        const note = await openNote(page, baseURL, await reactedNote(request, user));
        await expect(note.getByTestId('note-reaction')).toBeVisible();
        // リアクションの数は、フッターのリアクションのボタンの中に出る
        await expect(note.getByTestId('note-react')).toHaveText(/1/);
      },
    },
    {
      key: 'confirmOnReact', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, request, baseURL, user }) => {
        const note = await openNote(page, baseURL, await reactedNote(request, user));
        // 他の人が付けたリアクションを押すと、同じリアクションを付ける前に確かめる
        await note.getByTestId('note-reaction').click();
        // 絵文字は画像で描かれ、文字が途中で切れるので、前半で探す
        await expect(page.getByText(/Would you like to add a/)).toBeVisible();
        await expect(page.getByTestId('modal-dialog-ok')).toBeVisible();
      },
    },
    {
      key: 'reactableRemoteReactionEnabled', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: { skip: 'リモートのカスタム絵文字のリアクションが要る。同名のローカル絵文字があれば押せるようになる (MkReactionsViewer.reaction.vue)。連合の無い単体の stack では作れない' },
    },
    {
      key: 'loadRawImages', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, request, baseURL, user }) => {
        const { noteId, files } = await imageNote(request, baseURL, user);
        // サムネイルと原寸の URL が違わないと、どちらを読んだか区別できない
        expect(files[0].thumbnailUrl, 'サムネイルの URL が原寸と同じ').not.toBe(files[0].url);
        const note = await openNote(page, baseURL, noteId);
        await expect(note.getByTestId('media-image').locator('img').first()).toHaveAttribute('src', files[0].url);
      },
    },
    {
      key: 'useReactionPickerForContextMenu', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, request, baseURL, user }) => {
        const text = runTag('pwctx');
        await postNote(request, user, text);
        const article = await profileNote(page, baseURL, user, text);
        // 右クリックで、ノートのメニューの代わりにリアクションの選択が開く
        await article.getByTestId('note-content').click({ button: 'right' });
        await expect(page.getByTestId('emoji-picker-search')).toBeVisible();
      },
    },
    {
      key: 'showAvailableReactionsFirstInNote', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: { skip: '押せるリアクションを前に並べ替える (MkReactionsViewer.vue)。押せないのはリモートのカスタム絵文字だけなので、並びの差を作るには連合が要る' },
    },
    {
      key: 'reactionsDisplaySize', route: ROUTE, control: { kind: 'radio', label: 'Big' }, value: 'large',
      effect: async ({ page, request, baseURL, user }) => {
        const note = await openNote(page, baseURL, await reactedNote(request, user));
        // 高さは既定 42px、Big で 52px (MkReactionsViewer.reaction.vue)
        await expect.poll(() => note.getByTestId('note-reaction').first().evaluate((el) => getComputedStyle(el).height)).toBe('52px');
      },
    },
    {
      key: 'limitWidthOfReaction', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page, request, baseURL, user }) => {
        const note = await openNote(page, baseURL, await reactedNote(request, user));
        // 既定では、リアクションの絵文字の画像に max-width: 70px が掛かる
        const icon = note.getByTestId('note-reaction').first().locator('img').first();
        await expect(icon).toBeVisible();
        await expect.poll(() => icon.evaluate((el) => getComputedStyle(el).maxWidth)).toBe('none');
      },
    },
    {
      key: 'mediaListWithOneImageAppearance', route: ROUTE, control: { kind: 'radio', label: 'Limit to 16:9' }, value: '16_9',
      effect: async ({ page, request, baseURL, user }) => {
        const { noteId } = await imageNote(request, baseURL, user);
        const note = await openNote(page, baseURL, noteId);
        const media = note.getByTestId('media-image');
        await expect(media).toBeVisible();
        // 画像 1 枚の一覧の縦横比は、一覧の要素の inline style に入る (既定では空)。1x1 の画像は 16:9 に広げられる
        const ratio = await media.evaluate((el) => (el.parentElement as HTMLElement).style.aspectRatio);
        expect(ratio, '一覧に縦横比が付かない').not.toBe('');
        expect(Number.parseFloat(ratio)).toBeCloseTo(16 / 9, 2);
      },
    },
    {
      key: 'showMediaListByGridInWideArea', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, request, baseURL, user }) => {
        const { noteId } = await imageNote(request, baseURL, user, { count: 2 });
        const note = await openNote(page, baseURL, noteId);
        const media = note.getByTestId('media-image');
        await expect(media).toHaveCount(2);
        // 幅が 500px 以上のとき、枚数によらず 4 列の格子になる (既定は 2 枚なら 2 列)
        await expect.poll(() => media.first().evaluate((el) => getComputedStyle(el.parentElement as HTMLElement).gridTemplateColumns.split(' ').length)).toBe(4);
      },
    },
    {
      key: 'nsfw', route: ROUTE, control: { kind: 'select', label: 'Display media marked as sensitive' }, value: 'ignore',
      effect: async ({ page, request, baseURL, user }) => {
        const { noteId } = await imageNote(request, baseURL, user, { sensitive: true });
        const note = await openNote(page, baseURL, noteId);
        await expect(note.getByTestId('media-image')).toBeVisible();
        // センシティブな画像も隠さずに出す
        await expect(note.getByTestId('media-image-hidden')).toHaveCount(0);
      },
    },
  ];
  for (const row of ROWS) test(prefTitle(row), coversPrefs(row.key), ({ page, request, baseURL }) => runPrefRow({ page, request, baseURL }, row));
});
