/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 投稿フォームの絵文字ピッカーを確かめる (#4049)。
//
// - 検索すると、名前に検索語を含む絵文字だけに絞られる
// - 選ぶと本文のカーソルの位置に入り、送った本文 (notes/create) にも入っている
// - 最近使ったものが先頭に (新しい順で) 出て、再読み込みの後も残る
//
// 投稿フォームのピッカーは選んでも閉じない (utility/emoji-picker.ts の
// choseAndClose: false) ので、続けて 2 つ選ぶ。最近使ったものは端末に保存する
// (store の recentlyUsedEmojis、where: 'device') ので、サーバーには無い。再読み込みの
// 後にピッカーを開き直して読む。
//
// 固定の絵文字 (パレット) から選ぶと最近使ったものに入らない (MkEmojiPicker の
// chosen) ので、既定のパレットに無いものを選ぶ。

import { expect, type Page, test } from '@playwright/test';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';
import {
  clickOnlyTestId,
  openPostForm,
  postTextValue,
  setPostText,
  submitPostForm,
} from '../../../fixtures/ui_post_form';

const PIZZA = '\u{1F355}';
const TACO = '\u{1F32E}';

// 検索欄に打つ。ピッカーは @input で値を読む
async function searchEmoji(page: Page, q: string): Promise<void> {
  const input = page.getByTestId('emoji-picker-search');
  await expect(input).toHaveCount(1, { timeout: 15_000 });
  await input.fill(q);
}

// 検索の結果の title (絵文字の名前) を、出た順に読む。q に合う結果が出るまで待つ
async function searchResultNames(page: Page, q: string): Promise<string[]> {
  await expect.poll(async () => (await page.getByTestId('emoji-picker-search-result').evaluateAll(
    (els) => els.map((e) => e.getAttribute('title') ?? ''),
  )).includes(q), { timeout: 10_000 }).toBe(true);
  return page.getByTestId('emoji-picker-search-result').evaluateAll((els) => els.map((e) => e.getAttribute('title') ?? ''));
}

// 検索の結果のうち、名前が name のものを押す
function clickSearchResult(page: Page, name: string): Promise<void> {
  return clickWhenReady(
    page,
    `検索の結果の絵文字「${name}」(ちょうど 1 つ)`,
    (n: string) => {
      const hits = Array.from(document.querySelectorAll('[data-testid="emoji-picker-search-result"]')).filter((b) => b.getAttribute('title') === n);
      return hits.length === 1 ? hits[0] : null;
    },
    name,
  );
}

// 最近使ったものの欄の絵文字を、出ている順に読む
function recentEmojis(page: Page): Promise<string[]> {
  return page.getByTestId('emoji-picker-recent').locator('[data-emoji]').evaluateAll(
    (els) => els.map((e) => e.getAttribute('data-emoji') ?? ''),
  );
}

test.describe('UI: emoji picker in the post form', () => {
  test.setTimeout(90_000);

  test('search narrows the list, picking inserts into the text, and recents survive a reload', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwemoji'));
    await uiSigninAs(page, request, baseURL!, user);
    await openPostForm(page);
    const prefix = `pw-emoji-${Date.now()} `;
    await setPostText(page, prefix);

    await clickOnlyTestId(page, 'post-form-emoji');
    await expect(page.getByTestId('emoji-picker-recent')).toHaveCount(1, { timeout: 15_000 });
    expect(await recentEmojis(page)).toEqual([]);

    // 検索で絞る: 出たものは全部、名前に検索語を含む
    await searchEmoji(page, 'pizza');
    const names = await searchResultNames(page, 'pizza');
    expect(names.length).toBeGreaterThan(0);
    for (const n of names) expect(n).toContain('pizza');
    await clickSearchResult(page, 'pizza');
    await expect.poll(() => postTextValue(page)).toBe(`${prefix}${PIZZA}`);

    // ピッカーは開いたままなので、続けて選ぶ。前に入れた絵文字の後ろに入る
    await searchEmoji(page, 'taco');
    const tacoNames = await searchResultNames(page, 'taco');
    for (const n of tacoNames) expect(n).toContain('taco');
    await clickSearchResult(page, 'taco');
    await expect.poll(() => postTextValue(page)).toBe(`${prefix}${PIZZA}${TACO}`);

    // 検索を空にすると最近使ったものの欄が読める。新しいものが先頭
    await searchEmoji(page, '');
    await expect.poll(() => recentEmojis(page)).toEqual([TACO, PIZZA]);

    await page.getByTestId('emoji-picker-search').press('Escape');
    await expect(page.getByTestId('emoji-picker-search')).toHaveCount(0, { timeout: 10_000 });
    const sent = await submitPostForm(page);
    expect(sent.text).toBe(`${prefix}${PIZZA}${TACO}`);

    // 再読み込みの後も、最近使ったものが同じ順で残る
    await page.reload({ waitUntil: 'domcontentloaded' });
    await openPostForm(page);
    await clickOnlyTestId(page, 'post-form-emoji');
    await expect(page.getByTestId('emoji-picker-recent')).toHaveCount(1, { timeout: 15_000 });
    await expect.poll(() => recentEmojis(page)).toEqual([TACO, PIZZA]);
  });
});
