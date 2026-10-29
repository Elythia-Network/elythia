/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// プロフィールページ (`pages/user/home.vue`) のアイコンを押すと、既存の
// `MkLightbox` で拡大表示されるかを見る spec (#3124)。
//
// **`specs/elythia/` に置く。** upstream のプロフィールのアイコンは押せない素の
// 画像なので、`specs/upstream` だけを回す `make playwright-ts-test`
// (backend = ts) の対象に入れてはいけない。
//
// 見るのは issue の完了条件:
//   1. 押せることが見た目で分かる (`cursor: pointer`)
//   2. クリックでライトボックスが開き、**アイコンと同じ画像**が拡大表示される
//   3. Esc で閉じられる
//   4. キーボード (フォーカスして Enter) でも開く
//
// **CSS module のクラスを selector に使わない。** production build では
// ハッシュ化されるので `[class*="root"]` のような selector は当たらない。
// ライトボックスは img の実寸で見分ける — `MkLightbox.item.vue` の `.content` は
// `width: 100%; height: 100%` なので、元画像の解像度に関わらず box が
// ライトボックスいっぱいに広がる。裏を返すと**幅だけでは src が 404 でも真に
// なる**ので、`naturalWidth` も併せて見る。
//
// **`.avatar` は CSS module ではない共有クラス**で、DOM にこの名前が出るのは
// 7 ファイル (数え方: `grep -rhoE 'class="[^"]*\bavatar\b[^"]*"' src --include=*.vue`
// のうち `$style.` を使わない静的クラス。この 7 に `home.vue` 自身を含む)。
// `WidgetUserList` のようにウィジェット欄へ出るものがあるので、素の
// `page.locator('.avatar')` は strict mode violation で即死しうる。
// プロフィールページの root (`.ftskorzw`) で絞る。
//
// この spec は画像の**解像度**は見ていない (リモートのアイコンが高さ 320px に
// 制限される件は仕様として受け入れた)。

import { readFileSync } from 'node:fs';
import { expect, test, type Page } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

const AVATAR = '.ftskorzw .avatar';

// プロフィールのアイコンと、**同じ `src` を持つ拡大表示の img** の数を返す。
//
// 件数ではなく「アイコンの 2 倍以上の幅で描かれている img」を数えるのが要点。
// プロフィールページには同じ利用者のノートが並び、そこにも同じアイコンが出る
// ので、単純な総数だとタイムラインが後から描画されただけで動いてしまう
// (ノート側のアイコンはどれも本体より小さいので、この数は動かない)。
//
// **この突き合わせは既定の設定に依存する。** `disableShowingAnimatedImages` /
// `dataSaver.avatar` を有効にすると `MkAvatar` 側だけ `getStaticImageUrl` を
// 通った URL になり、ライトボックス側 (`user.avatarUrl` をそのまま渡す) と
// 一致しなくなる。これは upstream の `MkMediaList` も同じ作り (サムネイルは
// 静止画、ライトボックスは原本) で、既定値のまま走る CI では問題にならない。
//
// **当たらなかったセレクタは名前を出して落とす。** null に潰すと、upstream が
// クラス名を変えたときに「どれが当たらなかったのか」が分からない失敗になる。
async function avatarMetrics(page: Page) {
  return page.evaluate((sel) => {
    const avatarRoot = document.querySelector(sel);
    if (avatarRoot == null) throw new Error(`セレクタが当たらない: ${sel}`);
    // `MkAvatar` の中の最初の img が本体。装飾 (`.decoration`) はその後ろに
    // 並ぶうえ src が別なので、下の src 一致でも拾われない。
    const avatarImg = avatarRoot.querySelector('img');
    if (avatarImg == null) throw new Error(`${sel} に img が無い`);

    const avatarWidth = avatarImg.getBoundingClientRect().width;
    const enlarged = Array.from(document.querySelectorAll('img'))
      .filter((img) => img.src === avatarImg.src)
      .filter((img) => img.getBoundingClientRect().width >= avatarWidth * 2);

    return {
      avatarWidth,
      cursor: window.getComputedStyle(avatarRoot).cursor,
      enlargedCount: enlarged.length,
      // 絵が実際に出ているか。`.content` は box が常に大きいので、これが無いと
      // 「開いたが画像が壊れている」を検出できない。
      enlargedLoadedCount: enlarged.filter((img) => img.naturalWidth > 0).length,
    };
  }, AVATAR);
}

async function enlargedCount(page: Page): Promise<number> {
  return (await avatarMetrics(page)).enlargedCount;
}

test.describe('UI: /@user のアイコンを押すと拡大表示される', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(90_000);

  test('クリックと Enter でライトボックスが開き、Esc で閉じる', async ({ page, baseURL }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/@${root.username}`, { waitUntil: 'domcontentloaded' });

    const avatar = page.locator(AVATAR);
    await expect(avatar).toBeVisible({ timeout: 20_000 });
    // 画像が載る前に測ると幅 0 になり、「拡大されていない」と区別が付かない。
    await expect(avatar.locator('img').first()).toBeVisible({ timeout: 20_000 });

    // --- 1. 押せることが見た目で分かり、まだ開いていない ---
    const before = await avatarMetrics(page);
    expect(before.avatarWidth).toBeGreaterThan(0);
    expect(before.cursor).toBe('pointer');
    expect(before.enlargedCount).toBe(0);

    // --- 2. クリックで開く ---
    // **デコード完了まで含めて poll する。** `.content` は `width/height: 100%` なので
    // img が DOM に入った瞬間に box はビューポート幅になり、幅だけの判定は「まだ
    // デコードされていない img」でも成立する。そこで naturalWidth を 1 回だけ見ると、
    // キャッシュ済みでもロードが数 ms 遅れただけで落ちる。
    await avatar.click();
    await expect.poll(async () => {
      const m = await avatarMetrics(page);
      return { enlarged: m.enlargedCount, loaded: m.enlargedLoadedCount };
    }, { timeout: 15_000 }).toEqual({ enlarged: 1, loaded: 1 });

    // --- 3. Esc で閉じる ---
    await page.keyboard.press('Escape');
    await expect.poll(() => enlargedCount(page), { timeout: 15_000 }).toBe(0);

    // --- 4. キーボードでも開く ---
    // `MkAvatar` の root は span なので、`tabindex="0"` が無いと focus できず
    // ここで落ちる。あわせて、閉じたときに再入ガードが解けていることも見ている
    // (解けていないと 2 回目は開かない)。
    await avatar.focus();
    await expect.poll(() => page.evaluate((sel) => {
      const el = document.activeElement;
      return el != null && el.matches(sel);
    }, AVATAR), { timeout: 5_000 }).toBe(true);

    await page.keyboard.press('Enter');
    await expect.poll(() => enlargedCount(page), { timeout: 15_000 }).toBe(1);
  });
});
