/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// @ts-check
/**
 * Fails when a string in the ja-JP locale is missing from the en-US locale (#4047).
 *
 * Usage (from frontend/):
 *   node scripts/check-locale-coverage.mjs
 *   node scripts/check-locale-coverage.mjs --write-allowlist <upstream locales dir>
 *
 * Exit codes:
 *   0 = every ja-JP string is in en-US, or is listed in the allowlist
 *   1 = some ja-JP strings are missing from en-US, or the allowlist has entries that are not used
 *   2 = the check could not be trusted (a parser, a malformed allowlist, the lower bound, or the
 *       arguments failed, or --write-allowlist was given Elythia's own locales/)
 *
 * ロケールは ja-JP を土台に重ねる (packages/i18n/src/index.ts) ので、en-US に無い語は
 * 英語の UI でも日本語のまま出る。Elythia の語を接頭辞 (`_mkgo`) で見分けると、付け忘れた
 * 語が素通りするので、ja-JP の語はすべて en-US にあることを求める。
 *
 * 例外は許可リスト (locale-coverage-allowlist.tsv) に書く。入るのは本家の語で、本家の
 * en-US にもまだ無いもの (Crowdin の翻訳待ち) だけ。本家に追従して増えたら
 * `--write-allowlist` に本家の locales/ を渡して書き直す。CI には本家のソースが無いので、
 * 本家の語かどうかは書き直すときにだけ判定する。
 */

import { readFileSync, realpathSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const frontendRoot = fileURLToPath(new URL('..', import.meta.url));
// js-yaml はロケールを読む i18n パッケージの依存として入っている (ルートには無い)
const require = createRequire(join(frontendRoot, 'packages', 'i18n', 'package.json'));
/** @type {typeof import('js-yaml')} */
const yaml = require('js-yaml');

export const UPSTREAM_UNTRANSLATED_REASON = '本家の en-US に無い (Crowdin の翻訳待ち)';

/**
 * Lower bounds, so that a change in the locale format or the parser does not turn the
 * check into one that reads nothing and passes.
 *
 * 目印には、本家の語・Elythia の節の語・`_mkgo` で始まらない Elythia の節の語・最上位の
 * Elythia の語を 1 つずつ置く。
 *
 * Elythia の目印 (ELYTHIA_MARKERS) と `_mkgo` で始まる語は、許可リストに載せられない。
 */
export const ELYTHIA_MARKERS = [
	'_mkgoNotification.abuseReport',
	'_federationRules.tab',
	'minimumUsernameLength',
];
export const REQUIRED_KEYS = ['_notification.youGotMention', ...ELYTHIA_MARKERS];
export const MIN_KEYS = 3000;

/**
 * @typedef {{ [key: string]: string | LocaleRecord }} LocaleRecord
 */

/**
 * Lists the dotted paths of the strings in the locale, in the order they are written.
 *
 * @param {LocaleRecord} locale
 * @param {string} [prefix]
 * @returns {string[]}
 */
export function leafKeys(locale, prefix = '') {
	/** @type {string[]} */
	const keys = [];
	for (const [key, value] of Object.entries(locale ?? {})) {
		const path = prefix === '' ? key : `${prefix}.${key}`;
		if (value !== null && typeof value === 'object') {
			keys.push(...leafKeys(value, path));
		} else {
			keys.push(path);
		}
	}
	return keys;
}

/**
 * Reports whether the locale has a non-empty string at the dotted path.
 *
 * 空文字は index.ts の removeEmpty が消して ja-JP に戻るので、無いものとして扱う。
 *
 * @param {LocaleRecord} locale
 * @param {string} path
 * @returns {boolean}
 */
export function hasString(locale, path) {
	/** @type {unknown} */
	let node = locale;
	for (const key of path.split('.')) {
		if (node === null || typeof node !== 'object' || !Object.hasOwn(node, key)) return false;
		node = /** @type {Record<string, unknown>} */ (node)[key];
	}
	return typeof node === 'string' && node !== '';
}

/**
 * Parses the allowlist: one `<dotted key>\t<reason>` per line, `#` comments and blank lines ignored.
 *
 * @param {string} text
 * @returns {{ entries: Map<string, string>, malformed: string[] }}
 */
export function parseAllowlist(text) {
	/** @type {Map<string, string>} */
	const entries = new Map();
	/** @type {string[]} */
	const malformed = [];
	text.split('\n').forEach((line, i) => {
		if (line.trim() === '' || line.startsWith('#')) return;
		const [key, reason, ...rest] = line.split('\t');
		if (key === '' || reason == null || reason.trim() === '' || rest.length > 0) {
			malformed.push(`line ${i + 1}: expected '<key>\\t<reason>'`);
		} else if (key.startsWith('_mkgo') || ELYTHIA_MARKERS.includes(key)) {
			// 許可リストは本家の語だけ。手で足した Elythia の語は、CI では本家の語と
			// 見分けられないので、少なくとも見分けられる形のものは受け付けない
			malformed.push(`line ${i + 1}: ${key} is an Elythia string; translate it in en-US instead`);
		} else if (entries.has(key)) {
			malformed.push(`line ${i + 1}: ${key} is listed twice`);
		} else {
			entries.set(key, reason);
		}
	});
	return { entries, malformed };
}

/**
 * Compares the locales against the allowlist.
 *
 * @param {LocaleRecord} ja
 * @param {LocaleRecord} en
 * @param {Map<string, string>} allowlist
 */
export function check(ja, en, allowlist) {
	const jaKeys = leafKeys(ja);
	const missingKeys = jaKeys.filter(key => !hasString(en, key));
	const missing = new Set(missingKeys);
	return {
		jaKeys,
		missingKeys,
		// 許可リストに無いのに en-US に無い語
		violations: missingKeys.filter(key => !allowlist.has(key)),
		// 英訳が入ったか、ja-JP から消えた語。残すと、また抜けても気付けない
		unused: [...allowlist.keys()].filter(key => !missing.has(key)),
	};
}

/**
 * Returns why the run cannot be trusted, or an empty array.
 *
 * @param {ReturnType<typeof check>} result
 * @returns {string[]}
 */
export function trustProblems(result) {
	/** @type {string[]} */
	const problems = [];
	if (result.jaKeys.length < MIN_KEYS) {
		problems.push(`only ${result.jaKeys.length} strings were read from ja-JP (expected at least ${MIN_KEYS})`);
	}
	const read = new Set(result.jaKeys);
	for (const key of REQUIRED_KEYS) {
		if (!read.has(key)) problems.push(`${key} was not read from ja-JP`);
	}
	return problems;
}

/**
 * Builds the allowlist from the upstream locales: a missing string goes in when upstream
 * has it in ja-JP but not in en-US. Any other missing string is returned as an error.
 *
 * @param {string[]} missingKeys
 * @param {LocaleRecord} upstreamJa
 * @param {LocaleRecord} upstreamEn
 * @param {Map<string, string>} previous the current allowlist, whose reasons are kept
 * @returns {{ text: string, errors: string[] }}
 */
export function buildAllowlist(missingKeys, upstreamJa, upstreamEn, previous) {
	/** @type {string[]} */
	const lines = [];
	/** @type {string[]} */
	const errors = [];
	for (const key of missingKeys) {
		if (!hasString(upstreamJa, key)) {
			errors.push(`${key}: not in the upstream ja-JP, so it is an Elythia string. Add its translation to en-US.`);
		} else if (hasString(upstreamEn, key)) {
			errors.push(`${key}: the upstream en-US has it. Copy it from the upstream en-US.`);
		} else {
			lines.push(`${key}\t${previous.get(key) ?? UPSTREAM_UNTRANSLATED_REASON}`);
		}
	}
	const header = [
		'# ja-JP にあって en-US に無くてよい語 (#4047)。<キー>\\t<理由> の形で書く。',
		'# 本家の語で、本家の en-US にもまだ無いもの (Crowdin の翻訳待ち) だけを入れる。',
		'# 書き直すときは frontend/ で次を流す (手で足さない):',
		'#   node scripts/check-locale-coverage.mjs --write-allowlist <本家の locales/>',
	];
	return { text: [...header, ...lines].join('\n') + '\n', errors };
}

/**
 * @param {string} path
 * @returns {LocaleRecord}
 */
function loadLocale(path) {
	const locale = yaml.load(readFileSync(path, 'utf8'));
	if (locale === null || typeof locale !== 'object') throw new Error(`${path} is not a mapping`);
	return /** @type {LocaleRecord} */ (locale);
}

/**
 * Runs the check (or rewrites the allowlist) and returns the exit code.
 *
 * @param {string} root the frontend/ directory
 * @param {string[]} args
 * @param {{ log: (line: string) => void, error: (line: string) => void }} out
 * @param {string} [allowlistPath]
 * @returns {number}
 */
export function run(root, args, out, allowlistPath = join(root, 'scripts', 'locale-coverage-allowlist.tsv')) {
	const ja = loadLocale(join(root, 'locales', 'ja-JP.yml'));
	const en = loadLocale(join(root, 'locales', 'en-US.yml'));
	const { entries, malformed } = parseAllowlist(readFileSync(allowlistPath, 'utf8'));
	for (const problem of malformed) out.error(`check-locale-coverage: ${allowlistPath}: ${problem}`);
	if (malformed.length > 0) return 2;

	const result = check(ja, en, entries);
	const problems = trustProblems(result);
	for (const problem of problems) out.error(`check-locale-coverage: ${problem}`);
	if (problems.length > 0) return 2;

	if (args[0] === '--write-allowlist') {
		if (args.length !== 2) {
			out.error('check-locale-coverage: --write-allowlist takes the upstream locales directory');
			return 2;
		}
		const upstreamJa = loadLocale(join(args[1], 'ja-JP.yml'));
		// Elythia 自身の locales/ を渡すと、抜けた Elythia の語がすべて許可リストへ逃げる。
		// 見分けは `_mkgo` の節で行う (目印の語だと、本家が同じ名前の語を足したときに
		// 正しい本家の locales/ まで拒むことになる)
		const own = Object.keys(upstreamJa).filter(key => key.startsWith('_mkgo'));
		if (own.length > 0) {
			out.error(`check-locale-coverage: ${args[1]} has Elythia sections (${own.join(', ')}). Pass the upstream locales/ (.cache/misskey/<version>/locales), not Elythia's.`);
			return 2;
		}
		const built = buildAllowlist(result.missingKeys, upstreamJa, loadLocale(join(args[1], 'en-US.yml')), entries);
		// 1 つでも Elythia の語が混じったら書かない。許可リストへ逃がさず、英訳を足させる
		for (const error of built.errors) out.error(`check-locale-coverage: ${error}`);
		if (built.errors.length > 0) return 1;
		writeFileSync(allowlistPath, built.text);
		out.log(`check-locale-coverage: wrote ${result.missingKeys.length} entries to ${allowlistPath}`);
		return 0;
	}
	if (args.length > 0) {
		out.error(`check-locale-coverage: unknown arguments: ${args.join(' ')}`);
		return 2;
	}

	for (const key of result.violations) {
		out.error(`locales/en-US.yml: ${key} is missing, so the English UI shows the Japanese text. Add its translation.`);
	}
	for (const key of result.unused) {
		out.error(`${allowlistPath}: ${key} is listed but en-US has it (or ja-JP no longer does). Remove the line.`);
	}
	if (result.violations.length > 0 || result.unused.length > 0) return 1;
	out.log(`check-locale-coverage: ${result.jaKeys.length} ja-JP strings, all in en-US except ${entries.size} allowlisted`);
	return 0;
}

// symlink を通して起動しても main を走らせる (import.meta.url は実体のパスになる)
if (process.argv[1] != null && import.meta.url === pathToFileURL(realpathSync(process.argv[1])).href) {
	let code;
	try {
		code = run(frontendRoot, process.argv.slice(2), { log: console.log, error: console.error });
	} catch (error) {
		console.error(`check-locale-coverage: ${error instanceof Error ? error.message : String(error)}`);
		code = 2;
	}
	process.exit(code);
}
