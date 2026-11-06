/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// アカウントの設定 (#4056) のうち、行の少ないページ (/settings/drive、/settings/mute-block) のもの。
// メールの設定は account_email.spec.ts。

import { expect, type Page, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../../fixtures/auth';
import { pageBody } from '../../../../fixtures/route_check';
import { type AccountRow, labeledSwitch, runAccountRow } from '../../../../fixtures/settings_table';
import { distinctPNG, uploadPNG } from '../../../../fixtures/ui_drive';
import { clickFolderButton, fillFolderTextarea } from '../../../../fixtures/ui_folder';
import { coversRoutes, gotoRoute } from '../../../../fixtures/ui_route';
import { apiOk, follow, postNote, runTag } from '../../../../fixtures/ui_timeline';

async function expectSwitch(page: Page, label: string, on: boolean): Promise<void> {
  await expect(await labeledSwitch(page, label)).toHaveAttribute('data-checked', String(on));
}

/** The textarea inside the folder whose header reads exactly `label`. */
function folderTextarea(page: Page, label: string) {
  return pageBody(page).locator('[role="group"]').filter({ has: page.getByTestId('folder-header').filter({ has: page.getByText(label, { exact: true }) }) }).last().locator('textarea');
}

// 自動でセンシティブにするスイッチは、ラベルの後ろに「Beta」の印が付く (drive.vue)。
// labeledSwitch はラベルの欄の文字全体で比べるので、印まで含めて書く
const AUTO_SENSITIVE = 'Automatic marking as sensitiveBeta';
const ALWAYS_NSFW = 'Mark as sensitive by default';
// 語はテストの名前にも入るので、固定の文字列にする (乱数だと、一覧を作るときと流すときで
// 名前が変わって、Playwright がテストを見つけられない)。利用者は行ごとに新しいので重ならない
const SOFT_WORD = 'pwsoftmutedword';
const HARD_WORD = 'pwhardmutedword';

test.describe('settings: account (drive, mute-block)', () => {
  test.setTimeout(60_000);
  const ROWS: AccountRow[] = [
    {
      name: 'alwaysMarkNsfw → true', route: '/settings/drive',
      set: async ({ page }) => (await labeledSwitch(page, ALWAYS_NSFW)).click(),
      read: (me) => me.alwaysMarkNsfw, expected: true,
      shows: ({ page }) => expectSwitch(page, ALWAYS_NSFW, true),
      effect: async ({ request, baseURL, user }) => {
        // 本家はプロフィールの alwaysMarkNsfw を見て、上げたファイルをセンシティブにする (DriveService)
        const file = await uploadPNG(request, baseURL, user.token, `${runTag('pwnsfw')}.png`, distinctPNG()) as { isSensitive?: boolean };
        expect(file.isSensitive, '#4057: 常にセンシティブにしても、アップロードしたファイルがセンシティブにならない').toBe(true);
      },
      knownBug: '#4057',
    },
    {
      name: 'autoSensitive → true', route: '/settings/drive',
      set: async ({ page }) => (await labeledSwitch(page, AUTO_SENSITIVE)).click(),
      read: (me) => me.autoSensitive, expected: true,
      shows: ({ page }) => expectSwitch(page, AUTO_SENSITIVE, true),
      effect: { skip: 'NSFW の判定 (サーバーの設定とモデル) に掛かったファイルだけに効き、この spec のスタックでは判定が動かない。#4057 で、backend がプロフィールのこの値を読んでいない疑いもある' },
    },
    {
      name: `mutedWords → [[${SOFT_WORD}]]`, route: '/settings/mute-block',
      set: async ({ page }) => {
        await fillFolderTextarea(page, 'Word mute', SOFT_WORD);
        await clickFolderButton(page, 'Word mute', 'Save');
      },
      read: (me) => me.mutedWords, expected: [[SOFT_WORD]],
      shows: ({ page }) => expect(folderTextarea(page, 'Word mute')).toHaveValue(SOFT_WORD),
      effect: async ({ page, request, baseURL }) => {
        // ワードミュートは frontend だけで効き、当たったノートは「<名前> said something」に畳まれる (MkNote.vue)
        const other = await signupUser(request, randomUsername('pwsoft'));
        await postNote(request, other, `about ${SOFT_WORD} today`);
        await gotoRoute(page, baseURL, `/@${other.username}/notes`);
        await expect(pageBody(page).getByText(/said something/).first()).toBeVisible({ timeout: 20_000 });
      },
    },
    {
      name: `hardMutedWords → [[${HARD_WORD}]]`, route: '/settings/mute-block',
      set: async ({ page }) => {
        await fillFolderTextarea(page, 'Hard word mute', HARD_WORD);
        await clickFolderButton(page, 'Hard word mute', 'Save');
      },
      read: (me) => me.hardMutedWords, expected: [[HARD_WORD]],
      shows: ({ page }) => expect(folderTextarea(page, 'Hard word mute')).toHaveValue(HARD_WORD),
      effect: async ({ request, user }) => {
        // ハードミュートは backend がタイムラインの応答から除く (internal/core/notesfilter/hardmute.go)。
        // 当たるノートの後に当たらないノートを出し、後者が届いてから前者が無いことを見る
        const other = await signupUser(request, randomUsername('pwhard'));
        await follow(request, user, other);
        const muted = await postNote(request, other, `about ${HARD_WORD} today`);
        const control = await postNote(request, other, runTag('pwctrl'));
        let ids: string[] = [];
        await expect.poll(async () => {
          ids = (await apiOk(request, 'notes/timeline', { i: user.token, limit: 30 }) as { id: string }[]).map((n) => n.id);
          return ids.includes(control);
        }, { message: '目印のノートがタイムラインに届かない', timeout: 15_000 }).toBe(true);
        expect(ids).not.toContain(muted);
      },
    },
  ];
  for (const row of ROWS) test(row.name, coversRoutes(row.route), ({ page, request, baseURL }) => runAccountRow({ page, request, baseURL }, row));
});
