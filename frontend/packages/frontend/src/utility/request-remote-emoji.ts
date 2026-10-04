/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import * as os from '@/os.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { i18n } from '@/i18n.js';
import { emojiApplicationQuotaText, emojiApplicationPendingLimitText } from '@/utility/emoji-application-quota.js';

// **server の namePattern と同じ (`internal/core/emojiapplication` line 79)。**
// `emoji-request.vue` の NAME_RE と揃えてある。
const NAME_RE = /^[a-zA-Z0-9_]+$/;

// mk-go 独自のエンドポイントなので misskey-js の型集合には無い。
function api<T>(endpoint: string, params: Record<string, unknown> = {}): Promise<T> {
	return misskeyApi(endpoint as never, params as never) as unknown as Promise<T>;
}

/**
 * mk-go: リモート絵文字のインポートを申請する (#2935).
 *
 * `importRemoteEmoji` (#2698) と**同じ導線から呼ばれ、押した先だけが違う**。
 * 権限を持つ人はその場でインポートし、持たない人はここへ来る。
 *
 * **メタデータは取らない。** `admin/emoji/fetch-remote-meta` はモデレーター
 * 専用で、申請者には叩けない。申請者が入力するのは「どの名前で登録したいか」と
 * 「なぜ欲しいか」だけにする。
 *
 * **結果としてカテゴリとエイリアスは空で登録される。** リモート絵文字の行は
 * AP 経由で作られるため category / aliases を持たない (本番の実測でどちらも 0 件)。
 * 権限を持つ人が自分でインポートする経路 (#2698) は `fetch-remote-meta` で
 * 相手から取ってから渡すので、**申請経由のほうが成果物は劣る**。承認したあとに
 * 絵文字管理画面で手直しする運用になる。審査画面から `fetch-remote-meta` を
 * 呼べるようにするのは別の課題 (#2935 のレビュー M2)。
 *
 * **ライセンスは任意。** 取り込み元 (host + name) 自体が出典で、相手の絵文字が
 * `_misskey_license` を持っていれば承認時にそれが入る。申請者は参照できないので、
 * 必須にすると「それらしい嘘」を書かせることになる。
 *
 * **`name` と `host` を別々に受ける。** `MkCustomEmoji` は裸の名前と host を
 * 別の prop で持つので、`name@host` の形をここで組ませると呼び出し側が壊れる
 * (#2698 と同じ理由)。
 */
export async function requestRemoteEmojiImport(name: string, host: string | null | undefined): Promise<void> {
	// **`.` は「このサーバー」を指す (isLocalCustomEmojiName と同じ規則)。**
	// リアクションからの呼び出しは `name@host` を分解して渡すので、ローカルの
	// リアクションだと `.` が来る。ローカルには導線を出していないが、分解の
	// 都合でここへ来る経路があるので弾く (importRemoteEmoji と同じガード)。
	if (host == null || host === '' || host === '.') {
		// ローカル絵文字には導線を出していないので、ここに来るのは呼び出し側の
		// 不具合。黙って何もしないと原因が追えない。
		await os.alert({ type: 'error', text: i18n.ts.somethingHappened });
		return;
	}

	// **名前は送る前に検査する。** `emoji-application/create` は 1 時間 5 回の
	// レート制限が掛かっていて、**失敗も数える** (middleware が handler より前に
	// 走るため)。検査せずに送ると、打ち間違いを 5 回やっただけで 1 時間ロック
	// される — 申請そのものは 1 件も出せていないのに。
	//
	// **入力は持ち回して出し直す。** 弾くたびに空のダイアログを出すと、書いた
	// 理由まで消える。
	let localName = name;
	let license = '';
	let comment = '';
	for (;;) {
		const { canceled, result } = await os.form(i18n.ts._emojiApplication.requestImport, {
			localName: {
				// **required を明示する (レビュー M6)。** MkForm は
				// `'required' in item && item.required` で判定するので、書かないと
				// 空のまま OK が押せて server の INVALID_EMOJI_NAME に落ちる。
				type: 'string',
				required: true,
				label: i18n.ts.name,
				default: localName,
				description: i18n.ts._emojiApplication.nameCaption,
			},
			license: {
				// リモートでは任意 (相手の絵文字が持っていれば承認時に入る)。
				type: 'string',
				multiline: true,
				required: false,
				label: i18n.ts.license,
				default: license,
				description: i18n.ts._emojiApplication.licenseCaptionRemote,
			},
			comment: {
				type: 'string',
				multiline: true,
				label: i18n.ts._emojiApplication.comment,
				default: comment,
				required: false,
			},
		});
		if (canceled) return;

		localName = (result.localName as string | undefined ?? '').trim();
		license = result.license as string | undefined ?? '';
		comment = result.comment as string | undefined ?? '';
		if (NAME_RE.test(localName)) break;
		await os.alert({ type: 'error', text: i18n.ts._emojiApplication.nameInvalid });
	}

	try {
		await api('emoji-application/create', {
			kind: 'remote',
			// **申請する名前とリモートの名前は別。** 手元で別名にしたい場合が
			// あるので分けて送る (承認時はこちらの名前で登録される)。
			name: localName,
			remoteHost: host,
			remoteName: name,
			license,
			comment,
		});
	} catch (err) {
		await os.alert({ type: 'error', text: requestErrorText(err) });
		return;
	}
	await os.alert({ type: 'success', text: i18n.ts._emojiApplication.submitted });
}

/**
 * Maps the API error code onto a message.
 *
 * 種別を潰さない。「失敗しました」だけだと、名前を変えればよいのか、その絵文字が
 * もう取れないのかが分からない。
 */
function requestErrorText(err: unknown): string {
	switch ((err as { code?: string } | null)?.code) {
		case 'DUPLICATE_NAME': return i18n.ts._emojiApplication.errorDuplicateName;
		case 'ALREADY_REQUESTED': return i18n.ts._emojiApplication.errorAlreadyRequested;
		case 'INVALID_EMOJI_NAME': return i18n.ts._emojiApplication.nameInvalid;
		case 'TOO_LONG': return i18n.ts._emojiApplication.errorTooLong;
		// 相手サーバーの絵文字をこのサーバーが知らない (キャッシュから消えた)。
		case 'NO_SUCH_EMOJI': return i18n.ts._emojiApplication.errorRemoteGone;
		case 'ROLE_PERMISSION_DENIED': return i18n.ts._emojiApplication.errorNotAllowed;
		// **レート制限は新しく到達可能になった (レビュー R2-M3)。** 汎用の
		// 「何かがおかしいようです」に潰すと、待てば通ることが分からない。
		case 'RATE_LIMIT_EXCEEDED': return i18n.ts._emojiApplication.errorRateLimited;
		// ロールごとの期間上限 (#2958)。自作画像の申請と枠を共有する。
		case 'EMOJI_APPLICATION_QUOTA_EXCEEDED': return emojiApplicationQuotaText(err);
		// 審査待ちの上限 (#2977)。期間の上限とは解決の仕方が違う (待つのではなく
		// 取り下げるか、結果が出るのを待つ)。
		case 'EMOJI_APPLICATION_PENDING_LIMIT_EXCEEDED': return emojiApplicationPendingLimitText(err);
		default: return i18n.ts.somethingHappened;
	}
}
