/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 設定の表 (#4056) の helper。1 行が 1 つの設定で、次の 4 段を確かめる。
//   1. 設定の画面で部品を操作して、値を変える
//   2. 保存: 端末の設定は localStorage の `preferences`、アカウントの設定は `i` で読み直す
//   3. 再読み込みの後も、部品がその値を出している
//   4. 効き目 (確かめられない設定は、表に理由を書いて省く)
//
// **行ごとに新しい利用者と、空の端末の設定から始める。** 端末の設定は localStorage に
// あり、Playwright はテストごとに新しい browser context を作るので、前の行の値は残らない。
// サインインはフォームを通さず、globalSetup が保存した root の状態の `account` を、この
// 利用者のものに差し替えて読み込む (ui_auth.ts の uiSigninAsRoot と同じ方法)。
//
// **効き目は再読み込みの後に見る。** 端末の設定の多くは `prefer.s` (リアクティブでない)
// で読まれ、次に描画したときに値を拾う。

import { readFileSync } from 'node:fs';
import { type APIRequestContext, type BrowserContext, expect, type Locator, type Page, test } from '@playwright/test';
import { type Principal, randomUsername, signupUser } from './auth';
import { isTsBackend } from './backend';
import { pageBody, waitForMountedPage } from './route_check';
import { ROOT_STATE_PATH } from './ui_auth';
import { clickMenuItem } from './ui_post_form';
import { gotoRoute } from './ui_route';
import { apiOk } from './ui_timeline';

/** The annotation type that declares the preference keys a test covers (the gate collects it). */
export const PREF_ANNOTATION_TYPE = 'pref';

/** Declares the preference keys a test checks, for tests/playwright/settings-coverage.mjs. */
export function coversPrefs(...keys: [string, ...string[]]): { annotation: { type: string; description: string }[] } {
  return { annotation: keys.map((k) => ({ type: PREF_ANNOTATION_TYPE, description: k })) };
}

type StorageState = Exclude<Parameters<BrowserContext['setStorageState']>[0], string>;

/** A preferences profile holding `prefs` for every scope (device, and the account when known). */
function preferencesProfile(prefs: Record<string, unknown>, accountId: string, host: string): string {
  const preferences: Record<string, unknown[]> = {};
  for (const [key, value] of Object.entries(prefs)) {
    preferences[key] = [[{}, value, {}], [{ server: host, account: accountId }, value, {}]];
  }
  return JSON.stringify({ id: 'settings-table', version: '', type: 'main', modifiedAt: 0, name: '', preferences });
}

/**
 * Starts `page` signed in as `user` with an empty device preferences profile,
 * or one holding `prefs`. The account setup wizard is turned off.
 */
export async function signinWithState(page: Page, request: APIRequestContext, baseURL: string, user: Principal, prefs: Record<string, unknown> = {}): Promise<void> {
  await apiOk(request, 'i/registry/set', { i: user.token, scope: ['client', 'base'], key: 'accountSetupWizard', value: -1 });
  const me = await apiOk(request, 'i', { i: user.token });
  const saved = JSON.parse(readFileSync(ROOT_STATE_PATH, 'utf-8')) as StorageState;
  const host = new URL(baseURL).host;
  let replaced = 0;
  for (const origin of saved.origins ?? []) {
    // root の端末の設定と IndexedDB (状態の保存先) は持ち込まない。行ごとに空から始める
    origin.localStorage = (origin.localStorage ?? []).filter((item) => item.name !== 'preferences').map((item) => {
      if (item.name !== 'account') return item;
      replaced++;
      return { name: 'account', value: JSON.stringify({ ...me, token: user.token }) };
    });
    if (Object.keys(prefs).length > 0) origin.localStorage.push({ name: 'preferences', value: preferencesProfile(prefs, user.id, host) });
    // AGPL の第 13 条のソースコードの案内は、最初の起動で 1 回だけ出て、操作の上にかぶる
    if (!origin.localStorage.some((item) => item.name === 'modifiedVersionMustProminentlyOfferInAgplV3Section13Read')) {
      origin.localStorage.push({ name: 'modifiedVersionMustProminentlyOfferInAgplV3Section13Read', value: 'true' });
    }
    // 型には無いが、保存した状態には IndexedDB の中身が入っている (global-setup.ts の indexedDB: true)
    delete (origin as { indexedDB?: unknown }).indexedDB;
  }
  expect(replaced, `${ROOT_STATE_PATH} has no localStorage account`).toBe(1);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.context().setStorageState(saved);
  await page.goto(`${baseURL}/`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('open-post-form').first()).toBeVisible({ timeout: 15_000 });
  const current = await page.evaluate(() => (JSON.parse(window.localStorage.getItem('account') ?? '{}') as { id?: string }).id);
  expect(current, 'サインイン中の利用者が想定と違う').toBe(user.id);
}

/**
 * Reads the stored value of a device preference in the order the app does
 * (getMatchedRecordOf in preferences/manager.ts): the account scope, then the
 * server scope (emoji palettes), then the device scope.
 */
export async function readPref(page: Page, key: string): Promise<unknown> {
  return page.evaluate((k) => {
    const raw = window.localStorage.getItem('preferences');
    if (raw == null) return undefined;
    const records = (JSON.parse(raw) as { preferences: Record<string, [Record<string, string>, unknown][]> }).preferences[k];
    if (!records) return undefined;
    const account = records.find(([scope]) => scope.account != null);
    const server = records.find(([scope]) => scope.account == null && scope.server != null);
    const device = records.find(([scope]) => scope.account == null && scope.server == null);
    return (account ?? server ?? device)?.[1];
  }, key);
}

/**
 * Opens every folder (MkFolder) of the page body, nested ones included. A
 * folder renders its body only after it is opened once.
 */
export async function openAllFolders(page: Page): Promise<void> {
  for (let round = 0; round < 10; round++) {
    const clicked = await pageBody(page).evaluate((body) => {
      let n = 0;
      for (const header of Array.from(body.querySelectorAll<HTMLElement>('[data-testid="folder-header"]'))) {
        if (header.closest('[role="group"]')?.getAttribute('aria-expanded') === 'false') {
          header.click();
          n++;
        }
      }
      return n;
    });
    if (clicked === 0) return;
    // 開いた中に入れ子のフォルダがあれば、描画を待ってから次の回で開く
    await page.waitForTimeout(150);
  }
  throw new Error('folders kept appearing after 10 rounds');
}

/** The container (MkPreferenceContainer) of a preference key. `nth` picks one when the key appears twice. */
export function prefContainer(page: Page, key: string, nth = 0): Locator {
  return pageBody(page).locator(`[data-pref-key="${key}"]`).nth(nth);
}

export type Control =
  | { kind: 'switch'; on: boolean }
  | { kind: 'radio'; label: string }
  | { kind: 'select'; label: string }
  | { kind: 'range'; value: number; min: number; max: number }
  /** Buttons and dialogs (a file chooser, the emoji picker). `shows` asserts the page shows the value after a reload. */
  | { kind: 'custom'; set: (ctx: RowContext, data: unknown) => Promise<void>; shows?: (ctx: RowContext, data: unknown) => Promise<void> };

/** Operates the control inside `container` so that it shows the target of `control`. */
export async function setControl(page: Page, container: Locator, control: Exclude<Control, { kind: 'custom' }>): Promise<void> {
  await container.scrollIntoViewIfNeeded();
  switch (control.kind) {
    case 'switch': {
      const toggle = container.getByTestId('switch-toggle');
      await expect(toggle).toHaveCount(1);
      if ((await toggle.getAttribute('data-checked')) !== String(control.on)) await toggle.click();
      return;
    }
    case 'radio':
      await radioOption(container, control.label).click();
      return;
    case 'select':
      // MkSelect は mousedown でメニューを開く
      await container.locator('[tabindex="0"]').first().dispatchEvent('mousedown');
      await clickMenuItem(page, control.label);
      return;
    case 'range': {
      const track = container.locator('.timctyfi .container');
      const thumb = container.locator('.timctyfi .thumb');
      const box = await track.boundingBox();
      const t = await thumb.boundingBox();
      expect(box && t, 'range boxes').toBeTruthy();
      const ratio = (control.value - control.min) / (control.max - control.min);
      await page.mouse.move(t!.x + t!.width / 2, t!.y + t!.height / 2);
      await page.mouse.down();
      await page.mouse.move(box!.x + box!.width * ratio, t!.y + t!.height / 2, { steps: 5 });
      await page.mouse.up();
      return;
    }
  }
}

function radioOption(container: Locator, label: string): Locator {
  return container.locator('[role="checkbox"]').filter({ hasText: new RegExp(`^\\s*${label.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\s*$`) });
}

/** Asserts that the control inside `container` shows the target of `control`. */
export async function expectControl(container: Locator, control: Exclude<Control, { kind: 'custom' }>): Promise<void> {
  switch (control.kind) {
    case 'switch':
      await expect(container.getByTestId('switch-toggle')).toHaveAttribute('data-checked', String(control.on));
      return;
    case 'radio':
      await expect(radioOption(container, control.label)).toHaveAttribute('aria-checked', 'true');
      return;
    case 'select':
      await expect(container.locator('[tabindex="0"]').first()).toContainText(control.label);
      return;
    case 'range':
      // 値の文字は出さない部品もあるので、保存された値 (呼び出し元) で確かめる
      return;
  }
}

export interface RowContext {
  page: Page;
  request: APIRequestContext;
  baseURL: string;
  /** The user the row signs in as (made for the row). */
  user: Principal;
  /** What the row's `setup` returned. */
  data?: unknown;
}

export interface PrefRow {
  key: string;
  /** The settings page that has the control. */
  route: string;
  control: Control;
  /** The value the app stores, or a function of what `setup` returned when it is known only at run time. */
  value: unknown;
  /** Picks the container when the key appears twice on the page. */
  nth?: number;
  /**
   * Finds the control when it is not in a MkPreferenceContainer (no
   * `data-pref-key`), for example `(page) => labeledSwitch(page, 'Developer mode')`.
   */
  container?: (page: Page) => Promise<Locator>;
  /** Makes the data the row needs before the settings page opens (an uploaded file, for example). */
  setup?: (ctx: RowContext) => Promise<unknown>;
  /** Device preferences to set before the page loads (to enable the control, for example). */
  pre?: Record<string, unknown>;
  /** Asserts the effect after a reload, or the reason it is not checked. */
  effect: ((ctx: RowContext) => Promise<void>) | { skip: string };
  /**
   * An issue (`'#4057'`) for a known bug that makes the effect fail. The effect
   * must fail with an assertion whose message carries the number, until it is fixed.
   */
  knownBug?: string;
}

/** The title of a row's test. A value known only at run time shows as `<set at run time>`. */
export function prefTitle(row: PrefRow): string {
  return `${row.key} → ${typeof row.value === 'function' ? '<set at run time>' : JSON.stringify(row.value)}`;
}

/**
 * The mutation check (#4056). With `PW_SETTINGS_MUTATION=1`, a row skips only
 * the step that changes the setting and expects the effect check to fail. A
 * row whose effect check still passes checks nothing.
 */
export const MUTATION = process.env.PW_SETTINGS_MUTATION === '1';

/**
 * Skips the rows that the mutation check cannot judge: rows that do not check
 * the effect, and rows with a known bug (their effect fails because of the bug).
 */
export function skipForMutation(effect: unknown, knownBug: string | undefined): void {
  if (!MUTATION) return;
  if (typeof effect === 'object' && effect != null && 'skip' in effect) test.skip(true, (effect as { skip: string }).skip);
  test.skip(knownBug != null, `${knownBug} の不具合で効き目が落ちるので、変異の確かめにならない`);
}

/**
 * Checks the effect of a row. A row with a known bug must fail the effect
 * check with the assertion that shows the bug, whose message carries the issue
 * number (`'#4057: ...'`); any other failure (a broken prerequisite, a timeout)
 * fails the row. Once the effect passes, it says to remove `knownBug`.
 */
export async function checkEffect<C extends RowContext>(name: string, effect: ((ctx: C) => Promise<void>) | { skip: string }, knownBug: string | undefined, ctx: C): Promise<void> {
  if ('skip' in effect) {
    test.info().annotations.push({ type: 'effect-skipped', description: effect.skip });
    return;
  }
  if (knownBug == null) {
    await effect(ctx);
    return;
  }
  test.info().annotations.push({ type: 'known-bug', description: knownBug });
  let passed = false;
  try {
    await effect(ctx);
    passed = true;
  } catch (e) {
    // 不具合を示す主張 (メッセージに issue の番号を入れる) で落ちるのだけを、今の期待どおりとする。
    // 前提 (API の失敗、要素の待ち) で落ちたものまで受けると、壊れても緑になる
    if (!(e instanceof Error) || e.name === 'TimeoutError' || !e.message.includes(knownBug)) throw e;
  }
  expect(passed, `${name}: ${knownBug} が直ったように見える。行の knownBug を外す`).toBe(false);
}

/**
 * The settings table reads the controls by `data-pref-key` and `data-checked`,
 * which Elythia added to the frontend (#4056). The TS backend run serves the
 * upstream frontend without them, so the rows do not run there.
 */
export function skipOnTsBackend(): void {
  test.skip(isTsBackend, '設定の表は Elythia の frontend で足した data-pref-key / data-checked で部品を取る');
}

/**
 * Runs one row: changes the control, checks the stored value, reloads and
 * checks the control again, then checks the effect. Call it from a test in a
 * spec file (`test(prefTitle(row), coversPrefs(row.key), ({ page, request, baseURL }) => runPrefRow({ page, request, baseURL }, row))`; Playwright wants the fixtures destructured),
 * so that the test belongs to the spec file in the listing and the timings.
 */
export async function runPrefRow({ page, request, baseURL }: { page: Page; request: APIRequestContext; baseURL?: string }, row: PrefRow): Promise<void> {
  skipOnTsBackend();
  skipForMutation(row.effect, row.knownBug);
  const user = await signupUser(request, randomUsername('pwpref'));
  const ctx: RowContext = { page, request, baseURL: baseURL!, user };
  await signinWithState(page, request, baseURL!, user, row.pre);
  const data = row.setup ? await row.setup(ctx) : undefined;
  ctx.data = data;
  const value = typeof row.value === 'function' ? (row.value as (d: unknown) => unknown)(data) : row.value;
  const container = () => row.container ? row.container(page) : Promise.resolve(prefContainer(page, row.key, row.nth));
  await gotoRoute(page, baseURL!, row.route);
  await openAllFolders(page);
  // 変異の確かめでは、設定を変える操作だけを省き、ページの移動と再読み込みは同じ順で行う
  if (!MUTATION) {
    if (row.control.kind === 'custom') await row.control.set(ctx, data);
    else await setControl(page, await container(), row.control);
    await expect.poll(() => readPref(page, row.key), { message: `${row.key} が保存されない` }).toEqual(value);
  }

  await page.reload({ waitUntil: 'domcontentloaded' });
  await waitForMountedPage(page);
  await openAllFolders(page);
  if (MUTATION) {
    await expectEffectFails(row.key, row.effect, ctx);
    return;
  }
  expect(await readPref(page, row.key), `${row.key} が再読み込みで戻った`).toEqual(value);
  if (row.control.kind === 'custom') await row.control.shows?.(ctx, data);
  else await expectControl(await container(), row.control);
  await checkEffect(row.key, row.effect, row.knownBug, ctx);
}

/**
 * The mutation check of one effect: it must fail with an assertion. A timeout
 * of an action (a click, a wait for an element) does not count, because it
 * fails for any reason (a slow page, a renamed button) rather than for the
 * difference the effect checks.
 */
export async function expectEffectFails<C extends RowContext>(name: string, effect: ((ctx: C) => Promise<void>) | { skip: string }, ctx: C): Promise<void> {
  if ('skip' in effect) return;
  // 効き目の確かめの待ちが長いと、全行で時間切れを待つことになるので短くする
  ctx.page.setDefaultTimeout(5_000);
  let error: unknown = null;
  let passed = false;
  try {
    await effect(ctx);
    passed = true;
  } catch (e) {
    error = e;
  }
  expect(passed, `${name}: 設定を変えなくても効き目の確かめが通る (何も確かめていない)`).toBe(false);
  expect((error as Error | null)?.name, `${name}: 効き目の確かめが、主張でなく操作の時間切れで落ちた (${(error as Error | null)?.message?.split('\n')[0]})`).not.toBe('TimeoutError');
}

/**
 * The switch (MkSwitch) whose label is exactly `label`, inside the page body.
 * Account settings have no preference key on the container, so the switch is
 * found by its label (the text of the label slot, without the caption).
 */
export async function labeledSwitch(page: Page, label: string): Promise<Locator> {
  const marked = await pageBody(page).evaluate((body, text) => {
    for (const el of Array.from(body.querySelectorAll('[data-vr-switch]'))) el.removeAttribute('data-vr-switch');
    const hits = Array.from(body.querySelectorAll<HTMLElement>('[data-testid="switch-toggle"]')).filter((toggle) => {
      // MkSwitch: <div root><input><span toggle/><span body><span label>…</span><p caption/></span></div>
      const root = toggle.parentElement;
      const labelEl = root?.querySelector(':scope > span:last-child > span:first-child') ?? root?.querySelector(':scope > span:last-child');
      return (labelEl?.textContent ?? '').trim() === text;
    });
    if (hits.length !== 1) return hits.length;
    hits[0].setAttribute('data-vr-switch', '');
    return 1;
  }, label);
  expect(marked, `ラベルが「${label}」のスイッチ (ちょうど 1 つ)`).toBe(1);
  return pageBody(page).locator('[data-vr-switch]');
}

export interface AccountRow {
  /** A short name of the row (the `i/update` field and the value). */
  name: string;
  /** The settings page that has the control. */
  route: string;
  /** Makes the data the row needs before the settings page opens; the result is `ctx.data`. */
  setup?: (ctx: RowContext) => Promise<unknown>;
  /** Operates the page to change the setting (folders are already open). */
  set: (ctx: RowContext) => Promise<void>;
  /** Reads the value from the response of `i`. */
  read: (me: Record<string, any>) => unknown;
  /** The value `read` returns once the setting is saved, or a function of `ctx.data`. */
  expected: unknown;
  /** Asserts the page shows the saved value after a reload (folders are already open). */
  shows: (ctx: RowContext) => Promise<void>;
  /** Asserts the effect, or the reason it is not checked. */
  effect: ((ctx: RowContext) => Promise<void>) | { skip: string };
  /**
   * An issue (`'#4057'`) for a known bug that makes the effect fail. The effect
   * must fail with an assertion whose message carries the number, until it is fixed.
   */
  knownBug?: string;
}

/**
 * Runs one account-setting row: changes the setting on the page, reads it back
 * with `i`, reloads and checks the page shows it, then checks the effect. A
 * fresh user per row, so nothing has to be put back.
 */
export async function runAccountRow({ page, request, baseURL }: { page: Page; request: APIRequestContext; baseURL?: string }, row: AccountRow): Promise<void> {
  skipOnTsBackend();
  skipForMutation(row.effect, row.knownBug);
  const user = await signupUser(request, randomUsername('pwacct'));
  const ctx: RowContext = { page, request, baseURL: baseURL!, user };
  await signinWithState(page, request, baseURL!, user);
  ctx.data = row.setup ? await row.setup(ctx) : undefined;
  const expected = typeof row.expected === 'function' ? (row.expected as (d: unknown) => unknown)(ctx.data) : row.expected;
  await gotoRoute(page, baseURL!, row.route);
  await openAllFolders(page);
  if (!MUTATION) {
    await row.set(ctx);
    await expect.poll(async () => row.read(await apiOk(request, 'i', { i: user.token })), { message: `${row.name} が保存されない` }).toEqual(expected);
  }
  await page.reload({ waitUntil: 'domcontentloaded' });
  await waitForMountedPage(page);
  await openAllFolders(page);
  if (MUTATION) {
    await expectEffectFails(row.name, row.effect, ctx);
    return;
  }
  await row.shows(ctx);
  await checkEffect(row.name, row.effect, row.knownBug, ctx);
}

/**
 * Waits for something the effect expects to happen (a request, a new page) and
 * fails with an assertion, not a timeout, when it does not. The mutation check
 * counts only assertion failures.
 */
export async function expectHappens<T>(waiting: Promise<T>, message: string): Promise<T> {
  const result = await waiting.catch(() => null);
  expect(result, message).not.toBeNull();
  return result as T;
}
