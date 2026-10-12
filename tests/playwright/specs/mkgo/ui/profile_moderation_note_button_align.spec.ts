/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /@target のプロフィールで「モデレーションノートを追加する」ボタンが
// 中央揃えになるかを幾何で見る spec (#2926)。
//
// `pages/user/home.vue` の `@container (max-width: 500px)` ブロックは
// `.avatar` (margin: auto) / `.roles` (justify-content: center) /
// `.description` (text-align: center) を個別に中央へ寄せているが、
// `.moderationNote` にだけ指定が無く、`MkButton` は
// `display: block; width: max-content` なので左端に取り残されていた。
// 親に `text-align: center` を足しても動かない (ブロック要素なので)
// 型なので、CSS の存在ではなく**実際の位置**を測る。
//
// **`specs/mkgo/` に置く。** upstream Misskey は今もこれを直しておらず、
// `make playwright-ts-test` (backend = ts) は `specs/upstream` だけを
// 対象にするので、そちらでは実行されない。
//
// 見るのは issue の完了条件 3 つ:
//   1. コンテナ幅 500px 以下でボタンが中央に来る
//   2. 既定幅では従来どおり左 (154px 起点) のまま
//   3. 編集中の MkTextarea の幅が変わらない (= .moderationNote を
//      flex コンテナにしていない)
//
// **3 つとも変異させて落ちることを実測した** (数値は Received):
//   1. 修正前のビルド (`2026.9.0-mk.8d`) に当てる            -> 101.53px ずれ
//   2. `margin: 0 auto` を container query の外へ漏らす      -> 240.53px ずれ
//   3. `.moderationNote` を flex + justify-content: center   -> 11.31px 縮む
// 2 と 3 は `page.addStyleTag` で注入して確認した (再ビルド不要)。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { signupUser } from '../../../fixtures/auth';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';

// 対象要素の box を測る。`.moderationNote` は margin だけで padding /
// border を持たないので、border box の中心がそのまま「中央」になる。
//
// **container 幅も返す。** 分岐を決めるのは `.ftskorzw`
// (`style="container-type: inline-size"`) の inline size であって
// `.moderationNote` の幅ではない。後者は左右 16px の margin を持つので、
// container が 501-532px でも 500 以下に見える。どちらの分岐を見ているかを
// 取り違えると診断が誤誘導になる。
// **当たらなかったセレクタは名前を出して落とす。** 3 つとも null に潰すと、
// upstream がクラス名を変えたときに「どれが当たらなかったのか」が分からない
// 失敗になる。
async function measure(page: import('@playwright/test').Page) {
  return page.evaluate(() => {
    const container = document.querySelector('.ftskorzw');
    if (container == null) throw new Error('セレクタが当たらない: .ftskorzw');
    const note = document.querySelector('.moderationNote');
    if (note == null) throw new Error('セレクタが当たらない: .moderationNote');
    const button = note.querySelector('button');
    if (button == null) throw new Error('セレクタが当たらない: .moderationNote button');
    const n = note.getBoundingClientRect();
    const b = button.getBoundingClientRect();
    return {
      // **container query が見るのは content box。** `clientWidth` は
      // content + padding だが `.ftskorzw` は padding も border も持たないので
      // 一致する。実測は narrow 376 / wide 800 で閾値 500 から十分離れている。
      containerWidth: container.clientWidth,
      noteLeft: n.left,
      noteWidth: n.width,
      noteCenter: n.left + n.width / 2,
      buttonLeft: b.left,
      buttonWidth: b.width,
      buttonCenter: b.left + b.width / 2,
    };
  });
}

test.describe('UI: /@user のモデレーションノート追加ボタンの位置', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(90_000);

  test('コンテナ幅で中央揃え / 左寄せが切り替わり、編集中の textarea は幅いっぱいのまま', async ({
    page,
    baseURL,
    request,
  }) => {
    // moderationNote が空の user を作る (= 追加ボタンが出る状態)。root 自身を
    // 使うと他の spec が note を書いていた場合に MkTextarea 側が出る。
    const target = await signupUser(request, `pwmb${Date.now().toString().slice(-9)}`);
    expect(target.id).toBeTruthy();

    // ボタンは iAmModerator でしか出ないので root で入る。
    await uiSigninAsRoot(page, baseURL, root);

    // --- 1. コンテナ幅 500px 以下: 中央揃え ---
    // `.ftskorzw` は inline-size コンテナなので、viewport を狭めれば
    // そのまま @container の分岐が切り替わる。
    await page.setViewportSize({ width: 400, height: 900 });
    await page.goto(`${baseURL}/@${target.username}`, { waitUntil: 'domcontentloaded' });
    await expect(page.locator('.moderationNote button')).toBeVisible({ timeout: 20_000 });

    const narrow = await measure(page);
    // container が実際に 500px 以下の分岐に入っていることを先に確かめる。
    // ここが 500 を超えていると、以降の assert は別の分岐を見ている。
    expect(narrow.containerWidth).toBeLessThanOrEqual(500);
    // ボタンは中身なり (width: max-content) なので、左寄せなら中心が
    // 大きくずれる。1px は subpixel 丸めの許容。
    expect(Math.abs(narrow.buttonCenter - narrow.noteCenter)).toBeLessThanOrEqual(1);
    // 中央揃えと「ボタンが幅いっぱいに広がっただけ」を区別する。
    expect(narrow.buttonWidth).toBeLessThan(narrow.noteWidth);

    // --- 3. 編集中の MkTextarea は幅いっぱいのまま ---
    // `.moderationNote` を flex コンテナにして中央寄せする案だと、ここが
    // 内容幅に縮む。`margin: 0 auto` を選んだ理由そのものを固定する。
    //
    // **これは現行実装の回帰では落ちない番人。** ブロック整形文脈の子は
    // 親の幅いっぱいになるので、`margin: 0 auto` を消しても通る。落ちるのは
    // 将来 flex 化で書き直したときで、`page.addStyleTag` で
    // `.moderationNote { display: flex; justify-content: center }` を注入すると
    // 差が 11.31px になって落ちることを実測してある。
    await page.locator('.moderationNote button').click();
    await expect(page.locator('.moderationNote textarea')).toBeVisible({ timeout: 10_000 });
    const editing = await page.evaluate(() => {
      const note = document.querySelector('.moderationNote');
      if (note == null) throw new Error('セレクタが当たらない: .moderationNote');
      const child = note.firstElementChild;
      if (child == null) throw new Error('.moderationNote に子要素が無い');
      return {
        noteWidth: note.getBoundingClientRect().width,
        childWidth: child.getBoundingClientRect().width,
      };
    });
    expect(Math.abs(editing.childWidth - editing.noteWidth)).toBeLessThanOrEqual(1);

    // --- 2. 既定幅: 従来どおり左寄せ ---
    // 中央に寄せるのは 500px 以下だけ。広い幅ではアバターを避ける
    // `margin-left: 154px` の起点にボタンが残る。
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.locator('.moderationNote button')).toBeVisible({ timeout: 20_000 });

    const wide = await measure(page);
    expect(wide.containerWidth).toBeGreaterThan(500);
    expect(Math.abs(wide.buttonLeft - wide.noteLeft)).toBeLessThanOrEqual(1);
  });
});
