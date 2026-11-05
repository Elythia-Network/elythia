/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 投稿フォームの本文で `@`・`#`・`:` を打つと候補が出て、選ぶと本文に入り、
// そのまま送られることを確かめる (#4043)。
//
// 補完は textarea の input と caret の位置から動く (utility/autocomplete.ts)。
// native setter で値を入れると caret が動かないので、キーボードで打つ。
// 候補は途中まで打った文字で出し、選んだ後の本文が完全な形になっていることで、
// 候補を選んだ結果であることを確かめる。

import { expect, test, type Page } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickWhenReady } from '../../../fixtures/ui_click';
import { openPostForm, postTextValue, submitPostForm } from '../../../fixtures/ui_post_form';

async function typeIntoPostForm(page: Page, text: string): Promise<void> {
  await page.evaluate(() => {
    const ta = document.querySelector('[data-testid="post-form-text"]') as HTMLTextAreaElement | null;
    ta?.focus();
  });
  await page.keyboard.type(text, { delay: 30 });
}

async function chooseCandidate(page: Page, label: string): Promise<void> {
  await clickWhenReady(
    page,
    `補完の候補「${label}」`,
    (l: string) => {
      const roots = document.querySelectorAll('[data-testid="autocomplete"]');
      if (roots.length !== 1) return null;
      return Array.from(roots[0].querySelectorAll('li')).find((li) => (li.textContent ?? '').includes(l));
    },
    label,
  );
}

test.describe('UI: post form autocomplete', () => {
  test.setTimeout(90_000);

  test('@ suggests a user and completes the mention', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwac'));
    const target = await signupUser(request, randomUsername('pwacto'));
    await uiSigninAs(page, request, baseURL!, user);
    await openPostForm(page);

    // 名前の後ろ (乱数の部分) を残して打ち、候補から選ばせる
    await typeIntoPostForm(page, `@${target.username.slice(0, -4)}`);
    await chooseCandidate(page, `@${target.username}`);
    await expect.poll(() => postTextValue(page)).toBe(`@${target.username} `);

    const sent = await submitPostForm(page);
    expect(sent.text).toBe(`@${target.username} `);
    const notes = await (await callApi(request, 'users/notes', { i: user.token, userId: user.id })).json();
    expect(notes[0].mentions).toEqual([target.id]);
  });

  test('# suggests a known hashtag and completes it', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwac'));
    const tag = `pwtag${Date.now()}`;
    // 候補に出すには、先にどこかのノートで使われている必要がある
    const seed = await callApi(request, 'notes/create', { i: user.token, text: `seed #${tag}` });
    expect(seed.status()).toBe(200);
    // ハッシュタグの記録はノートの作成とは別に (非同期に) 行われる。候補は打ったときに
    // 1 回しか取りに行かないので、記録されるのを待ってから打つ
    await expect.poll(async () => (await (await callApi(request, 'hashtags/search', { query: tag, limit: 10 })).json()) as string[]).toContain(tag);
    await uiSigninAs(page, request, baseURL!, user);
    await openPostForm(page);

    await typeIntoPostForm(page, `#${tag.slice(0, -4)}`);
    await chooseCandidate(page, tag);
    await expect.poll(() => postTextValue(page)).toBe(`#${tag} `);

    const sent = await submitPostForm(page);
    expect(sent.text).toBe(`#${tag} `);
  });

  test(': suggests an emoji and completes it', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwac'));
    await uiSigninAs(page, request, baseURL!, user);
    await openPostForm(page);

    await typeIntoPostForm(page, 'pw :custar');
    await chooseCandidate(page, 'custard');
    // Unicode の絵文字は文字そのものが入る
    await expect.poll(() => postTextValue(page)).toBe('pw \u{1F36E}');

    const sent = await submitPostForm(page);
    expect(sent.text).toBe('pw \u{1F36E}');
  });
});
