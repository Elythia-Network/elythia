/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// ノートの投票で選択肢を選んで投票すると、notes/polls/vote に選んだ番号が送られ、
// 結果が出て、再読み込みの後も投票済みであることを確かめる (#4043)。
//
// 再読み込みの後の表示は、ノートの詳細が notes/show で取り直した poll の isVoted
// から決まる (MkPoll)。Elythia の notes/show は今これを埋めない (notes の handler の
// fieldResolver が投票の lookup を付けていない) ので、その部分だけを別のテストに
// 分け、Elythia では既知のバグの形 (#4044) だけを確かめて抜ける。直したらその分岐を消す。

import { expect, test, type APIRequestContext } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { type Principal, randomUsername, signupUser } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { showNoteRaw } from '../../../fixtures/notes';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickByTestId, clickWhenReady } from '../../../fixtures/ui_click';
import { waitForApiBody } from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

async function createPollNote(request: APIRequestContext, author: Principal, choices: string[]): Promise<string> {
  const resp = await callApi(request, 'notes/create', {
    i: author.token,
    text: `pw-vote-${Date.now()}`,
    poll: { choices, multiple: false },
  });
  expect(resp.status()).toBe(200);
  return (await resp.json()).createdNote.id;
}

test.describe('UI: note poll vote', () => {
  test.setTimeout(90_000);

  test('voting sends the choice and shows the result', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
    const author = await signupUser(request, randomUsername('pwvoteto'));
    const user = await signupUser(request, randomUsername('pwvote'));
    const choices = [`pw-vote-a-${Date.now()}`, `pw-vote-b-${Date.now()}`];
    const noteId = await createPollNote(request, author, choices);
    await uiSigninAs(page, request, baseURL!, user);

    await gotoRoute(page, baseURL!, `/notes/${noteId}`);
    const choiceB = page.getByTestId('poll-choice').filter({ hasText: choices[1] });
    await expect(page.getByTestId('poll-choice')).toHaveCount(2, { timeout: 20_000 });
    // 投票する前は票数を出さない
    await expect(choiceB).not.toContainText('votes');

    const sent = waitForApiBody(page, 'notes/polls/vote');
    await clickWhenReady(
      page,
      `選択肢「${choices[1]}」`,
      (t: string) => Array.from(document.querySelectorAll('[data-testid="poll-choice"]')).find((li) => (li.textContent ?? '').includes(t)),
      choices[1],
    );
    await clickByTestId(page, 'modal-dialog-ok');
    expect(await sent).toMatchObject({ noteId, choice: 1 });
    await expect(choiceB).toContainText('1 votes');

    // 票はサーバーに残っている
    await expect
      .poll(async () => (await (await showNoteRaw(request, user.token, noteId)).json()).poll.choices.map((c: { votes: number }) => c.votes))
      .toEqual([0, 1]);
  });

  test('the vote is shown as voted after a reload', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
    const author = await signupUser(request, randomUsername('pwvoteto'));
    const user = await signupUser(request, randomUsername('pwvote'));
    const choices = [`pw-vote-a-${Date.now()}`, `pw-vote-b-${Date.now()}`];
    const noteId = await createPollNote(request, author, choices);
    const vote = await callApi(request, 'notes/polls/vote', { i: user.token, noteId, choice: 1 });
    expect(vote.status()).toBe(204);

    const shown = await (await showNoteRaw(request, user.token, noteId)).json();
    const isVoted = shown.poll.choices.map((c: { isVoted: boolean }) => c.isVoted);
    if (!isTsBackend) {
      // Elythia の notes/show は poll の isVoted を埋めない (#4044)。test.fail にすると
      // どこで落ちても合格になり、前の段 (投票の 204 など) の回帰を見逃すので、既知の
      // 形だけを確かめて抜ける。直ったらここで落ちるので、この分岐を消す
      expect(isVoted, 'the known bug of #4044; remove this branch when it is fixed').toEqual([false, false]);
      test.info().annotations.push({ type: 'known-bug', description: '#4044' });
      return;
    }
    expect(isVoted).toEqual([false, true]);

    await uiSigninAs(page, request, baseURL!, user);
    await gotoRoute(page, baseURL!, `/notes/${noteId}`);
    await expect(page.getByTestId('poll-choice')).toHaveCount(2, { timeout: 20_000 });
    await expect(page.getByTestId('poll-choice').filter({ hasText: choices[1] }).locator('i.ti-check')).toHaveCount(1);
    await expect(page.getByText('Voted', { exact: true })).toBeVisible();
  });
});
