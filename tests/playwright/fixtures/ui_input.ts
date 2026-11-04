/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// MkInput の input を、ラベルの文字で取るためのヘルパ (#4025)。
//
// MkInput はラベルを `<label>` で結び付けていないので、getByLabel では取れない。
// 根の要素 (global class の `_selectable`) の最初の子がラベル、中に input がある。
// ラベルは**完全一致**で探し、ちょうど 1 つでなければ落とす。

import { type Page } from '@playwright/test';

/** Sets the value of the MkInput whose label is exactly `label`. */
export async function fillInputByLabel(page: Page, label: string, value: string): Promise<void> {
  const handle = await page.waitForFunction(
    (l: string) => {
      const inputs = Array.from(document.querySelectorAll('input')).filter((input) => {
        const root = input.closest('._selectable');
        return (root?.firstElementChild?.textContent ?? '').trim() === l;
      });
      return inputs.length === 1 ? inputs[0] : null;
    },
    label,
    { timeout: 20_000 },
  ).catch((cause: unknown) => {
    throw new Error(`ラベルが「${label}」の input がちょうど 1 つ見つからなかった`, { cause });
  });
  await handle.evaluate((node, v) => {
    const input = node as HTMLInputElement;
    input.focus();
    // v-model は input event で値を読むので、setter を通して event を出す
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set?.call(input, v);
    input.dispatchEvent(new Event('input', { bubbles: true }));
  }, value);
  await handle.dispose();
}
