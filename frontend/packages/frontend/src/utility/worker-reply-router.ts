/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/** The part of `WorkerMultiDispatch` the router needs. */
export type ReplySource = {
	addListener(callback: (this: Worker, ev: MessageEvent<unknown>) => unknown): void;
};

/**
 * Routes replies from a worker pool to the caller that asked, by `id`.
 *
 * One message listener is registered on the pool for the whole lifetime of the router,
 * however many callers are waiting.
 */
export class WorkerReplyRouter<T extends { id: string }> {
	private callbacks = new Map<string, (data: T) => void>();

	constructor(source: ReplySource) {
		// 呼び出し側ごとにリスナーを足すと、1 通の返事が待っている全員に配られ、
		// 件数の 2 乗で重くなる (#3935)。ここで 1 つだけ受けて id で引く。
		source.addListener((ev) => {
			const data = ev.data;
			if (data == null || typeof data !== 'object' || !('id' in data) || typeof data.id !== 'string') return;
			this.callbacks.get(data.id)?.(data as T);
		});
	}

	/** Registers (or replaces) the callback for `id`. */
	public set(id: string, callback: (data: T) => void): void {
		this.callbacks.set(id, callback);
	}

	/** Stops delivering replies for `id`. */
	public delete(id: string): void {
		this.callbacks.delete(id);
	}

	/** The number of callers currently registered. */
	public get size(): number {
		return this.callbacks.size;
	}
}
