/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定の表 (#4056): /settings/preferences の「General」の見出しの端末の設定を、
// 部品の操作 → 保存 → 再読み込みの後の表示 → 効き目、の順に確かめる。
//
// 効き目は、設定を変えていない (既定の値の) ときには必ず落ちる形で書く
// (`PW_SETTINGS_MUTATION=1` の変異の確かめ)。「無い」ことを見るときは、本来
// それがある画面にいることを先に確かめる。
//
// 「Device type」(overridedDeviceKind。MkPreferenceContainer の外) と
// 「Frequency of content retrieval」(pollingInterval。store の realtimeMode を切ると操作できる) は、
// prefs_unboxed.spec.ts で扱う。

import { readFileSync } from 'node:fs';
import { type APIRequestContext, expect, type Locator, type Page, test } from '@playwright/test';
import { type Principal, randomUsername, signupUser } from '../../../../fixtures/auth';
import { pageBody } from '../../../../fixtures/route_check';
import { coversPrefs, expectHappens, type PrefRow, prefTitle, runPrefRow } from '../../../../fixtures/settings_table';
import { distinctPNG, uploadPNG } from '../../../../fixtures/ui_drive';
import { gotoRoute } from '../../../../fixtures/ui_route';
import { apiOk, postNote, runTag, waitForApiBody } from '../../../../fixtures/ui_timeline';

const ROUTE = '/settings/preferences';

/** Posts a note with one PNG (sensitive when asked) and returns its id. */
async function imageNote(request: APIRequestContext, baseURL: string, user: Principal, sensitive: boolean): Promise<string> {
  const file = await uploadPNG(request, baseURL, user.token, 'pwpref.png', distinctPNG());
  if (sensitive) await apiOk(request, 'drive/files/update', { i: user.token, fileId: file.id, isSensitive: true });
  return postNote(request, user, null, { fileIds: [file.id] });
}

/** Opens /notes/<id> and returns the detailed note once its footer is shown. */
async function openNote(page: Page, baseURL: string, noteId: string): Promise<Locator> {
  await gotoRoute(page, baseURL, `/notes/${noteId}`);
  const note = pageBody(page).getByTestId('note-detailed');
  await expect(note.getByTestId('note-menu')).toBeVisible({ timeout: 20_000 });
  return note;
}

/** The follow button on the profile page of `other` (not followed yet). */
async function followButtonOf(page: Page, baseURL: string, other: Principal): Promise<Locator> {
  await gotoRoute(page, baseURL, `/@${other.username}`);
  const button = pageBody(page).locator('button').filter({ hasText: /^\s*Follow\s*$/ });
  await expect(button).toHaveCount(1, { timeout: 20_000 });
  return button;
}

test.describe('settings: preferences (General)', () => {
  test.setTimeout(60_000);
  const ROWS: PrefRow[] = [
    {
      key: 'showTitlebar', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page }) => {
        // タイトルバー (ui/_common_/titlebar.vue) は素のクラスも testid も持たない。
        // universal UI でページ本体を含む領域の直前に置かれ、サーバーのアイコンと名前を出す
        await expect.poll(() => page.evaluate(() => {
          const containers = document.querySelectorAll('._pageContainer');
          const body = containers[containers.length - 1];
          if (!body) return -1;
          return Array.from(document.querySelectorAll<HTMLElement>('div[style*="flex-shrink"]')).filter((el) => {
            const rect = el.getBoundingClientRect();
            return el.nextElementSibling?.contains(body) === true && rect.top === 0 && rect.height > 0
              && el.querySelector('img') !== null && (el.textContent ?? '').trim() !== '';
          }).length;
        }), { message: 'ページの上にタイトルバーが出ない' }).toBe(1);
      },
    },
    {
      key: 'showAvatarDecorations', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page, request, baseURL, user }) => {
        // デコレーションは管理者しか作れないので root で作り、行の利用者に付ける
        const root = JSON.parse(readFileSync('.auth/root.json', 'utf-8')) as { token: string };
        const name = randomUsername('pwdeco');
        const deco = await apiOk(request, 'admin/avatar-decorations/create', {
          i: root.token, name, description: '', url: `https://example.invalid/decoration/${name}.png`, roleIdsThatCanBeUsedThisDecoration: [],
        });
        await apiOk(request, 'i/update', { i: user.token, avatarDecorations: [{ id: deco.id, angle: 0, flipH: false, offsetX: 0, offsetY: 0 }] });
        // 装飾が本当に付いたことを先に確かめる (付いていなければ、出ないのは設定と関係ない)
        expect((await apiOk(request, 'i', { i: user.token })).avatarDecorations).toHaveLength(1);
        await gotoRoute(page, baseURL, `/@${user.username}`);
        const avatars = pageBody(page).locator(`._noSelect[title="${user.username}"]`);
        await expect(avatars.first()).toBeVisible({ timeout: 20_000 });
        // デコレーションは MkAvatar の root の直下の img。アバター本体は MkImgWithBlurhash の div の中にある
        await expect(pageBody(page).locator(`._noSelect[title="${user.username}"] > img`)).toHaveCount(0);
      },
    },
    {
      key: 'alwaysConfirmFollow', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page, request, baseURL }) => {
        const other = await signupUser(request, randomUsername('pwfolc'));
        const button = await followButtonOf(page, baseURL, other);
        // 確認のダイアログを挟まずに、押しただけでフォローの要求が出る
        const sent = waitForApiBody(page, 'following/create', () => true, 10_000);
        await button.click();
        await expectHappens(sent, '押しただけではフォローの要求が出ない');
        await expect(page.getByTestId('modal-dialog-ok')).toHaveCount(0);
      },
    },
    {
      key: 'highlightSensitiveMedia', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, request, baseURL, user }) => {
        const note = await openNote(page, baseURL, await imageNote(request, baseURL, user, true));
        const media = note.getByTestId('media-image');
        await expect(media).toBeVisible();
        // 強調は ::after の内側の影 (MkMediaImage.vue の .sensitive)。クラス名はハッシュになるので、効いた結果で見る
        await expect.poll(() => media.evaluate((el) => getComputedStyle(el, '::after').boxShadow), { message: 'センシティブな画像に枠が付かない' }).toContain('inset');
      },
    },
    {
      key: 'confirmWhenRevealingSensitiveMedia', route: ROUTE, control: { kind: 'switch', on: true }, value: true,
      effect: async ({ page, request, baseURL, user }) => {
        const note = await openNote(page, baseURL, await imageNote(request, baseURL, user, true));
        await note.getByTestId('media-image-hidden').click();
        await expect(page.getByText('This might be a sensitive media. Are you sure to reveal?')).toBeVisible();
        await expect(page.getByTestId('modal-dialog-ok')).toBeVisible();
      },
    },
    {
      key: 'advancedMfm', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page, request, baseURL, user }) => {
        const word = runTag('pwxtwo');
        const note = await openNote(page, baseURL, await postNote(request, user, `$[x2 ${word}]`));
        await expect(note.getByText(word)).toBeVisible();
        // 高度な MFM を切ると、x2 の span に素のクラス mfm-x2 が付かない (mfm-renderer.ts)
        await expect(note.locator('.mfm-x2')).toHaveCount(0);
      },
    },
    {
      key: 'enableInfiniteScroll', route: ROUTE, control: { kind: 'switch', on: false }, value: false,
      effect: async ({ page, request, baseURL, user }) => {
        // プロフィールのノートの一覧は 10 件ずつ読む (pages/user/index.timeline.vue)。12 件で 2 ページ目ができる
        const tag = runTag('pwinf');
        for (let i = 0; i < 12; i++) await postNote(request, user, `${tag} n${String(i).padStart(2, '0')}`);
        await gotoRoute(page, baseURL, `/@${user.username}/notes`);
        const body = pageBody(page);
        await expect(body.getByText(`${tag} n11`, { exact: true })).toBeVisible({ timeout: 20_000 });
        const more = body.getByRole('button', { name: 'Load more' });
        await more.scrollIntoViewIfNeeded();
        await expect(more).toBeVisible();
        // 自動の読み込み (v-appear) は、ボタンが見えた時点で走る。待っても次のページが出ないことを見る
        await page.waitForTimeout(2_000);
        await expect(body.getByText(`${tag} n00`, { exact: true })).toHaveCount(0);
        await expect(more).toBeVisible();
      },
    },
    {
      key: 'emojiStyle', route: ROUTE, control: { kind: 'radio', label: 'Native' }, value: 'native',
      effect: async ({ page }) => {
        // 設定のページの見本 (🍮) が、画像でなく文字で出る
        await expect(page.locator('img[src*="/twemoji/"]')).toHaveCount(0);
        await expect(page.getByText('🍮').first()).toBeVisible();
      },
    },
  ];
  for (const row of ROWS) test(prefTitle(row), coversPrefs(row.key), ({ page, request, baseURL }) => runPrefRow({ page, request, baseURL }, row));
});
