/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// キーボードのショートカットを確かめる (#4049)。
//
// - 全体: `p` で投稿フォームが開く、`s` で検索へ移る、`d` でライト/ダークが切り替わる
// - ノートの上 (フォーカスがあるとき): `r` で返信、`q` でリノート、`e` でリアクション
//
// `n` は post_form_keyboard.spec.ts が確かめている。issue に挙げた `h` は、本家にも
// Elythia にもショートカットが無い (boot/main-boot.ts の keymap) ので扱わない。
//
// ノートの操作の結果は、送った値 (notes/create の replyId / renoteId、
// notes/reactions/create) で見る。入力欄にフォーカスがあるとホットキーは効かないので、
// キーを押す前にフォーカスを外すか、ノートにフォーカスを当てる。

import { expect, type Page, test } from '@playwright/test';
import { callApi } from '../../../fixtures/api';
import { randomUsername, signupUser } from '../../../fixtures/auth';
import { createNote } from '../../../fixtures/notes';
import { uiSigninAs } from '../../../fixtures/ui_auth';
import { clickByTestId, clickWhenReady } from '../../../fixtures/ui_click';
import {
  clickMenuItem,
  setPostText,
  submitPostForm,
  waitForApiBody,
  waitForPostForm,
} from '../../../fixtures/ui_post_form';
import { coversRoutes, gotoRoute } from '../../../fixtures/ui_route';

const REACTION = '\u{1F44D}';

async function pressGlobal(page: Page, key: string): Promise<void> {
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await page.keyboard.press(key);
}

async function pressOnNote(page: Page, key: string): Promise<void> {
  const note = page.getByTestId('note-detailed');
  await expect(note).toHaveCount(1, { timeout: 15_000 });
  await note.focus();
  await page.keyboard.press(key);
}

test.describe('UI: keyboard shortcuts', () => {
  test.setTimeout(120_000);

  test('p opens the post form, s goes to search, d toggles dark mode', async ({ page, baseURL, request }) => {
    const user = await signupUser(request, randomUsername('pwhotkey'));
    await page.emulateMedia({ colorScheme: 'light' });
    await uiSigninAs(page, request, baseURL!, user);

    await pressGlobal(page, 'p');
    await waitForPostForm(page);
    await expect(page.getByTestId('post-form-text')).toBeFocused();
    await page.keyboard.press('Escape');
    await expect(page.getByTestId('post-form-text')).toHaveCount(0, { timeout: 10_000 });

    await pressGlobal(page, 's');
    await expect(page).toHaveURL(/\/search$/, { timeout: 10_000 });

    // d: OS の設定に追従している (既定) ので、追従を切るかを聞かれる
    expect(await page.evaluate(() => document.documentElement.dataset.colorScheme)).toBe('light');
    await pressGlobal(page, 'd');
    await clickByTestId(page, 'modal-dialog-ok');
    await expect.poll(() => page.evaluate(() => document.documentElement.dataset.colorScheme)).toBe('dark');
  });

  test('r replies, q renotes, and e reacts to the focused note', coversRoutes('/notes/:noteId/:initialTab?'), async ({ page, baseURL, request }) => {
    const author = await signupUser(request, randomUsername('pwhkto'));
    const user = await signupUser(request, randomUsername('pwhknote'));
    const target = await createNote(request, author.token, { text: `pw-hotkey-target-${Date.now()}` });
    await uiSigninAs(page, request, baseURL!, user);
    await gotoRoute(page, baseURL!, `/notes/${target.id}`);
    await expect(page.getByText(target.text!, { exact: true })).toBeVisible({ timeout: 20_000 });

    // r: 返信のフォームが開く
    await pressOnNote(page, 'r');
    await waitForPostForm(page);
    const replyText = `pw-hotkey-reply-${Date.now()}`;
    await setPostText(page, replyText);
    const reply = await submitPostForm(page);
    expect(reply).toMatchObject({ replyId: target.id, text: replyText });

    // q: リノートのメニューが開き、「Renote」でリノートする
    await expect(page.getByTestId('post-form-text')).toHaveCount(0, { timeout: 10_000 });
    const renote = waitForApiBody(page, 'notes/create');
    await pressOnNote(page, 'q');
    await clickMenuItem(page, 'Renote');
    expect(await renote).toMatchObject({ renoteId: target.id });

    // e: リアクションのピッカーが開き、選ぶとリアクションする
    const reacted = waitForApiBody(page, 'notes/reactions/create');
    await pressOnNote(page, 'e');
    await clickWhenReady(page, `ピッカーの ${REACTION}`, (e: string) => document.querySelector(`[data-emoji="${e}"]`), REACTION);
    expect(await reacted).toMatchObject({ noteId: target.id, reaction: REACTION });

    const notes = await (await callApi(request, 'users/notes', { i: user.token, userId: user.id, withReplies: true })).json() as { replyId: string | null; renoteId: string | null }[];
    expect(notes.filter((n) => n.replyId === target.id)).toHaveLength(1);
    expect(notes.filter((n) => n.renoteId === target.id)).toHaveLength(1);
  });
});
