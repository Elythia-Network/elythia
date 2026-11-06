/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 見た目の差分 (#4054): ホームのタイムライン、ノートの詳細、投稿フォーム、
// 絵文字ピッカー、リアクション。種まきと固定のしかたは fixtures/visual.ts。

import { test } from '@playwright/test';
import { isTsBackend } from '../../../../fixtures/backend';
import { pageBody } from '../../../../fixtures/route_check';
import { clickDriveItem } from '../../../../fixtures/ui_drive';
import { clickOnlyTestId, openPostForm, setFieldValue, setPostText } from '../../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../../fixtures/ui_route';
import { regionAbove, SCHEMES, snap, startAsAlice } from '../../../../fixtures/visual';

test.describe('visual: timeline and notes', () => {
  test.skip(isTsBackend, '見た目の差分は Elythia の frontend だけを撮る');
  test.setTimeout(90_000);

  for (const scheme of SCHEMES) {
    test(`home timeline (${scheme})`, coversRoutes('/'), async ({ page, baseURL, request }) => {
      // ノートの種類を 1 枚に収めるため、縦に長く撮る
      await startAsAlice(page, request, baseURL!, scheme, { viewport: { width: 1280, height: 2400 } });
      await gotoRoute(page, baseURL!, '/');
      await pageBody(page).getByText('Quoting Alice.').waitFor();
      await snap(page, `home-timeline-${scheme}`);
    });

    test(`note detail with a conversation (${scheme})`, coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
      const w = await startAsAlice(page, request, baseURL!, scheme, { viewport: { width: 1280, height: 1200 } });
      await gotoRoute(page, baseURL!, `/notes/${w.conversationNoteId}`);
      const body = pageBody(page);
      await body.getByText('What are you reading these days?').waitFor();
      // 返信は「Show replies」を押すまで読み込まない
      await body.getByRole('button', { name: 'Show replies' }).click();
      await body.getByText('Sounds nice, tell me more.').waitFor();
      await snap(page, `note-detail-${scheme}`);
    });

    test(`post form with CW, poll, files and the visibility picker (${scheme})`, async ({ page, baseURL, request }) => {
      const w = await startAsAlice(page, request, baseURL!, scheme, { viewport: { width: 1280, height: 1000 } });
      // 背後には、種まきのノートが出ないページを置く (ダイアログの絵にノートの変化が混ざらない)
      await gotoRoute(page, baseURL!, '/settings');
      await openPostForm(page);
      await clickOnlyTestId(page, 'post-form-cw-toggle');
      await setFieldValue(page, 'post-form-cw', 'About the weekend');
      await setPostText(page, 'Going to the planetarium on Saturday.');
      await clickOnlyTestId(page, 'post-form-poll');
      await page.getByPlaceholder('Choice 1').fill('Morning');
      await page.getByPlaceholder('Choice 2').fill('Evening');
      await clickOnlyTestId(page, 'post-form-attach-drive');
      await clickDriveItem(page, 'drive-file', `${w.driveFiles[0]}.png`);
      await clickDriveItem(page, 'drive-file', `${w.driveFiles[1]}.png`);
      await clickOnlyTestId(page, 'modal-window-ok');
      await page.getByTestId('modal-window-ok').waitFor({ state: 'detached' });
      await snap(page, `post-form-${scheme}`);
      await clickOnlyTestId(page, 'post-form-visibility');
      await page.getByTestId('visibility-home').waitFor();
      await snap(page, `post-form-visibility-${scheme}`);
    });

    test(`emoji picker with categories and a search (${scheme})`, async ({ page, baseURL, request }) => {
      await startAsAlice(page, request, baseURL!, scheme, { viewport: { width: 1280, height: 1000 } });
      await gotoRoute(page, baseURL!, '/settings');
      await openPostForm(page);
      await clickOnlyTestId(page, 'post-form-emoji');
      const search = page.getByTestId('emoji-picker-search');
      await search.waitFor();
      // カスタム絵文字の欄には、他の spec が作った絵文字の数が出る。その見出しより上を撮る
      const picker = search.locator('xpath=..');
      const custom = page.getByText('Custom Emoji', { exact: true });
      await snap(page, `emoji-picker-${scheme}`, { clip: await regionAbove(picker, custom) });
      await search.fill('star');
      await page.getByTestId('emoji-picker-search-result').first().waitFor();
      await snap(page, `emoji-picker-search-${scheme}`, { clip: await regionAbove(picker, custom) });
    });

    test(`reactions list and the reaction tooltip (${scheme})`, coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
      const w = await startAsAlice(page, request, baseURL!, scheme);
      await gotoRoute(page, baseURL!, `/notes/${w.aliceNoteId}/reactions`);
      const body = pageBody(page);
      // タブの中で絵文字を選ぶと、それを押した人が出る
      await body.getByTestId('note-reaction-tab').first().click();
      await body.getByText(/(Bob|Carol) Example/).first().waitFor();
      await snap(page, `reactions-list-${scheme}`);
      // リアクションのボタンにポインタを載せると、押した人の一覧が出る
      await body.getByTestId('note-reaction').first().hover();
      // ツールチップ (MkTooltip) の外枠は、CSS modules でない _acrylic と _shadow を持つ
      await page.locator('._acrylic._shadow').filter({ hasText: /(Bob|Carol) Example/ }).first().waitFor();
      await snap(page, `reaction-tooltip-${scheme}`, { keepPointer: true });
    });
  }
});
