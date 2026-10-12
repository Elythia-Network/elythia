/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// UI spec で「要素の出現を待って programmatic click する」ための共有ヘルパ。
//
// #2600 の flaky は待機そのものが無かったのではなく、**待機の述語と click の
// 述語がずれていた**ことが原因だった。待っていたのは username が body に出るか、
// 押したかったのは textContent が 'Follow' の button。さらに `btn?.click()` は
// 未検出を握り潰すので、失敗は 60 秒後に「確認ダイアログが出ない」という
// 原因から遠い症状として現れた。
//
// ここでは finder を **1 度だけ**書き、それを待機にも click にも使う。
// `page.waitForFunction` は truthy を返すまで polling し、その値の handle を
// 返すので、Element を返す finder を渡せば「待機を満たした要素そのもの」が
// 手に入る。述語が同じどころか要素インスタンスが同一なので、ずれようがない。
//
// 使い方:
//   import { clickButtonByText, clickWhenReady } from '../../fixtures/ui_click';
//   await clickButtonByText(page, 'Follow');
//   await clickWhenReady(page, 'Delete ボタン', () =>
//     Array.from(document.querySelectorAll('button')).find(
//       (b) => !b.disabled && b.querySelector('i.ti-trash') !== null),
//   );
//
// **finder は page 側で評価される。** 外側のスコープの変数を掴むと browser 側で
// undefined になるので、値を渡したいときは `arg` 経由にすること (sugar の実装が
// その形の例)。
//
// **Playwright の locator には寄せられない。** `MkFollowButton` は DOM にあっても
// Playwright の visible 判定に入らず、`waitFor({state:'visible'})` が 20 秒待っても
// 解決しないことを #2600 で実測している。programmatic click が必要である以上、
// 待機側もそれに合わせる必要がある。

import { type Page } from '@playwright/test';

// 既存 spec の待機は 10s / 20s / 30s が混在していた。多数派の 20s を既定にし、
// 重い画面 (admin の一覧など) は呼び出し側で `{ timeout }` を渡す。
const DEFAULT_TIMEOUT = 20_000;

// 見つけた直後に Vue が再 render して要素が差し替わることがある。handle 方式は
// 再クエリしないので、そのまま click すると detached node を叩いて**新しい形の
// silent no-op** になる。検出して待機からやり直す。
const MAX_ATTEMPTS = 3;

export interface ClickOptions {
  timeout?: number;
}

// Finds the element to click. Evaluated in the page context, so it must not
// capture anything from the enclosing scope; pass values through `arg`.
export type ElementFinder<Arg> = (arg: Arg) => Element | null | undefined;

/**
 * Waits for `finder` to return an element, then clicks that very element.
 *
 * The same predicate drives both the wait and the click, so they cannot drift
 * apart. Every failure mode is loud: not found within the timeout, found but
 * detached from the DOM before the click could land, or found but disabled
 * (where `click()` fires no event at all).
 *
 * @param what Human-readable name of the target, used in failure messages.
 */
export async function clickWhenReady<Arg = undefined>(
  page: Page,
  what: string,
  finder: ElementFinder<Arg>,
  arg?: Arg,
  options: ClickOptions = {},
): Promise<void> {
  const timeout = options.timeout ?? DEFAULT_TIMEOUT;
  const deadline = Date.now() + timeout;

  for (let attempt = 1; attempt <= MAX_ATTEMPTS; attempt++) {
    const remaining = deadline - Date.now();
    if (remaining <= 0) {
      throw new Error(`${what} が ${timeout}ms 以内に見つからなかった`);
    }

    // waitForFunction は truthy を返した時点で解決するので、handle が得られた
    // 時点で中身は必ず Element。`Element | null | undefined` のままだと handle の
    // 型が union に割れるだけなので、ここで絞る。
    const handle = await page
      .waitForFunction(finder as (arg: Arg) => Element, arg as Arg, { timeout: remaining })
      .catch((cause: unknown) => {
        // waitForFunction の素の timeout は述語のソースしか出さない。
        // 何を待っていたのかを名前で言い直す。
        throw new Error(`${what} が ${timeout}ms 以内に見つからなかった`, { cause });
      });

    try {
      const outcome = await handle.evaluate((node) => {
        const el = node as HTMLElement;
        // 差し替わった要素を叩いても何も起きない。detach は再試行で拾う。
        if (!el.isConnected) return 'detached';
        // disabled な button / input は click() を呼んでも event が飛ばない。
        // 待てば有効になるとは限らないので、ここは待たずに落とす。
        if (el.matches(':disabled')) return 'disabled';
        el.click();
        return 'clicked';
      });
      if (outcome === 'clicked') return;
      if (outcome === 'disabled') {
        throw new Error(`${what} は disabled なので click しても何も起きない`);
      }
    } finally {
      // dispose が失敗しても本来のエラーを覆い隠さない (page が閉じた後など)。
      await handle.dispose().catch(() => {});
    }
  }

  throw new Error(`${what} は ${MAX_ATTEMPTS} 回とも click 直前に DOM から外れた`);
}

/** Clicks the first `<button>` whose trimmed text is exactly `text`. */
export function clickButtonByText(page: Page, text: string, options?: ClickOptions): Promise<void> {
  return clickWhenReady(
    page,
    `ボタン「${text}」`,
    (t: string) =>
      Array.from(document.querySelectorAll('button')).find((b) => (b.textContent ?? '').trim() === t),
    text,
    options,
  );
}

/** Clicks the first `<button>` whose text contains `text`. */
export function clickButtonContainingText(
  page: Page,
  text: string,
  options?: ClickOptions,
): Promise<void> {
  return clickWhenReady(
    page,
    `「${text}」を含むボタン`,
    (t: string) =>
      Array.from(document.querySelectorAll('button')).find((b) => (b.textContent ?? '').includes(t)),
    text,
    options,
  );
}

/** Clicks the first element carrying `data-testid="<testId>"`. */
export function clickByTestId(page: Page, testId: string, options?: ClickOptions): Promise<void> {
  return clickWhenReady(
    page,
    `data-testid="${testId}"`,
    (id: string) => document.querySelector(`[data-testid="${id}"]`),
    testId,
    options,
  );
}

/**
 * Clicks the first `<button>` containing an element matching `iconSelector`
 * (e.g. `i.ti-trash`). Misskey renders icon-only actions this way, with no text
 * and no test id to key off.
 */
export function clickButtonWithIcon(
  page: Page,
  iconSelector: string,
  options?: ClickOptions,
): Promise<void> {
  return clickWhenReady(
    page,
    `${iconSelector} を持つボタン`,
    (sel: string) =>
      Array.from(document.querySelectorAll('button')).find((b) => b.querySelector(sel) !== null),
    iconSelector,
    options,
  );
}

/**
 * Clicks the `MkSwitch` whose label contains `label`.
 *
 * MkSwitch renders its `<input type="checkbox">` and its label inside the same
 * root element, so the input's parent carries the label text.
 */
// **index で switch を引かない。** 画面に switch が 1 つ足されるだけでずれる。
// `admin_moderation_email_required_signup_toggle` は独自の switch (#2570) が
// 2 番目に入ったせいで別の設定を切り替えており、それでも spec は緑だった
// (#2620)。ラベルで引けばこの壊れ方はしない。
//
// ラベルが同じ switch が同居する画面 (admin/security の "Enable" など) では
// 引けないので、そこは folder などに scope した finder を clickWhenReady へ
// 直接渡すこと。
export function clickSwitchByLabel(
  page: Page,
  label: string,
  options?: ClickOptions,
): Promise<void> {
  return clickWhenReady(
    page,
    `「${label}」のスイッチ`,
    (l: string) =>
      Array.from(document.querySelectorAll('input[type="checkbox"]')).find((cb) =>
        (cb.parentElement?.textContent ?? '').includes(l),
      ),
    label,
    options,
  );
}
