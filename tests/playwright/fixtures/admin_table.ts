/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 管理画面の設定とロールのポリシーの表 (#4066) の helper。1 行の流れは設定の表 (#4056、
// settings_table.ts) と同じ 4 段で、変異の確かめ (`PW_SETTINGS_MUTATION=1`) と
// 既知の不具合の印 (`knownBug`) もそちらと同じ関数で扱う。
//
// - meta の行: root で管理画面を操作し、`admin/meta` で保存を確かめる。行の前に控えた値を、
//   終わったら `admin/update-meta` で戻す。instance meta は全 spec が共有するので、
//   この helper を使う spec は `meta` の project で流れる (`runMetaRow` を呼ぶ spec は、
//   shard-plan.mjs の writesMeta が META_WRITING_HELPERS で拾う)
// - ポリシーの行: 行ごとに手動のロールを API で作って新しい利用者に割り当て、root で
//   そのロールの編集画面から値を保存する。meta を書かないので `main` の project で流れる。
//   割り当てたロールの値は、優先度を「高」にしないと既定値より厳しくできない (真偽値は
//   どれかのロールが真なら真、上限は大きい方が採られる。internal/core/role の
//   computePolicy) ので、どの行も優先度を「高」にする

import { readFileSync } from 'node:fs';
import { type APIRequestContext, expect, type Locator, type Page } from '@playwright/test';
import { callApi } from './api';
import { randomUsername, signupUser } from './auth';
import { pageBody, waitForMountedPage } from './route_check';
import { checkEffect, expectEffectFails, MUTATION, openAllFolders, type RowContext, skipForMutation, skipOnTsBackend } from './settings_table';
import { type RootFixture, uiSigninAsRoot } from './ui_auth';
import { clickFolderButton } from './ui_folder';
import { clickMenuItem } from './ui_post_form';
import { gotoRoute } from './ui_route';
import { apiOk } from './ui_timeline';

/** The annotation type that declares the `admin/update-meta` keys a test covers (the gate collects it). */
export const META_ANNOTATION_TYPE = 'meta';
/** The annotation type that declares the role policy keys a test covers (the gate collects it). */
export const POLICY_ANNOTATION_TYPE = 'policy';

/** Declares the `admin/update-meta` keys a test checks, for tests/playwright/admin-settings-coverage.mjs. */
export function coversMeta(...keys: [string, ...string[]]): { annotation: { type: string; description: string }[] } {
  return { annotation: keys.map((k) => ({ type: META_ANNOTATION_TYPE, description: k })) };
}

/** Declares the role policy keys a test checks, for tests/playwright/admin-settings-coverage.mjs. */
export function coversPolicies(...keys: [string, ...string[]]): { annotation: { type: string; description: string }[] } {
  return { annotation: keys.map((k) => ({ type: POLICY_ANNOTATION_TYPE, description: k })) };
}

/**
 * Signs in as root for an admin page. The notice about the source code (AGPL
 * section 13) shows once on the first start and covers the page, so it is
 * marked as read before the page loads (as fixtures/visual.ts does).
 */
export async function signinAdmin(page: Page, baseURL: string, root: RootFixture): Promise<void> {
  await page.addInitScript(() => {
    window.localStorage.setItem('modifiedVersionMustProminentlyOfferInAgplV3Section13Read', 'true');
  });
  await uiSigninAsRoot(page, baseURL, root);
}

/** Reads the root account globalSetup made. */
export function rootFixture(): RootFixture {
  return JSON.parse(readFileSync('.auth/root.json', 'utf-8')) as RootFixture;
}

export interface AdminRowContext extends RowContext {
  root: RootFixture;
}

type Effect = ((ctx: AdminRowContext) => Promise<void>) | { skip: string };

/**
 * The switch (MkSwitch) inside `scope` whose label is exactly `label`. The
 * scope is a folder, so the same label (`Enable`) can appear in other folders.
 */
export async function switchIn(scope: Locator, label: string): Promise<Locator> {
  const toggles = scope.getByTestId('switch-toggle');
  const n = await toggles.count();
  const exact: number[] = [];
  const prefixed: number[] = [];
  for (let i = 0; i < n; i++) {
    const text = await toggles.nth(i).evaluate((toggle) => {
      // MkSwitch: <div root><input><span toggle/><span body><span label>…</span><p caption/></span></div>
      const root = toggle.parentElement;
      const labelEl = root?.querySelector(':scope > span:last-child > span:first-child') ?? root?.querySelector(':scope > span:last-child');
      // 値を変えると見出しに「Modified」の印が付く (管理画面のフォーム)
      const clone = labelEl?.cloneNode(true) as Element | undefined;
      for (const m of Array.from(clone?.querySelectorAll('._modified, ._beta') ?? [])) m.remove();
      return (clone?.textContent ?? '').replace(/\s+/g, ' ').trim();
    });
    if (text === label) exact.push(i);
    // 「(Recommended)」のような括弧の注記が後に付く見出しもある
    else if (text.startsWith(`${label} (`)) prefixed.push(i);
  }
  const hits = exact.length > 0 ? exact : prefixed;
  expect(hits.length, `ラベルが「${label}」のスイッチ (ちょうど 1 つ)`).toBe(1);
  return toggles.nth(hits[0]);
}

/** Turns the switch on or off. */
export async function setSwitch(toggle: Locator, on: boolean): Promise<void> {
  await toggle.scrollIntoViewIfNeeded();
  if ((await toggle.getAttribute('data-checked')) !== String(on)) await toggle.click();
  await expect(toggle).toHaveAttribute('data-checked', String(on));
}

/** Fills an input (MkInput) and fires the events v-model and the debounced fields listen to. */
export async function fillInput(input: Locator, value: string): Promise<void> {
  await input.scrollIntoViewIfNeeded();
  await input.fill(value);
  await input.dispatchEvent('change');
}

/** Picks `label` in the only MkSelect inside `scope`. */
export async function pickSelect(page: Page, scope: Locator, label: string): Promise<void> {
  // MkSelect は mousedown でメニューを開く
  const select = scope.locator('[tabindex="0"]').first();
  await select.scrollIntoViewIfNeeded();
  await select.dispatchEvent('mousedown');
  await clickMenuItem(page, label);
}

/** Moves the knob of a range (MkRange) to its right end. */
export async function rangeToMax(page: Page, range: Locator): Promise<void> {
  // 画面の端だと、下に固定された保存の帯がつまみに重なり、ドラッグがそちらに当たる
  await range.evaluate((el) => el.scrollIntoView({ block: 'center' }));
  const track = range.locator('.container');
  const thumb = range.locator('.thumb');
  const box = await track.boundingBox();
  const t = await thumb.boundingBox();
  expect(box && t, 'range boxes').toBeTruthy();
  await page.mouse.move(t!.x + t!.width / 2, t!.y + t!.height / 2);
  await page.mouse.down();
  await page.mouse.move(box!.x + box!.width + 20, t!.y + t!.height / 2, { steps: 5 });
  await page.mouse.up();
}

/**
 * The form field (MkInput / MkTextarea / MkSelect / MkRadios / MkRange) whose
 * label is `label`, inside the page body. The label slot gets a `Modified` mark
 * once the value changes, and some labels end with ` (Optional)`, so both are
 * ignored. Exactly one must match.
 */
export async function fieldByLabel(page: Page, label: string): Promise<Locator> {
  const marked = await pageBody(page).evaluate((body, text) => {
    for (const el of Array.from(body.querySelectorAll('[data-vr-field]'))) el.removeAttribute('data-vr-field');
    const labelText = (el: Element) => {
      const clone = el.cloneNode(true) as Element;
      for (const m of Array.from(clone.querySelectorAll('._modified, ._beta'))) m.remove();
      return (clone.textContent ?? '').replace(/\s+/g, ' ').trim();
    };
    const fields = Array.from(body.querySelectorAll<HTMLElement>('div')).filter((root) => {
      const first = root.firstElementChild;
      // 部品は「見出しの div」と「入力の div」を子に持つ。見出しの div だけを比べる
      if (first?.tagName !== 'DIV' || root.querySelector(':scope > div ~ div') == null) return false;
      return root.querySelector('input, textarea, [tabindex="0"], [role="checkbox"], .timctyfi') != null;
    });
    // 見出しが完全に一致するものを優先する (「Icon URL」と「Icon URL (App/192px)」)
    const exact = fields.filter((root) => labelText(root.firstElementChild!) === text);
    const hits = exact.length > 0 ? exact : fields.filter((root) => labelText(root.firstElementChild!).startsWith(`${text} (`));
    // 入れ子で同じ見出しを持つ外側の要素を除き、いちばん内側だけを残す
    const innermost = hits.filter((h) => !hits.some((o) => o !== h && h.contains(o)));
    if (innermost.length !== 1) return innermost.length;
    innermost[0].setAttribute('data-vr-field', '');
    return 1;
  }, label);
  expect(marked, `見出しが「${label}」の入力の部品 (ちょうど 1 つ)`).toBe(1);
  return pageBody(page).locator('[data-vr-field]');
}

/** The switch (MkSwitch) whose label is `label` in the page body, ignoring the `Modified` mark. */
export async function switchByLabel(page: Page, label: string): Promise<Locator> {
  return switchIn(pageBody(page), label);
}

/** Fills the input or textarea of the field labelled `label`. */
export async function fillField(page: Page, label: string, value: string): Promise<void> {
  const field = await fieldByLabel(page, label);
  const input = field.locator('input, textarea').first();
  await input.scrollIntoViewIfNeeded();
  await input.fill(value);
}

/** Asserts the input or textarea of the field labelled `label` shows `value`. */
export async function expectField(page: Page, label: string, value: string): Promise<void> {
  await expect((await fieldByLabel(page, label)).locator('input, textarea').first()).toHaveValue(value);
}

/** Picks the option `option` of the radios (MkRadios) labelled `label`. */
export async function pickRadio(page: Page, label: string, option: string): Promise<void> {
  const item = radioItem(await fieldByLabel(page, label), option);
  await item.scrollIntoViewIfNeeded();
  await item.click();
  await expect(item).toHaveAttribute('aria-checked', 'true');
}

/** Asserts the option `option` of the radios labelled `label` is the one picked. */
export async function expectRadio(page: Page, label: string, option: string): Promise<void> {
  await expect(radioItem(await fieldByLabel(page, label), option)).toHaveAttribute('aria-checked', 'true');
}

function radioItem(field: Locator, option: string): Locator {
  return field.locator('[role="checkbox"]').filter({ hasText: new RegExp(`^\\s*${option.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\s*$`) });
}

/**
 * Clicks the page's own `Save` button (the admin pages that save the whole page
 * at once) and waits for `admin/update-meta` to succeed.
 */
export async function savePage(page: Page): Promise<void> {
  const saved = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/update-meta', { timeout: 15_000 });
  const button = page.locator('button').filter({ hasText: /^\s*Save\s*$/ });
  await expect(button, '画面の Save ボタン (ちょうど 1 つ)').toHaveCount(1);
  await button.click();
  expect((await saved).ok(), 'admin/update-meta が失敗した').toBe(true);
}

/**
 * Clicks `Save` in the form footer of the folder whose header is `folder`, and
 * waits for `admin/update-meta` to succeed.
 */
export async function saveFolder(page: Page, folder: string): Promise<void> {
  const saved = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/update-meta', { timeout: 15_000 });
  await clickFolderButton(page, folder, 'Save');
  expect((await saved).ok(), `${folder}: admin/update-meta が失敗した`).toBe(true);
}

export interface MetaRow {
  /** A short name of the row (the keys and the value). */
  name: string;
  /** The `admin/update-meta` keys the row covers (declared for the gate). */
  keys: [string, ...string[]];
  /** The settings page that has the control. */
  route: string;
  /**
   * The meta keys to put back after the row, as `admin/update-meta` takes them.
   * A pair `[write, read]` is for a key `admin/meta` returns under another name
   * (`['swPublicKey', 'swPublickey']`). Defaults to `keys`.
   */
  restore?: (string | [string, string])[];
  /** Keys `admin/meta` does not return, put back from `/api/meta` (`serverRules`, as upstream). */
  restorePublic?: string[];
  /** Makes the data the row needs before the page opens; the result is `ctx.data`. */
  setup?: (ctx: AdminRowContext) => Promise<unknown>;
  /** Removes what `setup` or `effect` made that other specs would see (an ad, a role). Runs even when the row fails. */
  cleanup?: (ctx: AdminRowContext) => Promise<void>;
  /** Operates the page to change the setting (folders are already open). */
  set: (ctx: AdminRowContext) => Promise<void>;
  /** Reads the value from the response of `admin/meta`. */
  read: (meta: Record<string, any>) => unknown;
  /** The value `read` returns once the setting is saved, or a function of the row context (`data` is what `setup` returned). */
  expected: unknown;
  /** Asserts the page shows the saved value after a reload (folders are already open). */
  shows: (ctx: AdminRowContext) => Promise<void>;
  /** Asserts the effect, or the reason it is not checked. */
  effect: Effect;
  /** An issue (`'#4057'`) for a known bug that makes the effect fail (see settings_table.ts). */
  knownBug?: string;
}

/**
 * Runs one meta row: changes the setting on the admin page as root, reads it
 * back with `admin/meta`, reloads and checks the page shows it, then checks the
 * effect. The keys in `restore` go back to the values they had before the row.
 */
export async function runMetaRow({ page, request, baseURL }: { page: Page; request: APIRequestContext; baseURL?: string }, row: MetaRow): Promise<void> {
  skipOnTsBackend();
  skipForMutation(row.effect, row.knownBug);
  const root = rootFixture();
  const before = await apiOk(request, 'admin/meta', { i: root.token });
  const original: Record<string, unknown> = {};
  for (const entry of row.restore ?? row.keys) {
    const [write, read] = typeof entry === 'string' ? [entry, entry] : entry;
    // 読めないキーを戻そうとすると undefined を書き、黙って何も戻さない
    expect(Object.hasOwn(before, read), `admin/meta が ${read} を返さない (restore に書くキーを直す)`).toBe(true);
    // 秘密の値は伏せ字で返り、伏せ字を送り返すと「変えない」と読まれる (internal/api/admin/meta_secrets.go)。
    // 設定済みだと元の値に戻せないので、始める前に落とす (このスタックでは未設定の null)
    expect(typeof before[read] === 'string' && /^\*+$/.test(before[read]), `${read} は設定済みの秘密の値で、行の後に元へ戻せない`).toBe(false);
    original[write] = before[read];
  }
  if (row.restorePublic != null) {
    const pub = await apiOk(request, 'meta', { detail: true });
    for (const k of row.restorePublic) {
      expect(Object.hasOwn(pub, k), `/api/meta が ${k} を返さない`).toBe(true);
      original[k] = pub[k];
    }
  }
  // 効き目を見る利用者は、行が登録の設定を変える前に作る
  const user = await signupUser(request, randomUsername('pwadm'));
  const ctx: AdminRowContext = { page, request, baseURL: baseURL!, user, root };
  try {
    ctx.data = row.setup ? await row.setup(ctx) : undefined;
    const expected = typeof row.expected === 'function' ? (row.expected as (c: AdminRowContext) => unknown)(ctx) : row.expected;
    await signinAdmin(page, baseURL!, root);
    await gotoRoute(page, baseURL!, row.route);
    await openAllFolders(page);
    if (!MUTATION) {
      await row.set(ctx);
      await expect.poll(async () => row.read(await apiOk(request, 'admin/meta', { i: root.token })), { message: `${row.name} が保存されない` }).toEqual(expected);
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
  } finally {
    // 後片付けが落ちても、meta は必ず戻す
    await runCleanup(row.name, row.cleanup, ctx);
    const restored = await callApi(request, 'admin/update-meta', { i: root.token, ...original });
    // 戻せなかったことは記録するが、元の失敗は隠さない (soft)
    expect.soft(restored.ok(), `${row.name}: meta を元の値に戻せなかった`).toBe(true);
  }
}

export type PolicyControl =
  | { kind: 'switch'; on: boolean }
  | { kind: 'select'; label: string }
  | { kind: 'number'; value: number }
  | { kind: 'textarea'; lines: string[] }
  /** Anything else (a list of switches, for example). `shows` asserts the folder shows the value after a reload. */
  | { kind: 'custom'; set: (folder: Locator, ctx: AdminRowContext) => Promise<void>; shows: (folder: Locator, ctx: AdminRowContext) => Promise<void> };

export interface PolicyRow {
  key: string;
  control: PolicyControl;
  /** The value the role stores for the policy. */
  value: unknown;
  /** Makes the data the row needs before the page opens; the result is `ctx.data`. */
  setup?: (ctx: AdminRowContext) => Promise<unknown>;
  /** Removes what `setup` or `effect` made that other specs would see (an ad, a role). Runs even when the row fails. */
  cleanup?: (ctx: AdminRowContext) => Promise<void>;
  /** Asserts the effect on the user the role is assigned to, or the reason it is not checked. */
  effect: Effect;
  /** An issue (`'#4057'`) for a known bug that makes the effect fail (see settings_table.ts). */
  knownBug?: string;
}

/** The title of a policy row's test. */
export function policyTitle(row: PolicyRow): string {
  return `${row.key} → ${JSON.stringify(row.value)}`;
}

/** The policy folder of `key` in the role editor, opened (the outer Policies folder too). */
export async function openPolicyFolder(page: Page, key: string): Promise<Locator> {
  // ポリシーの枠は 52 あり、全部を開くと遅いので、この行の枠だけを開く
  const folder = pageBody(page).locator(`[data-policy-key="${key}"]`);
  await expect(folder, `ポリシーの枠 ${key}`).toHaveCount(1, { timeout: 20_000 });
  const header = folder.getByTestId('folder-header').first();
  if ((await folder.getAttribute('aria-expanded')) !== 'true') await header.click();
  await expect(folder).toHaveAttribute('aria-expanded', 'true');
  return folder;
}

/** The control of the policy value inside the folder: what the slot renders (not the use-base switch or the priority). */
function valueControl(folder: Locator, control: Exclude<PolicyControl, { kind: 'custom' }>): Locator {
  switch (control.kind) {
    case 'switch':
      return folder.getByTestId('switch-toggle');
    case 'select':
      return folder;
    case 'number':
      return folder.locator('input[type="number"]');
    case 'textarea':
      return folder.locator('textarea');
  }
}

async function setPolicyValue(page: Page, folder: Locator, control: PolicyControl, ctx: AdminRowContext): Promise<void> {
  switch (control.kind) {
    case 'switch':
      await setSwitch(await switchIn(folder, 'Enable'), control.on);
      return;
    case 'select':
      await pickSelect(page, folder, control.label);
      return;
    case 'number': {
      const input = valueControl(folder, control);
      await expect(input).toHaveCount(1);
      await fillInput(input, String(control.value));
      return;
    }
    case 'textarea': {
      const ta = valueControl(folder, control);
      await expect(ta).toHaveCount(1);
      await ta.fill(control.lines.join('\n'));
      return;
    }
    case 'custom':
      await control.set(folder, ctx);
  }
}

async function expectPolicyValue(folder: Locator, control: PolicyControl, ctx: AdminRowContext): Promise<void> {
  switch (control.kind) {
    case 'switch':
      await expect(await switchIn(folder, 'Enable')).toHaveAttribute('data-checked', String(control.on));
      return;
    case 'select':
      await expect(folder.locator('[tabindex="0"]').first()).toContainText(control.label);
      return;
    case 'number':
      await expect(valueControl(folder, control)).toHaveValue(String(control.value));
      return;
    case 'textarea':
      await expect(valueControl(folder, control)).toHaveValue(control.lines.join('\n'));
      return;
    case 'custom':
      await control.shows(folder, ctx);
  }
}

const USE_BASE = 'Use role template value';

/**
 * Runs one policy row: makes a manual role and assigns it to a new user,
 * changes the policy in the role editor as root (use-base off, the value,
 * priority high), reads it back with `admin/roles/show`, reloads and checks the
 * editor shows it, then checks the effect on the user. The role is deleted
 * afterwards.
 */
export async function runPolicyRow({ page, request, baseURL }: { page: Page; request: APIRequestContext; baseURL?: string }, row: PolicyRow): Promise<void> {
  skipOnTsBackend();
  skipForMutation(row.effect, row.knownBug);
  const root = rootFixture();
  const user = await signupUser(request, randomUsername('pwpol'));
  const role = await apiOk(request, 'admin/roles/create', {
    i: root.token,
    name: `pw ${row.key}`,
    description: '',
    color: null,
    iconUrl: null,
    target: 'manual',
    condFormula: {},
    isPublic: false,
    isModerator: false,
    isAdministrator: false,
    isExplorable: false,
    asBadge: false,
    canEditMembersByModerator: false,
    displayOrder: 0,
    policies: {},
  }) as { id: string };
  const ctx: AdminRowContext = { page, request, baseURL: baseURL!, user, root };
  try {
    await apiOk(request, 'admin/roles/assign', { i: root.token, roleId: role.id, userId: user.id });
    ctx.data = row.setup ? await row.setup(ctx) : undefined;
    await signinAdmin(page, baseURL!, root);
    await gotoRoute(page, baseURL!, `/admin/roles/${role.id}/edit`);
    await waitForMountedPage(page);
    let folder = await openPolicyFolder(page, row.key);
    if (!MUTATION) {
      await setSwitch(await switchIn(folder, USE_BASE), false);
      await setPolicyValue(page, folder, row.control, ctx);
      // 優先度の部品は枠の中の最後の MkRange (rateLimitFactor の枠は値も MkRange)
      await rangeToMax(page, folder.locator('.timctyfi').last());
      const saved = page.waitForResponse((r) => new URL(r.url()).pathname === '/api/admin/roles/update', { timeout: 15_000 });
      await page.locator('button').filter({ hasText: /^\s*Save\s*$/ }).last().click();
      expect((await saved).ok(), 'admin/roles/update が失敗した').toBe(true);
      await expect.poll(async () => (await apiOk(request, 'admin/roles/show', { i: root.token, roleId: role.id })).policies?.[row.key], { message: `${row.key} が保存されない` })
        .toEqual({ useDefault: false, priority: 2, value: row.value });
      await gotoRoute(page, baseURL!, `/admin/roles/${role.id}/edit`);
    }
    await page.reload({ waitUntil: 'domcontentloaded' });
    await waitForMountedPage(page);
    folder = await openPolicyFolder(page, row.key);
    if (MUTATION) {
      await expectEffectFails(row.key, row.effect, ctx);
      return;
    }
    await expect(await switchIn(folder, USE_BASE)).toHaveAttribute('data-checked', 'false');
    await expectPolicyValue(folder, row.control, ctx);
    await checkEffect(row.key, row.effect, row.knownBug, ctx);
  } finally {
    // 後片付けが落ちても、ロールは必ず消す
    await runCleanup(row.key, row.cleanup, ctx);
    const deleted = await callApi(request, 'admin/roles/delete', { i: root.token, roleId: role.id });
    expect.soft(deleted.ok(), `${row.key}: ロールを消せなかった`).toBe(true);
  }
}

/** Runs a row's cleanup; a failure is recorded (soft) so the rest of the clean-up still runs and the row's own failure is not hidden. */
async function runCleanup(name: string, cleanup: ((ctx: AdminRowContext) => Promise<void>) | undefined, ctx: AdminRowContext): Promise<void> {
  if (cleanup == null) return;
  try {
    await cleanup(ctx);
  } catch (e) {
    expect.soft(e, `${name}: 後片付けが失敗した`).toBeUndefined();
  }
}

/** Calls an API as part of a clean-up and records (soft) when it fails, so a leftover does not go unnoticed. */
export async function cleanupApi(request: APIRequestContext, endpoint: string, body: Record<string, unknown>): Promise<void> {
  const resp = await callApi(request, endpoint, body);
  expect.soft(resp.ok(), `後片付けの ${endpoint} が失敗した (status ${resp.status()})`).toBe(true);
}

/** Asserts that an API call succeeds (an assertion, so the mutation check counts it). */
export async function expectApiOk(request: APIRequestContext, endpoint: string, body: Record<string, unknown>): Promise<any> {
  const resp = await callApi(request, endpoint, body);
  const text = await resp.text();
  expect(resp.ok(), `${endpoint} が通らない (status ${resp.status()}: ${text.slice(0, 200)})`).toBe(true);
  return text === '' ? null : JSON.parse(text);
}

/** Makes a manual role with `policies` (priority high) and assigns it to `userId`. Returns the role id. */
export async function assignRole(request: APIRequestContext, root: RootFixture, userId: string, extra: { isModerator?: boolean; policies?: Record<string, unknown> }): Promise<string> {
  const policies: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(extra.policies ?? {})) policies[k] = { useDefault: false, priority: 2, value: v };
  const role = await apiOk(request, 'admin/roles/create', {
    i: root.token, name: 'pw helper', description: '', color: null, iconUrl: null, target: 'manual', condFormula: {},
    isPublic: false, isModerator: extra.isModerator ?? false, isAdministrator: false, isExplorable: false, asBadge: false,
    canEditMembersByModerator: false, displayOrder: 0, policies,
  }) as { id: string };
  await apiOk(request, 'admin/roles/assign', { i: root.token, roleId: role.id, userId });
  return role.id;
}

/**
 * Asserts that an API call made as `token` fails with the error `code`. The
 * assertion (not a thrown request) is what the mutation check counts.
 */
export async function expectApiError(request: APIRequestContext, endpoint: string, body: Record<string, unknown>, code: string): Promise<void> {
  const resp = await callApi(request, endpoint, body);
  const json = await resp.json().catch(() => ({})) as { error?: { code?: string } };
  expect(json.error?.code, `${endpoint} が ${code} で拒否されない (status ${resp.status()})`).toBe(code);
}
