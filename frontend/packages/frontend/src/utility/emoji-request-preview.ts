/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { i18n } from '@/i18n.js';
import { prefer } from '@/preferences.js';
import { getProxiedImageUrl, getStaticImageUrl } from '@/utility/media-proxy.js';

/**
 * 申請の画像の解決結果 (#2989)。backend の `emoji-application/list-mine` が返す。
 *
 * **URL が空かどうかで状態を推測しない。** 「申請元が削除された」「承認後の
 * 絵文字が消された」「DB 障害で確認できなかった」は利用者に出す文面が違う。
 */
export type ApplicationPreview = {
	url: string;
	source: 'applicationFile' | 'remoteEmoji' | 'approvedEmoji';
	state: 'available' | 'sourceGone' | 'approvedEmojiGone' | 'unknown';
};

/**
 * 表示する画像 URL。出せないときは null。
 *
 * **リモートの生 URL は本番の CSP で読めない。** `img-src 'self' data: blob:` を
 * enforce しているので、任意の origin の画像はブロックされる (#2903 / #2935 /
 * #2957 で 3 回踏んだ形)。media proxy を通すと同一オリジンになり、allowlist が
 * `emoji.originalUrl` / `publicUrl` を通すので解決できる。
 *
 * **drive のファイルと承認後の絵文字は通さない。** どちらも自サーバーの URL で、
 * 承認後は #2966 で system 所有の drive ファイルへ複製されている。
 *
 * **静止画設定は `getStaticImageUrl` で包む。** 第 4 引数は `noFallback` で
 * あって静止画とは無関係 (`media-proxy.ts` のシグネチャ)。包まないと
 * `disableShowingAnimatedImages` を on にしていてもここだけ動く。
 */
export function applicationPreviewUrl(preview: ApplicationPreview | undefined, broken: boolean): string | null {
	if (preview == null || preview.state !== 'available' || preview.url === '') return null;
	if (broken) return null;
	// `@error` の受け皿があるので `noFallback` を渡す (`MkCustomEmoji` と同じ扱い)。
	const resolved = preview.source === 'remoteEmoji'
		? getProxiedImageUrl(preview.url, 'emoji', false, true)
		: preview.url;
	return prefer.s.disableShowingAnimatedImages ? getStaticImageUrl(resolved) : resolved;
}

/**
 * 画像が出せないときの文面 (#2989)。
 *
 * **「確認できなかった」を「削除済み」に丸めない。** 丸めると、実際には残って
 * いる画像について利用者が「消えた」と判断して再申請する。読み込み失敗も同じ
 * 理由で別の文面にする — API 上は存在しているので、「削除済み」は嘘になる。
 */
export function applicationPreviewMessage(preview: ApplicationPreview | undefined, broken: boolean): string {
	if (broken) return i18n.ts._emojiApplication.previewLoadFailed;
	switch (preview?.state) {
		case 'sourceGone':
			return preview.source === 'remoteEmoji'
				? i18n.ts._emojiApplication.previewRemoteGone
				: i18n.ts._emojiApplication.previewFileGone;
		case 'approvedEmojiGone':
			return i18n.ts._emojiApplication.previewApprovedEmojiGone;
		default:
			// state が無い (古い応答 / lookup 未配線) 場合も「確認できなかった」。
			return i18n.ts._emojiApplication.previewUnknown;
	}
}
