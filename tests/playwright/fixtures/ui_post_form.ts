/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 投稿フォーム (MkPostForm) とノートの操作を確かめる spec で共有するヘルパ (#4043)。
//
// 結果は、UI が送った値 (`postDataJSON`) で確かめる。応答の status や画面の
// 表示だけで済ませると、「送ったつもり」の回帰 (値を詰め忘れる、別の値を送る)
// を見逃すため。
//
// 要素は data-testid で、**ちょうど 1 つ**のときだけ取る。ノートの一覧に同じ
// ボタンが並ぶ画面で、先頭のものを黙って押すと別のノートを操作してしまう。

import { expect, type Page, type Request } from '@playwright/test';
import { clickByTestId, clickWhenReady, type ClickOptions } from './ui_click';

/** Opens the post form from the navbar and waits for its textarea. */
export async function openPostForm(page: Page): Promise<void> {
  await clickByTestId(page, 'open-post-form');
  await waitForPostForm(page);
}

/** Waits until exactly one post form textarea is on the page. */
export async function waitForPostForm(page: Page): Promise<void> {
  await page.waitForFunction(
    () => document.querySelectorAll('[data-testid="post-form-text"]').length === 1,
    undefined,
    { timeout: 15_000 },
  );
}

/**
 * Replaces the post form text through the native value setter, so that Vue's
 * v-model sees an input event.
 */
export async function setPostText(page: Page, text: string): Promise<void> {
  await setFieldValue(page, 'post-form-text', text);
}

/** Replaces the value of the input or textarea carrying `data-testid="<testId>"`. */
export async function setFieldValue(page: Page, testId: string, value: string): Promise<void> {
  const handle = await page.waitForFunction(
    (id: string) => {
      const els = document.querySelectorAll(`[data-testid="${id}"]`);
      return els.length === 1 ? els[0] : null;
    },
    testId,
    { timeout: 15_000 },
  );
  await handle.evaluate((node, v) => {
    const el = node as HTMLInputElement | HTMLTextAreaElement;
    el.focus();
    const proto = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    Object.getOwnPropertyDescriptor(proto, 'value')?.set?.call(el, v);
    el.dispatchEvent(new Event('input', { bubbles: true }));
  }, value);
  await handle.dispose();
}

/** Reads the current value of the post form textarea. */
export async function postTextValue(page: Page): Promise<string> {
  return page.evaluate(() => {
    const els = document.querySelectorAll('[data-testid="post-form-text"]');
    if (els.length !== 1) throw new Error(`post-form-text が ${els.length} 個ある`);
    return (els[0] as HTMLTextAreaElement).value;
  });
}

/** Waits until the submit button of the post form is enabled. */
export async function waitForSubmitEnabled(page: Page): Promise<void> {
  await page.waitForFunction(
    () => {
      const btn = document.querySelector('[data-testid="post-form-submit"]') as HTMLButtonElement | null;
      return btn !== null && !btn.disabled;
    },
    undefined,
    { timeout: 10_000 },
  );
}

/**
 * Starts waiting for the browser to send `POST /api/<endpoint>` and returns
 * its JSON body. Call before the action that sends it.
 */
export function waitForApiBody(page: Page, endpoint: string, timeout = 15_000): Promise<Record<string, unknown>> {
  return page
    .waitForRequest((r: Request) => r.method() === 'POST' && new URL(r.url()).pathname === `/api/${endpoint}`, { timeout })
    .then((r) => r.postDataJSON() as Record<string, unknown>);
}

/**
 * Clicks the submit button and returns the body the form sent to
 * `notes/create`, after checking that the server accepted it.
 */
export async function submitPostForm(page: Page): Promise<Record<string, unknown>> {
  await waitForSubmitEnabled(page);
  const sent = waitForApiBody(page, 'notes/create');
  const accepted = page.waitForResponse(
    (r) => new URL(r.url()).pathname === '/api/notes/create',
    { timeout: 15_000 },
  );
  await clickByTestId(page, 'post-form-submit');
  const body = await sent;
  const resp = await accepted;
  expect(resp.status(), `notes/create failed: ${await resp.text()}`).toBe(200);
  return body;
}

/**
 * Clicks the element carrying `data-testid="<testId>"`, failing unless exactly
 * one is on the page.
 */
export function clickOnlyTestId(page: Page, testId: string, options?: ClickOptions): Promise<void> {
  return clickWhenReady(
    page,
    `data-testid="${testId}" (ちょうど 1 つ)`,
    (id: string) => {
      const els = document.querySelectorAll(`[data-testid="${id}"]`);
      return els.length === 1 ? els[0] : null;
    },
    testId,
    options,
  );
}

/**
 * Dispatches `mousedown` on the only element carrying `data-testid="<testId>"`.
 * The renote and "..." buttons of a note open their menu on mousedown
 * (`@mousedown.prevent`), so a click does nothing there.
 */
export async function mousedownOnlyTestId(page: Page, testId: string): Promise<void> {
  const handle = await page
    .waitForFunction(
      (id: string) => {
        const els = document.querySelectorAll(`[data-testid="${id}"]`);
        return els.length === 1 ? els[0] : null;
      },
      testId,
      { timeout: 20_000 },
    )
    .catch((cause: unknown) => {
      throw new Error(`data-testid="${testId}" がちょうど 1 つ見つからなかった`, { cause });
    });
  await handle.evaluate((node) => {
    node.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0 }));
  });
  await handle.dispose();
}

/**
 * Clicks the menu item whose title is exactly `text` in the open popup menu.
 * Fails unless exactly one item in the open menus has that title.
 */
export function clickMenuItem(page: Page, text: string, options?: ClickOptions): Promise<void> {
  return clickWhenReady(
    page,
    `メニューの項目「${text}」(ちょうど 1 つ)`,
    (t: string) => {
      const items = Array.from(document.querySelectorAll('[role="menu"] [role^="menuitem"]')).filter((item) =>
        // 項目の題は葉の div に入る。caption や子メニューの印と混ざらないよう、
        // 子要素を持たない要素の文字だけを見る
        Array.from(item.querySelectorAll('div, span')).some(
          (el) => el.children.length === 0 && (el.textContent ?? '').trim() === t,
        ),
      );
      return items.length === 1 ? items[0] : null;
    },
    text,
    options,
  );
}

/** Waits until some open popup menu has an item titled exactly `text`. */
export async function waitForMenuItem(page: Page, text: string): Promise<void> {
  await page.waitForFunction(
    (t: string) =>
      Array.from(document.querySelectorAll('[role="menu"] [role^="menuitem"]')).some((item) =>
        Array.from(item.querySelectorAll('div, span')).some(
          (el) => el.children.length === 0 && (el.textContent ?? '').trim() === t,
        ),
      ),
    text,
    { timeout: 15_000 },
  );
}

/**
 * Opens the submenu of the parent menu item titled exactly `text`. Parent items
 * open on `mouseenter` (MkMenu's showChildren), not on click, unless the device
 * prefers clicks.
 */
export async function openSubmenu(page: Page, text: string): Promise<void> {
  const handle = await page
    .waitForFunction(
      (t: string) => {
        const items = Array.from(document.querySelectorAll('[role="menu"] [role^="menuitem"]')).filter((item) =>
          Array.from(item.querySelectorAll('div, span')).some(
            (el) => el.children.length === 0 && (el.textContent ?? '').trim() === t,
          ),
        );
        return items.length === 1 ? items[0] : null;
      },
      text,
      { timeout: 15_000 },
    )
    .catch((cause: unknown) => {
      throw new Error(`メニューの項目「${text}」がちょうど 1 つ見つからなかった`, { cause });
    });
  await handle.evaluate((node) => node.dispatchEvent(new MouseEvent('mouseenter', { bubbles: false })));
  await handle.dispose();
}
