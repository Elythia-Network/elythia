// Phase 14-3 (#394) swap mode 検出 + known-failing spec の skip helper。
//
// `CYPRESS_MODE=baseline|swap` を compose 経由で cypress-runner の env に
// 渡す。spec 内で `inSwapMode()` を呼んで、mk 側の既知バグで swap モード
// では pass しない見込みの spec を `this.skip()` 扱いにできる。

// eslint-disable-next-line @typescript-eslint/triple-slash-reference
/// <reference types="cypress" />

export type TestMode = 'baseline' | 'swap';

// Cypress.env から mode を取得する。compose の CYPRESS_MODE が空なら
// baseline 扱い (ローカル手動実行の safe default)。
export function currentMode(): TestMode {
  const raw = Cypress.env('MODE');
  return raw === 'swap' ? 'swap' : 'baseline';
}

export function inSwapMode(): boolean {
  return currentMode() === 'swap';
}

// 指定 spec が swap モードで skip 対象なら this.skip() を呼ぶ helper。
// `this` context が必要なので it の第2引数にある function 内でのみ使う。
export function skipInSwap(
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  ctx: any,
  reason: string,
): void {
  if (inSwapMode()) {
    // eslint-disable-next-line no-console
    console.log(`[skipInSwap] ${reason}`);
    ctx.skip();
  }
}
