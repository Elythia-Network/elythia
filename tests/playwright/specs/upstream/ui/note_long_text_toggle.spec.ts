/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 一覧 (MkNote) の長いノートが折りたたまれて出て、「Show more」で全体が開き、
// 「Close」で元の高さに戻ることを確かめる (#4043)。
//
// 開閉は本文の要素の高さで見る。ボタンの文言が入れ替わるだけでは、本文の高さが
// 変わらない回帰 (class が付かないなど) を見逃す。10 行以上の本文は折りたたむ
// (frontend-shared/js/collapsed.ts)。

import { expect, test, type Locator } from '@playwright/test';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { createNote } from '../../../fixtures/notes';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickOnlyTestId } from '../../../fixtures/ui_post_form';
import { gotoRoute } from '../../../fixtures/ui_route';

async function height(locator: Locator): Promise<number> {
  const box = await locator.boundingBox();
  if (box === null) throw new Error('本文の要素の大きさが取れなかった');
  return box.height;
}

test.describe('UI: note long text toggle', () => {
  test.setTimeout(90_000);

  test('a long note is collapsed, Show more expands it and Close collapses it', async ({ page, baseURL, request }) => {
    const author = await signupUser(request, randomUsername('pwlongto'));
    const user = await signupUser(request, randomUsername('pwlong'));
    const lines = Array.from({ length: 30 }, (_, i) => `pw-long-line-${i}`);
    await createNote(request, author.token, { text: lines.join('\n') });
    await uiSigninAs(page, request, baseURL!, user);

    await gotoRoute(page, baseURL!, `/@${author.username}/notes`);
    const content = page.getByTestId('note-content');
    await expect(content).toHaveCount(1, { timeout: 20_000 });
    await expect(page.getByTestId('note-show-more')).toHaveCount(1);
    const collapsed = await height(content);

    await clickOnlyTestId(page, 'note-show-more');
    await expect(page.getByTestId('note-show-less')).toHaveCount(1);
    await expect.poll(() => height(content)).toBeGreaterThan(collapsed * 1.5);

    await clickOnlyTestId(page, 'note-show-less');
    await expect(page.getByTestId('note-show-more')).toHaveCount(1);
    await expect.poll(() => height(content)).toBeCloseTo(collapsed, 0);
  });
});
