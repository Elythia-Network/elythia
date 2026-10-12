/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Misskey の streaming WebSocket helper。streaming spec (#815) と
// notifications spec (#823, #847) で共有する。
//
// 提供 API:
//   - openStream(token): /streaming?i=<token> に WS connect + open 待ち。
//     close (= server reject) / error は即 fail として報告
//   - subscribeChannel(ws, channel, params?): channel subscribe message を
//     送信し subId を返す (= ack 不在のため fire and forget、subscribe
//     確定までの 200ms バッファは caller が制御)
//   - awaitChannelEvent<T>(ws, match, timeoutMs?): caller-supplied match
//     関数で channel envelope を絞り込み、最初の 1 通の body を resolve、
//     timeout で reject。spec 上 5s 既定。

const baseURL = process.env.MK_BASE_URL ?? 'https://mkgo.local';
const wsURL = baseURL.replace(/^http/, 'ws');

// openStream connects to /streaming?i=<token> and resolves once `open`.
// `error` / `close` (before open) は即 fail として reject。
export async function openStream(token: string): Promise<WebSocket> {
  const ws = new WebSocket(`${wsURL}/streaming?i=${token}`);
  await new Promise<void>((resolve, reject) => {
    ws.addEventListener('open', () => resolve(), { once: true });
    ws.addEventListener('error', () => reject(new Error('ws connection error')), { once: true });
    ws.addEventListener(
      'close',
      (ev) =>
        reject(
          new Error(
            `ws closed before open: code=${ev.code} reason=${ev.reason || '(empty)'}`,
          ),
        ),
      { once: true },
    );
  });
  return ws;
}

// subscribeChannel sends a `connect` message to the given WS to subscribe
// the named channel. Returns the random subId used so callers can match
// channel events.
export function subscribeChannel(ws: WebSocket, channel: string, params: Record<string, unknown> = {}): string {
  const subId = 'sub-' + Math.random().toString(16).slice(2);
  ws.send(
    JSON.stringify({
      type: 'connect',
      body: { channel, id: subId, params },
    }),
  );
  return subId;
}

// ChannelEvent は `{type:"channel", body:{id, type, body}}` の body 部分
// (= channel-specific payload)。caller は予期する shape に narrow する。
export interface ChannelEvent<T> {
  id: string;
  type: string;
  body: T;
}

// awaitSubscribeBuffer は subscribe → publish の race を防ぐための短時間
// バッファ。upstream は subscribe ack を返さないので、connect message が
// server に到達して channel registry に登録されるまでの猶予として 200ms
// 待つ (streaming spec で実用上十分と確認済み)。
export async function awaitSubscribeBuffer(): Promise<void> {
  await new Promise<void>((resolve) => setTimeout(resolve, 200));
}

// awaitChannelEvent registers a message listener and resolves with the
// first matching `{type:"channel"}` envelope's `body`. Caller-supplied
// `match` decides whether the event is the target. Rejects on timeout.
//
// 投稿 / reaction 等の trigger 前に呼んで promise を保持しておくのが
// 正しい使い方 (= race 回避)。
export function awaitChannelEvent<T>(
  ws: WebSocket,
  match: (envelope: ChannelEvent<T>) => boolean,
  timeoutMs = 5000,
): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const handler = (ev: MessageEvent) => {
      let msg: { type?: string; body?: ChannelEvent<T> };
      try {
        msg = JSON.parse(typeof ev.data === 'string' ? ev.data : '');
      } catch {
        return;
      }
      if (msg.type === 'channel' && msg.body && match(msg.body)) {
        ws.removeEventListener('message', handler);
        resolve(msg.body.body);
      }
    };
    ws.addEventListener('message', handler);
    setTimeout(() => {
      ws.removeEventListener('message', handler);
      reject(new Error('channel event not received within timeout'));
    }, timeoutMs);
  });
}
