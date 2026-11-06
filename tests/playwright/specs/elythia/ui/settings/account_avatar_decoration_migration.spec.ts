/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// アカウントの設定 (#4056) のうち、/settings/avatar-decoration のアバターの装飾と、
// /settings/other の「Account Migration」の中の移行元の別名 (alsoKnownAs)。
//
// 装飾は管理者が登録したものと、カスタム絵文字 (#2975) の 2 種類。どちらも root の
// token で 1 度だけ作り、この spec の行で使い回す。一覧はページを開いたときに読むので、
// 作った後にページを読み込み直してから選ぶ。
//
// 外す行 (1 つ外す、全部外す) は、外した後の状態が既定 (何も着けていない) と同じなので、
// 変異の確かめ (set を省く) で効き目を区別できない。効き目は省き、i と画面の読み直し
// だけを確かめる。

import { readFileSync } from 'node:fs';
import { type APIRequestContext, expect, type Page, test } from '@playwright/test';
import { type Principal, randomUsername, signupUser } from '../../../../fixtures/auth';
import { pageBody } from '../../../../fixtures/route_check';
import { type AccountRow, type RowContext, runAccountRow } from '../../../../fixtures/settings_table';
import type { RootFixture } from '../../../../fixtures/ui_auth';
import { distinctPNG, uploadPNG } from '../../../../fixtures/ui_drive';
import { clickFolderButton } from '../../../../fixtures/ui_folder';
import { fillInputByLabel } from '../../../../fixtures/ui_input';
import { coversRoutes } from '../../../../fixtures/ui_route';
import { apiOk, runTag } from '../../../../fixtures/ui_timeline';

function root(): RootFixture {
  return JSON.parse(readFileSync('.auth/root.json', 'utf-8')) as RootFixture;
}

interface Decoration {
  id: string;
  name: string;
}

// 名前は worker ごとに 1 つ。行をまたいで同じ装飾と絵文字を使う
const DECORATION_NAME = runTag('pwdeco');
const EMOJI_NAME = `pwemo_${runTag('')}`;
let decoration: Promise<Decoration> | undefined;
let emoji: Promise<string> | undefined;

/** The avatar decoration the rows wear, registered by root once per worker. */
function ensureDecoration(request: APIRequestContext): Promise<Decoration> {
  decoration ??= (async () => {
    const admin = root();
    await apiOk(request, 'admin/avatar-decorations/create', {
      i: admin.token, name: DECORATION_NAME, description: '', url: `https://example.invalid/decoration/${DECORATION_NAME}.png`, roleIdsThatCanBeUsedThisDecoration: [],
    });
    const list = await apiOk(request, 'get-avatar-decorations', { i: admin.token }) as Decoration[];
    const found = list.find((d) => d.name === DECORATION_NAME);
    expect(found, `装飾「${DECORATION_NAME}」が一覧に無い`).toBeTruthy();
    return found!;
  })();
  // 落ちたときは持ち続けない (同じ worker の後の行が、同じ理由で落ち続けないように)
  decoration.catch(() => { decoration = undefined; });
  return decoration;
}

/** The local custom emoji the emoji row wears, added by root once per worker. */
function ensureEmoji(request: APIRequestContext, baseURL: string): Promise<string> {
  emoji ??= (async () => {
    const admin = root();
    const file = await uploadPNG(request, baseURL, admin.token, `${EMOJI_NAME}.png`, distinctPNG());
    await apiOk(request, 'admin/emoji/add', { i: admin.token, name: EMOJI_NAME, fileId: file.id });
    return EMOJI_NAME;
  })();
  emoji.catch(() => { emoji = undefined; });
  return emoji;
}

// アイコンの字形 (::before) が role の名前に入りうるので、ボタンは文字で取る
function button(page: Page, text: string) {
  return page.locator('button').filter({ hasText: new RegExp(`^\\s*${text}\\s*$`) });
}

/** The toggle of the "Flip" switch in the decoration dialog (outside the page body). */
function flipToggle(page: Page) {
  return page.locator('div').filter({ has: page.getByTestId('switch-toggle') }).filter({ hasText: /^\s*Flip\s*$/ }).getByTestId('switch-toggle');
}

async function decorationsSeenBy(request: APIRequestContext, user: Principal): Promise<{ id: string; emojiName?: string; flipH?: boolean }[]> {
  const other = await signupUser(request, randomUsername('pwdecob'));
  return (await apiOk(request, 'users/show', { i: other.token, userId: user.id })).avatarDecorations;
}

/**
 * Wears the decoration through the API before the page opens (a row's setup),
 * so that the page lists it under "Used" and the row's set changes only the setting.
 */
async function wear({ request, user }: RowContext): Promise<Decoration> {
  const deco = await ensureDecoration(request);
  await apiOk(request, 'i/update', { i: user.token, avatarDecorations: [{ id: deco.id }] });
  return deco;
}

async function expectInUse(page: Page, inUse: boolean): Promise<void> {
  await expect(pageBody(page).getByText('Used', { exact: true })).toHaveCount(inUse ? 1 : 0);
}

const REMOVED_SKIP = '外した後は何も着けていない既定の状態と同じで、変異の確かめで区別できない。着けたときの効き目は「avatarDecorations → attach」の行で見る';

const MOVE_FROM = 'Migrate another account to this one';

test.describe('settings: account (avatar decoration, migration)', () => {
  test.setTimeout(60_000);
  const ROWS: AccountRow[] = [
    {
      name: 'avatarDecorations → attach', route: '/settings/avatar-decoration',
      // 一覧はページを開いたときに読むので、装飾はページを開く前に作る
      setup: async ({ request }) => ensureDecoration(request),
      set: async ({ page }) => {
        await pageBody(page).getByText(DECORATION_NAME, { exact: true }).first().click();
        await button(page, 'Attach').click();
      },
      // 装飾の ID は set の中で初めて分かるので、i では件数を見て、ID は効き目で見る
      read: (me) => (me.avatarDecorations as unknown[]).length, expected: 1,
      shows: ({ page }) => expectInUse(page, true),
      effect: async ({ request, user }) => {
        const deco = await ensureDecoration(request);
        expect((await decorationsSeenBy(request, user)).map((d) => d.id)).toEqual([deco.id]);
      },
    },
    {
      name: 'avatarDecorations → attach emoji', route: '/settings/avatar-decoration',
      // 絵文字の一覧 (customEmojisMap) は起動時に読むので、ページを開く前に足す
      setup: async ({ request, baseURL }) => ensureEmoji(request, baseURL),
      set: async ({ page }) => {
        await button(page, 'Pick an emoji').click();
        await page.getByTestId('emoji-picker-search').fill(EMOJI_NAME);
        await page.getByTestId('emoji-picker-search-result').first().click();
        await button(page, 'Attach').click();
      },
      read: (me) => (me.avatarDecorations as { emojiName?: string }[]).map((d) => d.emojiName), expected: [EMOJI_NAME],
      shows: async ({ page }) => {
        await expectInUse(page, true);
        await expect(pageBody(page).getByText(`:${EMOJI_NAME}:`, { exact: true }).first()).toBeVisible();
      },
      effect: async ({ request, baseURL, user }) => {
        const name = await ensureEmoji(request, baseURL);
        expect((await decorationsSeenBy(request, user)).map((d) => d.emojiName)).toEqual([name]);
      },
    },
    {
      name: 'avatarDecorations → flipH true', route: '/settings/avatar-decoration',
      // 着けるのは前提 (setup)。変異の確かめでは左右の反転だけを省く
      setup: wear,
      set: async (ctx) => {
        // 着けている装飾は「Used」の欄にあり、DOM では下の一覧より前に来る
        await pageBody(ctx.page).getByText(DECORATION_NAME, { exact: true }).first().click();
        await flipToggle(ctx.page).click();
        await button(ctx.page, 'Update').click();
      },
      read: (me) => (me.avatarDecorations as { flipH?: boolean }[]).map((d) => d.flipH), expected: [true],
      shows: async ({ page }) => {
        await pageBody(page).getByText(DECORATION_NAME, { exact: true }).first().click();
        await expect(flipToggle(page)).toHaveAttribute('data-checked', 'true');
      },
      effect: async ({ request, user }) => {
        expect((await decorationsSeenBy(request, user)).map((d) => d.flipH)).toEqual([true]);
      },
    },
    {
      name: 'avatarDecorations → remove one', route: '/settings/avatar-decoration',
      setup: wear,
      set: async (ctx) => {
        await pageBody(ctx.page).getByText(DECORATION_NAME, { exact: true }).first().click();
        await button(ctx.page, 'Remove').click();
      },
      read: (me) => me.avatarDecorations, expected: [],
      shows: ({ page }) => expectInUse(page, false),
      effect: { skip: REMOVED_SKIP },
    },
    {
      name: 'avatarDecorations → remove all', route: '/settings/avatar-decoration',
      setup: wear,
      set: async (ctx) => {
        await button(ctx.page, 'Remove All').click();
        await ctx.page.getByTestId('modal-dialog-ok').click();
      },
      read: (me) => me.avatarDecorations, expected: [],
      shows: ({ page }) => expectInUse(page, false),
      effect: { skip: REMOVED_SKIP },
    },
    {
      name: 'alsoKnownAs → local alias', route: '/settings/other',
      setup: async ({ request }) => ({ alias: await signupUser(request, randomUsername('pwalias')), response: undefined as unknown }),
      set: async ({ page, data }) => {
        const d = data as { alias: Principal; response: unknown };
        await fillInputByLabel(page, 'Original Account #1', `@${d.alias.username}`);
        const saved = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/i/update' && (r.request().postData() ?? '').includes('alsoKnownAs'));
        await clickFolderButton(page, MOVE_FROM, 'Save');
        d.response = (await (await saved).json()).alsoKnownAs;
      },
      read: (me) => me.alsoKnownAs, expected: (data: unknown) => [(data as { alias: Principal }).alias.id],
      shows: async ({ page, data }) => {
        const { alias } = data as { alias: Principal };
        await expect(pageBody(page).locator('._selectable').filter({ has: page.getByText('Original Account #1', { exact: true }) }).locator('input')).toHaveValue(`@${alias.username}`);
      },
      effect: async ({ request, user, data }) => {
        const d = data as { alias: Principal; response: unknown };
        // 別名に入れた利用者は、この利用者へ移行できる。別名が無ければ 400 DESTINATION_ACCOUNT_FORBIDS
        // (internal/api/i/move.go)。移行は戻せないので、移行元は行ごとに作った使い捨ての利用者
        await apiOk(request, 'i/move', { i: d.alias.token, moveToAccount: `@${user.username}` });
        // #4059: i/update の応答の alsoKnownAs は ID へ解決されずに返る。画面はこの応答で $i を
        // 書き換えるので、/api/i (ID に解決して返す) と食い違う
        expect(d.response, '#4059: i/update の応答の alsoKnownAs が利用者の ID に解決されない').toEqual([d.alias.id]);
      },
      knownBug: '#4059',
    },
  ];
  for (const row of ROWS) test(row.name, coversRoutes(row.route), ({ page, request, baseURL }) => runAccountRow({ page, request, baseURL }, row));
});
