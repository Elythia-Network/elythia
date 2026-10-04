/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { compareVersions } from 'compare-versions';

/** localStorage key this module owns, paired with the value to write. */
export type VersionToPersist = readonly ['lastVersion' | 'lastMkGoVersion', string];

/**
 * Result of deciding whether the "client was updated" dialog should be shown.
 */
export type ClientUpdateCheck = {
	updated: boolean;
	/** True when the decision was made on the mk-go release version. */
	isMkGo: boolean;
	/** The version the dialog should show. */
	version: string;
	/**
	 * What the caller must write back to localStorage. Empty when nothing changed.
	 */
	persist: readonly VersionToPersist[];
};

// **初回訪問とダウングレードでは出さない。** last が null なのは localStorage を
// 消した直後や新しい端末で、そこで「更新されました」と出すのは嘘になる。
// compareVersions は解釈できない文字列で throw するので握り潰す (upstream の
// boot も同じ理由で try/catch していた)。
function bumped(current: string, last: string | null): boolean {
	if (last == null) return false;
	try {
		return compareVersions(current, last) === 1;
	} catch {
		return false;
	}
}

/**
 * Decides whether the update dialog should be shown, and for which version.
 */
// **mkGoVersion を基準にする (#2939)。** Misskey 側の版は upstream 追従のときしか
// 動かないので、fork の変更を何度重ねても利用者には一度も出なかった。
//
// **frontend の fork タグ (mkGoFrontendVersion) では判定できない** — compareVersions は
// 英字サフィックスを見ないので `mk.9a` と `mk.9` が等しい扱いになる (実測)。
// mkGoVersion は素の semver なのでその問題が無い。
export function resolveClientUpdate(args: {
	misskeyVersion: string;
	mkGoVersion: string | null;
	lastMisskeyVersion: string | null;
	lastMkGoVersion: string | null;
}): ClientUpdateCheck {
	// **両方の版を最新に揃える。** 判定に使ったほうだけ書くと、backend を
	// 差し替えたときに片側が数世代前のまま残り、戻した瞬間に誤検知する。
	const persist: VersionToPersist[] = [];
	if (args.lastMisskeyVersion !== args.misskeyVersion) {
		persist.push(['lastVersion', args.misskeyVersion]);
	}
	// **空文字は「載っていない」と同じ扱い。** 純正 backend では undefined だが、
	// mkGoVersion を載せていない古い mk-go や、meta を差し替える前段が挟まっている
	// 構成では空文字が来うる。
	// 一度 null に正規化してから見る — `hasMkGo` のような boolean では
	// TypeScript が args.mkGoVersion を絞り込めず、as が要る形になる。
	const mkGoVersion = args.mkGoVersion != null && args.mkGoVersion !== '' ? args.mkGoVersion : null;
	if (mkGoVersion != null && args.lastMkGoVersion !== mkGoVersion) {
		persist.push(['lastMkGoVersion', mkGoVersion]);
	}

	if (mkGoVersion != null) {
		return {
			updated: bumped(mkGoVersion, args.lastMkGoVersion),
			isMkGo: true,
			version: mkGoVersion,
			persist,
		};
	}
	return {
		updated: bumped(args.misskeyVersion, args.lastMisskeyVersion),
		isMkGo: false,
		version: args.misskeyVersion,
		persist,
	};
}

/**
 * Builds the mk-go CHANGELOG URL for the given release version.
 */
// CHANGELOG の見出しは `## 1.3.0` なので GitHub のアンカーは `#130`。外れても
// CHANGELOG の先頭に着地し、最新版が一番上にある。
export function mkGoChangelogUrl(version: string): string {
	return `https://github.com/shiroha-a/mk/blob/main/CHANGELOG.md#${version.replace(/\./g, '')}`;
}
