/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// アカウントの設定 (#4056) のうち、/settings/profile のもの。入力欄は manualSave で、
// 入力すると欄の下に出る「Save」で送る (送ると profile.vue の save() が 8 項目を
// まとめて送る)。効き目は、別の利用者の users/show か、プロフィールやノートの描画で
// 確かめる。
//
// アバターとバナーを外す行は、外した後の状態が既定 (付けていない) と同じなので、
// 変異の確かめ (set を省く) で効き目を区別できない。効き目は省き、i と画面の
// 読み直しだけを確かめる。

import { expect, type Locator, type Page, test } from '@playwright/test';
import type { DriveFile } from '../../../../fixtures/files';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { pageBody, waitForMountedPage } from '../../../../fixtures/route_check';
import { type AccountRow, expectControl, labeledSwitch, runAccountRow, setControl } from '../../../../fixtures/settings_table';
import { clickDriveItem, distinctPNG, uploadPNG } from '../../../../fixtures/ui_drive';
import { clickMenuItem, clickOnlyTestId, waitForMenuItem } from '../../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../../fixtures/ui_route';
import { apiOk, follow, postNote, runTag } from '../../../../fixtures/ui_timeline';

/**
 * The innermost block (MkInput, MkTextarea, MkSelect) whose first child is a
 * label div reading exactly `label`.
 */
async function labeled(page: Page, label: string): Promise<Locator> {
  const n = await pageBody(page).evaluate((body, text) => {
    for (const el of Array.from(body.querySelectorAll('[data-pw-labeled]'))) el.removeAttribute('data-pw-labeled');
    // SearchMarker の div も部品を最初の子に持つので、最初の子が操作できる要素を含まない
    // (ラベルだけの div である) ものに絞る
    const hits = Array.from(body.querySelectorAll<HTMLElement>('div')).filter((el) => {
      const first = el.firstElementChild;
      return first?.tagName === 'DIV' && first.querySelector('input, textarea, [tabindex]') == null && (first.textContent ?? '').trim() === text;
    });
    if (hits.length !== 1) return hits.length;
    hits[0].setAttribute('data-pw-labeled', '');
    return 1;
  }, label);
  expect(n, `ラベルが「${label}」の部品 (ちょうど 1 つ)`).toBe(1);
  return pageBody(page).locator('[data-pw-labeled]');
}

/** The MkFolder (role="group") whose header has a part reading exactly `label`. */
async function folderGroup(page: Page, label: string): Promise<Locator> {
  const n = await pageBody(page).evaluate((body, text) => {
    for (const el of Array.from(body.querySelectorAll('[data-pw-folder]'))) el.removeAttribute('data-pw-folder');
    const headers = Array.from(body.querySelectorAll('[data-testid="folder-header"]'))
      .filter((h) => Array.from(h.querySelectorAll('*')).some((d) => (d.textContent ?? '').trim() === text));
    if (headers.length !== 1) return headers.length;
    headers[0].closest('[role="group"]')?.setAttribute('data-pw-folder', '');
    return 1;
  }, label);
  expect(n, `見出しが「${label}」のフォルダ (ちょうど 1 つ)`).toBe(1);
  return pageBody(page).locator('[data-pw-folder]');
}

// アイコンの字形 (::before) が role の名前に入りうるので、ボタンは文字で取る
function button(scope: Locator, text: string): Locator {
  return scope.locator('button').filter({ hasText: new RegExp(`^\\s*${text}\\s*$`) });
}

/** Types `value` into the manualSave input labeled `label` and presses its Save. */
async function saveInput(page: Page, label: string, value: string): Promise<void> {
  const root = await labeled(page, label);
  await root.locator('input, textarea').first().fill(value);
  await button(root, 'Save').click();
}

async function expectInput(page: Page, label: string, value: string): Promise<void> {
  await expect((await labeled(page, label)).locator('input, textarea').first()).toHaveValue(value);
}

async function showOf(request: Parameters<typeof apiOk>[0], userId: string): Promise<Record<string, any>> {
  const other = await signupUser(request, randomUsername('pwprof'));
  return apiOk(request, 'users/show', { i: other.token, userId });
}

// 値はテストの名前にも入るので、固定の文字列にする (乱数だと、一覧を作るときと流すときで
// 名前が変わって、Playwright がテストを見つけられない)。利用者は行ごとに新しいので重ならない
const NAME = 'pw-name-value';
const BIO = 'pw-bio-value';
const LOCATION = 'pw-location-value';
const BIRTHDAY = '2000-01-02';
const FOLLOWED_MESSAGE = 'pw-followed-message';
const FIELD = { name: 'pw-field-label', value: 'pw-field-value' };
const CAT = 'Mark this account as a cat';
const BOT = 'Mark this account as a bot';
const METADATA = 'Edit additional Information';

// アバターとバナーの行が、set で上げたファイルを read と効き目に渡す
const files: Record<string, DriveFile | undefined> = {};

/** Uploads a PNG as the row's user and picks it from the drive through the "Change …" menu. */
async function pickImage(ctx: Parameters<AccountRow['set']>[0], key: 'avatar' | 'banner', buttonText: string): Promise<void> {
  const file = await uploadPNG(ctx.request, ctx.baseURL, ctx.user.token, `${runTag(`pw${key}`)}.png`, distinctPNG());
  files[key] = file;
  await button(pageBody(ctx.page), buttonText).click();
  await clickMenuItem(ctx.page, 'From Drive');
  await clickDriveItem(ctx.page, 'drive-file', file.name);
  await clickOnlyTestId(ctx.page, 'modal-window-ok');
}

/** Sets the image through the API, reloads, and picks "Remove" from the "Change …" menu. */
async function removeImage(ctx: Parameters<AccountRow['set']>[0], key: 'avatarId' | 'bannerId', buttonText: string): Promise<void> {
  const file = await uploadPNG(ctx.request, ctx.baseURL, ctx.user.token, `${runTag('pwremove')}.png`, distinctPNG());
  await apiOk(ctx.request, 'i/update', { i: ctx.user.token, [key]: file.id });
  // メニューの「Remove」は、画面の $i が画像を持っているときだけ出る。読み込み直して $i を取り直す
  await ctx.page.reload({ waitUntil: 'domcontentloaded' });
  await waitForMountedPage(ctx.page);
  await button(pageBody(ctx.page), buttonText).click();
  await clickMenuItem(ctx.page, 'Remove');
}

/** Asserts whether the "Change …" menu offers "Remove" (it does only while an image is set). */
async function expectRemoveOffered(page: Page, buttonText: string, offered: boolean): Promise<void> {
  await button(pageBody(page), buttonText).click();
  await waitForMenuItem(page, 'From Drive');
  await expect(page.locator('[role="menu"] [role^="menuitem"]').filter({ hasText: /^\s*Remove\s*$/ })).toHaveCount(offered ? 1 : 0);
  await page.keyboard.press('Escape');
}

test.describe('settings: account (profile)', () => {
  test.setTimeout(60_000);
  const ROWS: AccountRow[] = [
    {
      name: `name → ${NAME}`, route: '/settings/profile',
      set: ({ page }) => saveInput(page, 'Name', NAME),
      read: (me) => me.name, expected: NAME,
      shows: ({ page }) => expectInput(page, 'Name', NAME),
      effect: async ({ request, user }) => expect((await showOf(request, user.id)).name).toBe(NAME),
    },
    {
      name: `description → ${BIO}`, route: '/settings/profile',
      set: ({ page }) => saveInput(page, 'Bio', BIO),
      read: (me) => me.description, expected: BIO,
      shows: ({ page }) => expectInput(page, 'Bio', BIO),
      effect: async ({ request, user }) => expect((await showOf(request, user.id)).description).toBe(BIO),
    },
    {
      name: `location → ${LOCATION}`, route: '/settings/profile',
      set: ({ page }) => saveInput(page, 'Location', LOCATION),
      read: (me) => me.location, expected: LOCATION,
      shows: ({ page }) => expectInput(page, 'Location', LOCATION),
      effect: async ({ page, baseURL, user }) => {
        // プロフィールの「Location」の欄に出る (pages/user/home.vue)
        await gotoRoute(page, baseURL, `/@${user.username}`);
        await expect(pageBody(page).getByText(LOCATION, { exact: true })).toBeVisible({ timeout: 20_000 });
      },
    },
    {
      name: `birthday → ${BIRTHDAY}`, route: '/settings/profile',
      set: ({ page }) => saveInput(page, 'Birthday', BIRTHDAY),
      read: (me) => me.birthday, expected: BIRTHDAY,
      shows: ({ page }) => expectInput(page, 'Birthday', BIRTHDAY),
      effect: async ({ request, user }) => expect((await showOf(request, user.id)).birthday).toBe(BIRTHDAY),
    },
    {
      name: 'lang → ja-JP', route: '/settings/profile',
      set: async ({ page }) => setControl(page, await labeled(page, 'Language'), { kind: 'select', label: '日本語 (日本)' }),
      read: (me) => me.lang, expected: 'ja-JP',
      shows: async ({ page }) => expectControl(await labeled(page, 'Language'), { kind: 'select', label: '日本語 (日本)' }),
      // 画面には出ない (home.vue に表示が無い) ので、他の利用者から見た値で確かめる
      effect: async ({ request, user }) => expect((await showOf(request, user.id)).lang).toBe('ja-JP'),
    },
    {
      name: `fields → ${FIELD.name}: ${FIELD.value}`, route: '/settings/profile',
      set: async ({ page }) => {
        const group = await folderGroup(page, METADATA);
        await group.locator('input[placeholder="Label"]').first().fill(FIELD.name);
        await group.locator('input[placeholder="Content"]').first().fill(FIELD.value);
        await button(group, 'Save').click();
      },
      read: (me) => me.fields, expected: [FIELD],
      shows: async ({ page }) => {
        const group = await folderGroup(page, METADATA);
        await expect(group.locator('input[placeholder="Label"]').first()).toHaveValue(FIELD.name);
        await expect(group.locator('input[placeholder="Content"]').first()).toHaveValue(FIELD.value);
      },
      effect: async ({ request, user }) => expect((await showOf(request, user.id)).fields).toEqual([FIELD]),
    },
    {
      name: `followedMessage → ${FOLLOWED_MESSAGE}`, route: '/settings/profile',
      set: ({ page }) => saveInput(page, 'Message when you are followed', FOLLOWED_MESSAGE),
      read: (me) => me.followedMessage, expected: FOLLOWED_MESSAGE,
      shows: ({ page }) => expectInput(page, 'Message when you are followed', FOLLOWED_MESSAGE),
      effect: async ({ request, user }) => {
        // フォローした相手にだけ見える (internal/api/userrelation/resolver.go)
        const other = await signupUser(request, randomUsername('pwfmsg'));
        await follow(request, other, user);
        const shown = await apiOk(request, 'users/show', { i: other.token, userId: user.id });
        expect(shown.followedMessage).toBe(FOLLOWED_MESSAGE);
      },
    },
    {
      name: 'isCat → true', route: '/settings/profile',
      set: async ({ page }) => (await labeledSwitch(page, CAT)).click(),
      read: (me) => me.isCat, expected: true,
      shows: async ({ page }) => expect(await labeledSwitch(page, CAT)).toHaveAttribute('data-checked', 'true'),
      effect: async ({ page, request, baseURL, user }) => {
        // 猫の投稿は、本文の「na」が「nya」で描かれる (mfm-renderer.ts の nyaize)。耳はクラス名が
        // 本番でハッシュになるので見ない
        const noteId = await postNote(request, user, `banana ${runTag('pwcat')}`);
        await gotoRoute(page, baseURL, `/notes/${noteId}`);
        await expect(pageBody(page).getByTestId('note-detailed')).toContainText('banyanya', { timeout: 20_000 });
      },
    },
    {
      name: 'isBot → true', route: '/settings/profile',
      set: async ({ page }) => (await labeledSwitch(page, BOT)).click(),
      read: (me) => me.isBot, expected: true,
      shows: async ({ page }) => expect(await labeledSwitch(page, BOT)).toHaveAttribute('data-checked', 'true'),
      effect: async ({ page, baseURL, user }) => {
        // プロフィールの名前の横にロボットのアイコンが出る (pages/user/home.vue)
        await gotoRoute(page, baseURL, `/@${user.username}`);
        await expect(pageBody(page).locator('i.ti-robot').first()).toBeVisible({ timeout: 20_000 });
      },
    },
    {
      name: 'avatarId → uploaded file', route: '/settings/profile',
      set: (ctx) => pickImage(ctx, 'avatar', 'Change avatar'),
      read: (me) => me.avatarId != null && me.avatarId === files.avatar?.id, expected: true,
      shows: ({ page }) => expectRemoveOffered(page, 'Change avatar', true),
      effect: async ({ request, user }) => {
        // 付けていなければ identicon (/identicon/…) が返る (internal/entity/user.go の UserLite)
        expect((await showOf(request, user.id)).avatarUrl).not.toContain('/identicon/');
      },
    },
    {
      name: 'avatarId → null', route: '/settings/profile',
      set: (ctx) => removeImage(ctx, 'avatarId', 'Change avatar'),
      read: (me) => me.avatarId, expected: null,
      shows: ({ page }) => expectRemoveOffered(page, 'Change avatar', false),
      effect: { skip: '外した後は付けていない既定の状態 (identicon) と同じで、変異の確かめで区別できない。付けたときの効き目は「avatarId → uploaded file」の行で見る' },
    },
    {
      name: 'bannerId → uploaded file', route: '/settings/profile',
      set: (ctx) => pickImage(ctx, 'banner', 'Change banner'),
      read: (me) => me.bannerId != null && me.bannerId === files.banner?.id, expected: true,
      shows: ({ page }) => expectRemoveOffered(page, 'Change banner', true),
      effect: async ({ request, user }) => {
        expect((await showOf(request, user.id)).bannerUrl).toEqual(expect.any(String));
      },
    },
    {
      name: 'bannerId → null', route: '/settings/profile',
      set: (ctx) => removeImage(ctx, 'bannerId', 'Change banner'),
      read: (me) => me.bannerId, expected: null,
      shows: ({ page }) => expectRemoveOffered(page, 'Change banner', false),
      effect: { skip: '外した後はバナーの無い既定の状態と同じで、変異の確かめで区別できない。付けたときの効き目は「bannerId → uploaded file」の行で見る' },
    },
  ];
  for (const row of ROWS) test(row.name, coversRoutes('/settings/profile'), ({ page, request, baseURL }) => runAccountRow({ page, request, baseURL }, row));
});
