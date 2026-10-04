/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/**
 * mk-go: ローカル絵文字の名前として使える綴りか (#2998)。
 *
 * backend は `admin/emoji/copy` / `admin/emoji/add` の両方で `^[a-zA-Z0-9_]+$` を
 * 要求し、`emoji.name` は varchar(128)。**リモート絵文字の名前は相手が決める値**なので
 * これを満たすとは限らず (本番の実測ではリモート 21,505 件のうち 10 件が制約外)、
 * そのまま取り込むと **MFM の `:name:` から参照できない絵文字**ができる。
 *
 * **インポートの 3 導線 (個別ダイアログ / 管理画面のリモート一覧 / 従来の管理画面) は
 * ここに集約する。** 式を書き分けると、条件を変えたときに一部の画面だけ違う状態が残る
 * (#2984 と同じ形)。
 *
 * **画面全体で 1 つにはなっていない。** 同じ綴りの判定は登録フォーム
 * (`custom-emojis-manager.register.vue` / `.local.list.vue` の `validators.regex`) と
 * 申請フォーム (`emoji-request.vue` / `request-remote-emoji.ts` の `NAME_RE`) にもある。
 * あちらは vue の validator 形式に埋まっていて型が違い、**長さの上限も持たない**
 * (backend 側も `admin/emoji/add` は #2998 で足したばかり)。まとめるなら別途。
 *
 * **`import-remote-emoji.ts` ではなく独立したファイルに置く。** あちらは `@/os.js` を
 * 引き込むので、単体テストから読むと DOM を触る module chain ごと評価されて落ちる
 * (#2989 で `emoji-request-preview.ts` を切り出したのと同じ理由)。
 *
 * 長さは code unit 数で見てよい — 通る文字が ASCII だけなので rune 数と一致する。
 */
export const EMOJI_NAME_MAX_LENGTH = 128;

export function isUsableEmojiName(name: string): boolean {
	return /^[a-zA-Z0-9_]+$/.test(name) && name.length <= EMOJI_NAME_MAX_LENGTH;
}
