/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { defineAsyncComponent } from 'vue';
import type * as Elythia from 'elythia-js';
import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { i18n } from '@/i18n.js';
import { customEmojisMap } from '@/custom-emojis.js';
import { $i } from '@/i.js';

// 取得に成功したかで持つ項目が違うので、fetched で絞ってから読む。
export type RemoteEmojiMeta = Elythia.RemoteEmojiMeta;

/**
 * mk-go: リモート絵文字をその場からインポートする (#2698)。
 *
 * 投稿本文中の絵文字とリアクションの**両方**から呼ぶので、ここに集約する。
 * CherryPick は本文からはモーダルを出し、リアクションからは endpoint を直接
 * 叩いていて挙動が揃っていない。そこは踏襲しない。
 *
 * **`name` と `host` を別々に受ける。** `MkCustomEmoji` は
 * `name`（ホスト無しの裸の名前）と `host` を別の prop で受け取るので、
 * `name@host` の形をここで組み立てさせると呼び出し側が壊れる。
 *
 * **取得はここで 1 回だけ行い、結果をモーダルへ渡す。** モーダル側でも取ると
 * 1 回のインポートで相手へ 2 リクエスト出ることになる。
 */
/**
 * Strip the `@host` suffix from an emoji name (#2903).
 *
 * `MkCustomEmoji` は `name` に `@host` を含めないが、リアクション側の
 * `getEmojiNameFromReaction` は `name@host` を返す。両方を受けられるようにする。
 *
 * **1 箇所に集約する。** 同じ分解が 3 箇所に散らばっていたせいで、リアクション側の
 * 同名判定を「変数は作ったが条件式に配線し忘れる」形で出荷しかけた。
 */
export function bareEmojiName(name: string): string {
	return name.includes('@') ? name.slice(0, name.lastIndexOf('@')) : name;
}

/**
 * Whether a local custom emoji with the same (bare) name already exists (#2903).
 *
 * 既にあるならインポートの導線を出さない。押しても `admin/emoji/copy` が
 * 重複で弾くだけで、押してみるまで分からなかった。
 */
export function hasLocalEmojiWithSameName(name: string): boolean {
	const bare = bareEmojiName(name);
	return bare !== '' && customEmojisMap.has(bare);
}

/**
 * Opens the import dialog for a remote emoji and resolves with the name it was
 * saved under, or `null` when nothing was imported (closed, failed, or bad
 * arguments).
 *
 * **ダイアログが閉じるまで待つ (#3187)。** 取り込んだ絵文字でそのままリアクション
 * する導線が、確定した名前を要る。名前は取り込み時に直せる (#2998) ので、呼び出し側が
 * 渡した名前を使うと別の絵文字を指す。
 */
export async function importRemoteEmoji(name: string, host: string | null | undefined): Promise<string | null> {
	if (name === '' || host == null || host === '' || host === '.') return null;
	const bare = bareEmojiName(name);
	if (bare === '') return null;

	// **取得に失敗しても id と画像 URL は返る**ので、そのままモーダルを開いて
	// 手で埋めてもらう。ここで弾くと「取り込めない絵文字」ができてしまう。
	let res: RemoteEmojiMeta;
	try {
		const meta = await misskeyApi('admin/emoji/fetch-remote-meta', { name: bare, host });
		res = meta;
	} catch {
		os.alert({ type: 'error', text: i18n.ts.somethingHappened });
		return null;
	}

	return new Promise((resolve) => {
		let imported: string | null = null;
		const { dispose } = os.popup(defineAsyncComponent(() => import('@/components/MkRemoteEmojiEditDialog.vue')), {
			emoji: {
				id: res.emojiId,
				name: res.name,
				host: res.host,
				license: res.license ?? null,
				url: res.originalUrl,
			},
			meta: res,
		}, {
			done: (saved: string) => {
				imported = saved;
			},
			// **閉じた時点で決着させる。** done は閉じる前に来るので、閉じただけ
			// (取り消し) なら null のまま返る。
			closed: () => {
				dispose();
				resolve(imported);
			},
		});
	});
}

/**
 * Whether the signed-in user can import remote emojis directly (as opposed to
 * only requesting an import).
 *
 * #3187 の「インポートしてリアクション」は**取り込めるときだけ**出す。申請はすぐには
 * 通らないので、申請しかできない人に出すとリアクションできないまま終わる。
 */
export function canImportRemoteEmoji(): boolean {
	return $i != null && ($i.isModerator || $i.policies.canManageCustomEmojis);
}

/**
 * Imports a remote emoji and reacts with it once the import is done (#3187).
 *
 * 送るのは `:name@.:`。**frontend の絵文字一覧の更新を待たない** — backend は
 * リアクションの解決でキャッシュを通さず DB を引く (`FindByNameAndHost`) ので、
 * 取り込み直後でも解決できる。取り消し / 失敗ではリアクションしない。
 */
export async function importRemoteEmojiAndReact(
	name: string,
	host: string | null | undefined,
	react: (reaction: string) => void | Promise<void>,
): Promise<void> {
	const saved = await importRemoteEmoji(name, host);
	if (saved == null || saved === '') return;
	await react(`:${saved}@.:`);
}
