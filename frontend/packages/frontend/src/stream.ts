/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import * as Misskey from 'misskey-js';
import { markRaw } from 'vue';
import { $i } from '@/i.js';
import { wsOrigin } from '@@/js/config.js';

// heart beat interval in ms
const HEART_BEAT_INTERVAL = 1000 * 60;

// バックグラウンドに置かれていた時間がこれを超えたら、生死を判定せず接続を張り直す。
//
// モバイルの PWA では OS がサスペンド中に TCP を切ることがあるが、そのときブラウザは
// close を配送せず readyState が OPEN のまま残る。この状態はクライアントからは
// 見分けられない (heartbeat は投げっぱなしで、サーバーは返事をしない) ので、
// 疑わしいだけ間隔が空いたら確認せず張り直す。
//
// 生きた接続を誤って張り直す代償は、ハンドシェイクと購読の再送、およびその間
// (実測でミリ秒単位) に発行されたイベントの取りこぼしで、UI には出さない。
// 一方で張り直しは毎回サーバー側の接続を作り直すので、閾値を下げるほど
// タブを切り替えるだけの操作で接続チャーンが増える。
//
// iOS / Android はアプリを十数秒でサスペンドしうるので短めに取りつつ、通知を見て
// 戻るだけの往復で毎回張り替わらない下限として 20 秒にしてある。
const RECONNECT_AFTER_HIDDEN = 1000 * 20;

let stream: Misskey.IStream | null = null;
let timeoutHeartBeat: number | null = null;
let lastHeartbeatCall = 0;
let hiddenAt: number | null = null;

export function useStream(): Misskey.IStream {
	if (stream) return stream;

	stream = markRaw(new Misskey.Stream(wsOrigin, $i ? {
		token: $i.token,
	} : null));

	if (timeoutHeartBeat) window.clearTimeout(timeoutHeartBeat);
	timeoutHeartBeat = window.setTimeout(heartbeat, HEART_BEAT_INTERVAL);

	// 既に隠れている状態で最初に呼ばれることがある (バックグラウンドタブとして
	// 復元されたセッション、裏で起動した PWA)。hidden の遷移を観測できないので
	// ここで入れておかないと、何時間放置されても復帰時に張り直さない。
	// この接続はまさに放置されてゾンビ化しやすい側にあたる。
	if (window.document.visibilityState !== 'visible') hiddenAt = Date.now();

	window.document.addEventListener('visibilitychange', () => {
		if (!stream) return;

		if (window.document.visibilityState !== 'visible') {
			hiddenAt = Date.now();
			return;
		}

		// 復帰時に一定時間以上空いていたら張り直す。張り直した接続では misskey-js が
		// チャンネルを購読し直すので、利用者にリロードを求める必要は無い。
		const hiddenFor = hiddenAt == null ? 0 : Date.now() - hiddenAt;
		hiddenAt = null;
		if (hiddenFor >= RECONNECT_AFTER_HIDDEN) {
			stream.reconnect();
			return;
		}

		// send heartbeat right now when last send time is over HEART_BEAT_INTERVAL
		if (Date.now() - lastHeartbeatCall < HEART_BEAT_INTERVAL) return;
		heartbeat();
	});

	return stream;
}

function heartbeat(): void {
	if (stream != null && window.document.visibilityState === 'visible') {
		stream.heartbeat();
	}
	lastHeartbeatCall = Date.now();
	if (timeoutHeartBeat) window.clearTimeout(timeoutHeartBeat);
	timeoutHeartBeat = window.setTimeout(heartbeat, HEART_BEAT_INTERVAL);
}
