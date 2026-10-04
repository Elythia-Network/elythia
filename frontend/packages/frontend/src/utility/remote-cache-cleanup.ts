/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/**
 * Decides whether "clear cached files" has anything to delete (#3102).
 *
 * **mk-go はリモートメディアをキャッシュしない** (docs/divergence.md 5.5) ので、
 * mk-go が作った行に削除対象は無い (`upsertAttachments` は常に `isLink: true` /
 * `size: 0` の行しか作らない)。**しかし純正 Misskey から引き継いだ DB には、
 * `cacheRemoteFiles` が有効だった時期の実体つきの行が残る。** その運用者にとっては
 * 消せるはずのものが UI から消せない状態だった。
 *
 * 判定は画面から切り出してある。#3104 / #3105 で「言ってよいこと」の分岐を 3 周
 * 続けて間違えた経験から、**確かめていないことを断定しない**枝を表で固定する。
 */

/** Subset of `admin/drive/usage` の remote バケット。 */
export type RemoteUsageBucket = {
	/** リモート行の総数。 */
	count: number;
	/** `size` 列の合計 (バイト)。実体を持たない行は 0。 */
	size: number;
	/** `count` のうち実体を持たない行 (`isLink = true`) の数。 */
	linkCount: number;
};

export type CleanRemoteFilesState =
	/** 実体つきの行がある。押せる。 */
	| 'ready'
	/** 実体つきの行が 1 件も無い。押せない (押させない)。 */
	| 'none'
	/** 集計を取れていない。**「対象が無い」とは言えない**ので押せるままにする。 */
	| 'unknown';

/**
 * @param remote `admin/drive/usage` の `remote`。取得できていなければ null。
 */
export function cleanRemoteFilesState(remote: RemoteUsageBucket | null | undefined): CleanRemoteFilesState {
	// **取れていないときに「対象が無い」と断定しない。** `admin/drive/usage` は
	// mk-go 独自の endpoint なので、**純正 backend に向けたときは必ず失敗する** —
	// そちらはリモートメディアを実際にキャッシュするので、押せなくするのは誤り。
	// 一時的な失敗でも同じ判断でよい (実行そのものは確認ダイアログが守る)。
	if (remote == null) return 'unknown';
	return cachedRemoteFileCount(remote) > 0 ? 'ready' : 'none';
}

/**
 * 実体を持つリモート行の数。
 *
 * **`count - linkCount` で数える。** `size > 0` では数えられない — 実体はあるが
 * `size` が 0 の行 (純正が length を取れなかったケース) を取りこぼす。
 * 負にならないよう 0 で止める (集計が別々のクエリ由来で食い違いうるため)。
 */
export function cachedRemoteFileCount(remote: RemoteUsageBucket): number {
	return Math.max(0, remote.count - remote.linkCount);
}
