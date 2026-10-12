/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// /admin/system-webhook で "Create" button → MkSystemWebhookEditor dialog →
// title / url を埋めて Save → /api/admin/system-webhook/create が round-trip
// する write-flow spec。
//
// system-webhook.vue の onCreateWebhookClicked は
// showSystemWebhookEditorDialog({mode: 'create'}) を popup する (line 45-48)。
// dialog の中に MkInput が 3 個 (title / url / secret) と 5 個程度の switch
// (events.* trigger) が並び、最後に primary "Save" button (= onSubmitClicked)
// がある。本 spec は title + url 必須を埋めて Save を click する。

import { readFileSync } from 'node:fs';
import { expect, test } from '@playwright/test';
import { type RootFixture, uiSigninAsRoot } from '../../../fixtures/ui_auth';
import { clickButtonWithIcon, clickWhenReady } from '../../../fixtures/ui_click';

test.describe('UI: /admin/system-webhook create flow', () => {
  let root: RootFixture;
  test.beforeAll(() => {
    root = JSON.parse(readFileSync('.auth/root.json', 'utf-8'));
  });
  test.setTimeout(60_000);

  test('Create button → editor dialog → admin/system-webhook/create', async ({
    page,
    baseURL,
  }) => {
    await uiSigninAsRoot(page, baseURL, root);
    await page.goto(`${baseURL}/admin/system-webhook`, {
      waitUntil: 'domcontentloaded',
    });

    // "Create webhook" primary button (= ti-plus icon を持つ button) hydrate
    await clickButtonWithIcon(page, 'i.ti-plus');

    // dialog 内 text input が 3 個 (title / url / secret) hydrate するまで待つ
    await page.waitForFunction(
      () => {
        const inputs = Array.from(document.querySelectorAll('input')) as HTMLInputElement[];
        return inputs.filter((i) => i.type === 'text').length >= 3;
      },
      { timeout: 10_000 },
    );

    // title / url / secret を投入。MkSystemWebhookEditor.vue の
    // `disableSubmitButton` は title / url に加えて **secret も必須** に
    // しており (`if (!secret.value) return true`)、空のままだと OK button が
    // disabled のままで click が効かない。
    const title = `pw-webhook-${Date.now()}`;
    const url = `https://example.invalid/pw/${Date.now()}`;
    const secret = `pw-secret-${Date.now()}`;
    await page.evaluate(
      ({ t, u, s }) => {
        const inputs = (Array.from(document.querySelectorAll('input')) as HTMLInputElement[]).filter(
          (i) => i.type === 'text',
        );
        const setter = Object.getOwnPropertyDescriptor(
          window.HTMLInputElement.prototype,
          'value',
        )?.set;
        for (const [idx, value] of [t, u, s].entries()) {
          const target = inputs[idx];
          if (!target) continue;
          target.focus();
          setter?.call(target, value);
          target.dispatchEvent(new Event('input', { bubbles: true }));
        }
      },
      { t: title, u: url, s: secret },
    );

    // Submit button = MkSystemWebhookEditor.vue:84 の `<MkButton primary
    // rounded :disabled="disableSubmitButton" @click="onSubmitClicked">`
    // で text は **`i18n.ts.ok`** = `"OK"` (en-US.yml)。"Save" ではない。
    // ti-check icon + "OK" text (trim+exact match) + non-disabled で識別。
    // `.includes('OK')` だと "Lookup" / "Block OK" 等の他 button にも hit
    // する false positive risk があるので `.trim() === 'OK'` で絞る。
    const createResp = page.waitForResponse(
      (r) =>
        r.url().includes('/api/admin/system-webhook/create') && r.status() < 300,
      { timeout: 15_000 },
    );
    await clickWhenReady(page, '「OK」のボタン', () =>
      Array.from(document.querySelectorAll('button')).find(
        (b) =>
          !b.disabled &&
          b.querySelector('i.ti-check') !== null &&
          (b.textContent ?? '').trim() === 'OK',
      ),
    );
    const create = await createResp;
    expect(create.status()).toBeLessThan(300);
  });
});
