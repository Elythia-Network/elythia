/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// MkFolder の中の要素を、フォルダの見出しの文字で取るためのヘルパ (#4025)。
//
// 同じ画面に textarea や「Save」が複数あるとき、「1 つ目の textarea」で取ると
// フォルダが 1 つ増えただけで別の項目を指す (docs/playwright.md の「spec を書く
// ときの注意」)。見出しの文字でフォルダを決め、その中だけを探す。
//
// 見出しは**完全一致**で探し、ちょうど 1 つでなければ落とす。「Prohibited words」と
// 「Prohibited words for usernames」のように前方が同じ見出しがあるため。
//
// 見出しの文字は、要素の文字全体か、要素がじかに持つ文字 (子要素の分を除く) の
// どちらかで比べる。SearchLabel は要素を作らない (`<slot>` だけ) ので、見出しの
// 後ろにバッジ (「Google Analytics」の「Beta」など) があると、文字全体は一致しない。
//
// MkFolder は `role="group"` の要素の中に、見出しの button と中身を持つ
// (`asPage` のフォルダは中身を別の場所に Teleport するので、ここでは扱えない)。

import { type Page } from '@playwright/test';
import { type ClickOptions, clickWhenReady } from './ui_click';

// 見出しが `l` の folder-header を、ちょうど 1 つのときだけ返す (page 側で評価する)
function findHeader(l: string): HTMLElement | null {
  const headers = Array.from(document.querySelectorAll<HTMLElement>('[data-testid="folder-header"]'))
    .filter((h) => Array.from(h.querySelectorAll('*')).some((d) => (d.textContent ?? '').trim() === l || Array.from(d.childNodes).filter((c) => c.nodeType === Node.TEXT_NODE).map((c) => c.textContent ?? '').join('').trim() === l));
  return headers.length === 1 ? headers[0] : null;
}

/** Opens the MkFolder whose header text is exactly `label` (no-op when open). */
export async function openFolderByLabel(page: Page, label: string, options?: ClickOptions): Promise<void> {
  const timeout = options?.timeout ?? 20_000;
  const expanded = (l: string) => {
    const headers = Array.from(document.querySelectorAll('[data-testid="folder-header"]'))
      .filter((h) => Array.from(h.querySelectorAll('*')).some((d) => (d.textContent ?? '').trim() === l || Array.from(d.childNodes).filter((c) => c.nodeType === Node.TEXT_NODE).map((c) => c.textContent ?? '').join('').trim() === l));
    if (headers.length !== 1) return null;
    return headers[0].closest('[role="group"]')?.getAttribute('aria-expanded') === 'true' ? 'open' : 'closed';
  };
  // 見出しがちょうど 1 つ出るまで待ち、開いているかを読む
  const state = await page.waitForFunction(expanded, label, { timeout })
    .then((h) => h.jsonValue())
    .catch((cause: unknown) => {
      throw new Error(`見出しが「${label}」のフォルダがちょうど 1 つ見つからなかった`, { cause });
    });
  if (state === 'closed') {
    await clickWhenReady(page, `見出しが「${label}」のフォルダ`, findHeader, label, options);
  }
  await page.waitForFunction((l: string) => {
    const headers = Array.from(document.querySelectorAll('[data-testid="folder-header"]'))
      .filter((h) => Array.from(h.querySelectorAll('*')).some((d) => (d.textContent ?? '').trim() === l || Array.from(d.childNodes).filter((c) => c.nodeType === Node.TEXT_NODE).map((c) => c.textContent ?? '').join('').trim() === l));
    return headers.length === 1 && headers[0].closest('[role="group"]')?.getAttribute('aria-expanded') === 'true';
  }, label, { timeout });
}

/** Sets the value of the only textarea inside the folder whose header is `label`. */
export async function fillFolderTextarea(page: Page, label: string, value: string): Promise<void> {
  const handle = await page.waitForFunction(
    (l: string) => {
      const headers = Array.from(document.querySelectorAll('[data-testid="folder-header"]'))
        .filter((h) => Array.from(h.querySelectorAll('*')).some((d) => (d.textContent ?? '').trim() === l || Array.from(d.childNodes).filter((c) => c.nodeType === Node.TEXT_NODE).map((c) => c.textContent ?? '').join('').trim() === l));
      // 見出しはちょうど 1 つのときだけ使う (openFolderByLabel と同じ)
      const header = headers.length === 1 ? headers[0] : undefined;
      const textareas = header?.closest('[role="group"]')?.querySelectorAll('textarea');
      return textareas?.length === 1 ? textareas[0] : null;
    },
    label,
    { timeout: 20_000 },
  ).catch((cause: unknown) => {
    throw new Error(`見出しが「${label}」のフォルダの中に textarea がちょうど 1 つ見つからなかった`, { cause });
  });
  await handle.evaluate((node, v) => {
    const ta = node as HTMLTextAreaElement;
    ta.focus();
    // v-model は input event で値を読むので、setter を通して event を出す
    Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')?.set?.call(ta, v);
    ta.dispatchEvent(new Event('input', { bubbles: true }));
  }, value);
  await handle.dispose();
}

/** Clicks the button whose text is exactly `text` inside the folder whose header is `label`. */
export function clickFolderButton(page: Page, label: string, text: string, options?: ClickOptions): Promise<void> {
  return clickWhenReady(
    page,
    `見出しが「${label}」のフォルダの中のボタン「${text}」`,
    ({ l, t }: { l: string; t: string }) => {
      const headers = Array.from(document.querySelectorAll('[data-testid="folder-header"]'))
        .filter((h) => Array.from(h.querySelectorAll('*')).some((d) => (d.textContent ?? '').trim() === l || Array.from(d.childNodes).filter((c) => c.nodeType === Node.TEXT_NODE).map((c) => c.textContent ?? '').join('').trim() === l));
      // 見出しはちょうど 1 つのときだけ使う (openFolderByLabel と同じ)
      const header = headers.length === 1 ? headers[0] : undefined;
      const group = header?.closest('[role="group"]');
      if (!group) return null;
      const buttons = Array.from(group.querySelectorAll('button'))
        .filter((b) => b !== header && (b.textContent ?? '').trim() === t);
      return buttons.length === 1 ? buttons[0] : null;
    },
    { l: label, t: text },
    options,
  );
}
