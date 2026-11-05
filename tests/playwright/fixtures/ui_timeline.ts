/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// タイムラインと通知の UI spec (#4046) で共有する helper。
//
// **判定はページの本体 (`pageBody`) に絞る。** 画面が広いと右のウィジェット列に
// 通知が出て、ナビには自分の名前が出る (docs/playwright.md)。ノートの本文や
// 相手の名前でページ全体を探すと、ウィジェットに出たもので通ってしまう。
//
// **UI が送った値は、操作の前に待ち受けを張って取る。** 操作の後に張ると、速い
// 応答では待ち受けが間に合わずに時間切れになる。`waitForApiBody` は Promise を
// 返すので、操作の前に呼んで、操作の後に await する。

import { randomBytes } from 'node:crypto';
import { expect, type APIRequestContext, type Page, type Request } from '@playwright/test';
import { callApi } from './api';
import { type Principal, randomUsername, signupUser } from './auth';
import { pageBody } from './route_check';
import { clickWhenReady } from './ui_click';

export { pageBody };

/** A short tag that keeps the note texts of one test apart from every other run. */
export function runTag(prefix: string): string {
  return `${prefix}${randomBytes(4).toString('hex')}`;
}

/** Signs up a fresh user whose username starts with `prefix`. */
export function freshUser(request: APIRequestContext, prefix: string): Promise<Principal> {
  return signupUser(request, randomUsername(prefix));
}

/** Calls an API as `user` and fails unless it answers with a 2xx. Returns the parsed body (null for 204). */
export async function apiOk(request: APIRequestContext, endpoint: string, body: Record<string, unknown>): Promise<any> {
  const resp = await callApi(request, endpoint, body);
  expect(resp.ok(), `${endpoint} failed: ${resp.status()} ${await resp.text()}`).toBe(true);
  return resp.status() === 204 ? null : resp.json();
}

/** Posts a note and returns its id. `extra` carries visibility, replyId, renoteId, poll and so on. */
export async function postNote(request: APIRequestContext, user: Principal, text: string | null, extra: Record<string, unknown> = {}): Promise<string> {
  const body = await apiOk(request, 'notes/create', { i: user.token, ...(text === null ? {} : { text }), ...extra });
  return body.createdNote.id as string;
}

/** Makes `follower` follow `followee` (the followee must not be locked). */
export async function follow(request: APIRequestContext, follower: Principal, followee: Principal): Promise<void> {
  await apiOk(request, 'following/create', { i: follower.token, userId: followee.id });
}

/**
 * Starts waiting for the next request the page sends to /api/<endpoint> that
 * `match` accepts, and resolves with its JSON body. Call it before the action.
 */
export function waitForApiBody(
  page: Page,
  endpoint: string,
  match: (body: Record<string, unknown>) => boolean = () => true,
  timeout = 20_000,
): Promise<Record<string, unknown>> {
  const p = page
    .waitForRequest(
      (req: Request) => {
        if (req.method() !== 'POST' || !req.url().endsWith(`/api/${endpoint}`)) return false;
        const body = req.postDataJSON() as Record<string, unknown> | null;
        return body !== null && match(body);
      },
      { timeout },
    )
    .then((req) => req.postDataJSON() as Record<string, unknown>);
  // 先に別の判定で落ちたときに、待ち受けの reject が未処理のまま残らないようにする
  p.catch(() => {});
  return p;
}

/** Records the bodies of every request the page sends to /api/<endpoint> from now on. */
export function recordApiBodies(page: Page, endpoint: string): Record<string, unknown>[] {
  const bodies: Record<string, unknown>[] = [];
  page.on('request', (req) => {
    if (req.method() === 'POST' && req.url().endsWith(`/api/${endpoint}`)) {
      bodies.push((req.postDataJSON() as Record<string, unknown> | null) ?? {});
    }
  });
  return bodies;
}

/**
 * Waits until every text in `shown` is visible in the page body, then checks
 * that no text in `hidden` is there.
 *
 * 出ないことは、出るべきものが全部出た後に確かめる。一覧は 1 回の取得で
 * まとめて描かれるので、出るべきものが揃った時点で、同じ取得に入っていた
 * 出ないはずのものも描かれている。
 */
export async function expectNotesInBody(page: Page, shown: string[], hidden: string[], timeout = 20_000): Promise<void> {
  const body = pageBody(page);
  // 部分一致で探す。本文にメンションを含むノートは、メンションのリンクと残りの
  // 文字が別の要素に分かれるので、完全一致では取れない。どの spec も、互いに
  // 部分文字列にならない文字列を渡す
  for (const text of shown) {
    await expect(body.getByText(text).first(), `「${text}」が本体に出ない`).toBeVisible({ timeout });
  }
  for (const text of hidden) {
    await expect(body.getByText(text), `「${text}」が本体に出てしまった`).toHaveCount(0);
  }
}

/** Scrolls every scrollable element in the page body (and the window) to its bottom. */
export async function scrollPageBodyToBottom(page: Page): Promise<void> {
  await scrollPageBody(page, 'bottom');
}

/** Scrolls the page body back to its top, so that its bottom leaves the viewport. */
export async function scrollPageBodyToTop(page: Page): Promise<void> {
  await scrollPageBody(page, 'top');
}

async function scrollPageBody(page: Page, to: 'top' | 'bottom'): Promise<void> {
  await page.evaluate((to) => {
    const containers = document.querySelectorAll('._pageContainer');
    const root = containers[containers.length - 1] as HTMLElement | undefined;
    if (!root) throw new Error('._pageContainer が無い');
    // スクロールするのは本体の中の、中身が溢れている要素 (ページの作りで場所が変わる)
    const scrollers = [root, ...Array.from(root.querySelectorAll<HTMLElement>('*'))].filter(
      (el) => el.scrollHeight > el.clientHeight + 1 && ['auto', 'scroll'].includes(getComputedStyle(el).overflowY),
    );
    for (const el of scrollers) el.scrollTop = to === 'bottom' ? el.scrollHeight : 0;
    window.scrollTo(0, to === 'bottom' ? document.documentElement.scrollHeight : 0);
  }, to);
}

/** Clicks the page header tab with `data-testid="<testId>"` in the page body. */
export async function clickPageTab(page: Page, testId: string): Promise<void> {
  const tab = pageBody(page).getByTestId(testId);
  await expect(tab, `タブ ${testId} が無い`).toHaveCount(1, { timeout: 20_000 });
  await tab.click();
}

/** Clicks the popup menu item whose text is exactly `text` (menus are rendered outside the page body). */
export async function clickMenuItem(page: Page, text: string): Promise<void> {
  const item = page.locator('[role="menu"] [role="menuitem"]').filter({ hasText: new RegExp(`^\\s*${text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\s*$`) });
  await expect(item, `メニューの「${text}」がちょうど 1 つ出ない`).toHaveCount(1, { timeout: 10_000 });
  await item.click();
}

/**
 * Records the channels the page subscribes to on the streaming WebSocket.
 * Call it before the page opens the stream.
 */
export function recordStreamChannels(page: Page): { channels: string[]; sent: string[] } {
  const state = { channels: [] as string[], sent: [] as string[] };
  page.on('websocket', (ws) => {
    if (!ws.url().includes('/streaming')) return;
    ws.on('framesent', (frame) => {
      const payload = typeof frame.payload === 'string' ? frame.payload : frame.payload.toString('utf-8');
      state.sent.push(payload);
      try {
        const msg = JSON.parse(payload) as { type?: string; body?: { channel?: string } };
        if (msg.type === 'connect' && typeof msg.body?.channel === 'string') state.channels.push(msg.body.channel);
      } catch {
        // ping などの JSON でない frame は数えない
      }
    });
  });
  return state;
}

/**
 * Waits until the page has subscribed to `channel` at least `times` times.
 *
 * 購読する前に投稿すると、ストリーミングでは届かない (pub/sub)。投稿の前に、
 * 画面が購読を送ったことを待つ。
 */
export async function waitForStreamChannel(state: { channels: string[] }, channel: string, times = 1): Promise<void> {
  await expect
    .poll(() => state.channels.filter((c) => c === channel).length, { message: `${channel} を購読しない`, timeout: 20_000 })
    .toBeGreaterThanOrEqual(times);
}

/** A streaming connection the spec can cut and restore. */
export interface SwitchableStream {
  /** Channels the page subscribed to, across every connection. */
  channels: string[];
  /** How many connections the page opened while the switch was on. */
  connections: number;
  /** How many connections the page attempted while the switch was off (they are closed at once). */
  refused: number;
  /** Closes every live connection and refuses new ones until `restore()`. */
  cut(): Promise<void>;
  /** Lets the page connect again. */
  restore(): void;
}

/**
 * Routes the streaming WebSocket through the spec so that it can be cut.
 * Call it before the page opens the stream.
 *
 * **`context.setOffline` だけでは切れない。** Chromium のオフラインの模擬は、
 * 張ってある WebSocket を閉じない (実測: setOffline(true) の後も close が来ず、
 * 配信も届き続けた)。WebSocket を spec の中継に通し、中継の側で閉じる。
 * 切っている間に張り直しに来たものは、すぐ閉じる (サーバーへは繋がない)。
 */
export async function routeSwitchableStream(page: Page): Promise<SwitchableStream> {
  let on = true;
  const live = new Set<{ close: () => Promise<void> }>();
  const state: SwitchableStream = {
    channels: [],
    connections: 0,
    refused: 0,
    cut: async () => {
      on = false;
      await Promise.all([...live].map((c) => c.close()));
      live.clear();
    },
    restore: () => {
      on = true;
    },
  };
  await page.routeWebSocket(/\/streaming(\?|$)/, (ws) => {
    if (!on) {
      state.refused++;
      void ws.close({ code: 4000, reason: 'cut by spec' });
      return;
    }
    state.connections++;
    const server = ws.connectToServer();
    // onMessage を付けると、ページからサーバーへの自動の転送が止まるので、自分で送る
    ws.onMessage((message) => {
      if (typeof message === 'string') {
        try {
          const msg = JSON.parse(message) as { type?: string; body?: { channel?: string } };
          if (msg.type === 'connect' && typeof msg.body?.channel === 'string') state.channels.push(msg.body.channel);
        } catch {
          // JSON でない frame (ping など) は数えない
        }
      }
      server.send(message);
    });
    const conn = {
      close: async () => {
        await ws.close({ code: 4000, reason: 'cut by spec' }).catch(() => {});
        await server.close().catch(() => {});
      },
    };
    live.add(conn);
    // onClose を設定すると、閉じたことの自動の転送が止まる (Playwright の仕様)。ページの
    // 側で閉じたとき (再読み込みなど) にサーバーとの接続が残らないよう、自分で閉じる
    ws.onClose(() => {
      live.delete(conn);
      void server.close().catch(() => {});
    });
  });
  return state;
}

/**
 * Clicks the `<button>` in the page body whose trimmed text is exactly `text`.
 *
 * 押すのは programmatic click (fixtures/ui_click.ts)。広い画面では、初回に出る
 * 案内のダイアログ (About のお知らせ) が本体の下の方に重なり、座標で押す click を奪う。
 */
export function clickBodyButton(page: Page, text: string): Promise<void> {
  return clickWhenReady(
    page,
    `本体のボタン「${text}」`,
    (t: string) => {
      const bodies = document.querySelectorAll('._pageContainer');
      const body = bodies[bodies.length - 1];
      return Array.from(body?.querySelectorAll('button') ?? []).find((b) => (b.textContent ?? '').trim() === t);
    },
    text,
  );
}
