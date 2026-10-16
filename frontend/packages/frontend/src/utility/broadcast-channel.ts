/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Elythia: ライブラリ broadcast-channel の代わりに、ブラウザの BroadcastChannel を使う (#3613)。
// ライブラリは Safari に BroadcastChannel が無かったころのためのもので、対応ブラウザ
// (chrome130 / firefox132 / safari18.2) では中で同じ BroadcastChannel を使っていた。
// 起動時の JS から外すため (gzip で約 3KB)。
//
// **メッセージの形はライブラリ (7.4.0) と同じにする。** ライブラリは値を
// `{ time, type: 'message', data }` に包んで送り、受けた側は type で振り分けて data だけを
// listener に渡す。チャンネルの名前も同じなので、形を変えると、デプロイの前に開いた
// タブ (ライブラリの版) と後に開いたタブが混在する間、一斉リロードや設定の同期が壊れる。
// listener を足した時刻より前に送られたメッセージを渡さないのも、ライブラリと同じ。

/** The envelope the `broadcast-channel` package puts every message in. */
export type BroadcastEnvelope<T> = {
	time: number;
	type: 'message' | 'internal';
	data: T;
};

let lastMicroSeconds = 0;

/** Same clock as `microSeconds()` of the package: Date.now() in microseconds, strictly increasing. */
export function microSeconds(): number {
	let ret = Date.now() * 1000;
	if (ret <= lastMicroSeconds) ret = lastMicroSeconds + 1;
	lastMicroSeconds = ret;
	return ret;
}

function isEnvelope(value: unknown): value is BroadcastEnvelope<unknown> {
	return typeof value === 'object' && value != null
		&& typeof (value as { time?: unknown }).time === 'number'
		&& typeof (value as { type?: unknown }).type === 'string'
		&& 'data' in value;
}

/**
 * A typed wrapper of the native BroadcastChannel that speaks the message
 * format of the `broadcast-channel` package used before.
 */
export class TypedBroadcastChannel<T> {
	readonly #channel: BroadcastChannel;

	constructor(name: string) {
		this.#channel = new BroadcastChannel(name);
	}

	postMessage(message: T): Promise<void> {
		const envelope: BroadcastEnvelope<T> = { time: microSeconds(), type: 'message', data: message };
		try {
			this.#channel.postMessage(envelope);
		} catch (err) {
			// ライブラリと同じく、送れないメッセージ (複製できない値など) は例外にせず reject で返す
			return Promise.reject(err);
		}
		return Promise.resolve();
	}

	addEventListener(type: 'message', listener: (message: T) => void): void {
		const since = microSeconds();
		this.#channel.addEventListener('message', (ev) => {
			const envelope = (ev as MessageEvent<unknown>).data;
			if (!isEnvelope(envelope) || envelope.type !== type) return;
			if (envelope.time < since) return;
			listener(envelope.data as T);
		});
	}

	close(): void {
		this.#channel.close();
	}
}
