/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// @ts-check
/**
 * Fails when a file written by Elythia imports misskey-js directly (#4028).
 *
 * Usage (from frontend/):
 *   node scripts/check-elythia-imports.mjs
 *
 * Exit codes:
 *   0 = no Elythia file imports misskey-js
 *   1 = some Elythia file imports misskey-js
 *   2 = the check could not be trusted (git, a parser, a malformed header, or the lower bound failed)
 *
 * Elythia の書いたコードは elythia-js だけを import する。elythia-js が misskey-js の
 * 中身をすべて再公開しているので、misskey-js を直接読む理由が無い。本家から来た
 * ファイルは対象にしない (書き換えると本家への追従で衝突する)。
 *
 * Elythia のファイルは、最初のコードより前のコメントにある SPDX の著作権の行が `Elythia-Network`
 * だけのもの (CLAUDE.md Section 5 の、新しいファイルの見出しの規則)。本家の行が
 * 残っているファイルは本家から来たものとして扱う。
 *
 * vi.mock('misskey-js') と vi.importActual('misskey-js') は対象にしない。本家のコードが
 * import する先を差し替えるためのもので、Elythia のコードが misskey-js を使うわけではない。
 */

import { lstatSync, readFileSync, realpathSync } from 'node:fs';
import { createRequire } from 'node:module';
import { extname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

import { gitPaths } from './lib/git.mjs';

const frontendRoot = fileURLToPath(new URL('..', import.meta.url));
// typescript と vue は packages/frontend の依存として入っている (ルートには vue が無い)
const require = createRequire(join(frontendRoot, 'packages', 'frontend', 'package.json'));
/** @type {typeof import('typescript')} */
const ts = require('typescript');
/** @type {typeof import('vue/compiler-sfc')} */
const sfc = require('vue/compiler-sfc');

/** The extensions whose imports are checked. */
export const EXTENSIONS = ['.ts', '.tsx', '.mts', '.cts', '.js', '.jsx', '.mjs', '.cjs', '.vue'];

/** Paths (relative to frontend/) excluded from the check, as prefixes. */
export const EXCLUDED_PREFIXES = [
	// misskey-js の上に重ねて再公開するパッケージそのもの
	'packages/elythia-js/',
];

/**
 * Lower bounds, so that a change in the header format or the parser does not turn
 * the check into one that finds nothing and passes.
 *
 * 目印には elythia-js / misskey-js 以外の import を選ぶ。どちらかにすると、目印の
 * ファイル自身が misskey-js を import したときに、違反ではなく下限の失敗として報告される。
 * test/ のファイルも 1 つ入れる (eslint ではなくこの検査にした理由が test/ を見ることなので)。
 */
export const REQUIRED_ELYTHIA_FILES = {
	'packages/frontend/src/utility/abuse-report.ts': '@/i18n.js',
	'packages/frontend/src/pages/drop-and-fusion.versus.history.vue': '@/os.js',
	'packages/frontend/test/unit/setup-network.test.ts': '@/directives/index.js',
};
export const MIN_ELYTHIA_FILES = 100;

const ELYTHIA_HOLDER = 'Elythia-Network';
// 最初のコードより前のコメント (`// @ts-check` などを挟んでもよい) だけを見出しとして読む。
// 本文の fixture などに SPDX の行があっても、見出しとは扱わない。.vue は先頭に並ぶ `<!-- -->`
const VUE_LEADING_COMMENTS = /^(?:\s*<!--[\s\S]*?-->)+/;
// 改行をまたがないよう、空白は [ \t] で受ける (値が空の行で、次の行を著作権者として読まない)
const COPYRIGHT_LINE = /^[\s*/<!-]*SPDX-FileCopyrightText:[ \t]*(.+?)[ \t]*(?:-->|\*\/)?[ \t]*$/gm;
// 本家のファイルの本文にある SPDX の行 (fixture など) では落とさないよう、Elythia の行に限る
const ELYTHIA_COPYRIGHT_LINE = /^[\s*/<!-]*SPDX-FileCopyrightText:.*elythia/im;

/**
 * Lists the SPDX copyright holders written in the comments before the first code of the file.
 *
 * @param {string} text
 * @param {string} [path] decides how the comments are read (.vue or a script)
 * @returns {string[]}
 */
export function copyrightHolders(text, path = 'x.ts') {
	// BOM は TypeScript も正規表現の \s も空白として読み飛ばす。shebang は TypeScript が読み飛ばす
	const header = extname(path) === '.vue'
		? VUE_LEADING_COMMENTS.exec(text)?.[0] ?? ''
		: (ts.getLeadingCommentRanges(text, 0) ?? []).map(r => text.slice(r.pos, r.end)).join('\n');
	return [...header.matchAll(COPYRIGHT_LINE)].map(m => m[1]);
}

/**
 * Lists the module specifiers that the source imports or re-exports.
 *
 * import / export ... from / import() / require() / import('x').T (型の位置、JSDoc の中を含む)、
 * JSDoc の `@import`、.vue の `<script setup generic="...">` を拾う。JSDoc は、構文木の
 * ノードに付いたものだけを読む (どのノードにも付かないコメントの中は読まない)。
 * vi.importActual<typeof import('misskey-js')>() の型引数は、型の位置の import として拾う。
 *
 * @param {string} path
 * @param {string} text
 * @returns {string[]}
 */
export function importSpecifiers(path, text) {
	if (extname(path) === '.vue') {
		const { descriptor, errors } = sfc.parse(text, { filename: path });
		if (errors.length > 0) throw new Error(`${path}: ${errors[0]}`);
		const found = [descriptor.script, descriptor.scriptSetup]
			.filter(block => block != null)
			.flatMap(block => scriptSpecifiers(`${path}.${block.lang ?? 'js'}`, block.content));
		const generic = descriptor.scriptSetup?.attrs.generic;
		if (typeof generic === 'string') {
			found.push(...scriptSpecifiers(`${path}.generic.ts`, `function __generic<${generic}>() {}`));
		}
		return found;
	}
	return scriptSpecifiers(path, text);
}

/**
 * @param {string} path
 * @param {string} text
 * @returns {string[]}
 */
function scriptSpecifiers(path, text) {
	// scriptKind を省くと、TypeScript がファイル名の拡張子から決める
	const source = ts.createSourceFile(path, text, ts.ScriptTarget.Latest, true);
	/** @type {string[]} */
	const found = [];
	/** @param {import('typescript').Node | undefined} node */
	const literal = (node) => {
		if (node != null && ts.isStringLiteralLike(node)) found.push(node.text);
	};
	/** @param {import('typescript').Node} node */
	const visit = (node) => {
		if (ts.isImportDeclaration(node) || ts.isExportDeclaration(node) || ts.isJSDocImportTag(node)) {
			literal(node.moduleSpecifier);
		} else if (ts.isImportEqualsDeclaration(node) && ts.isExternalModuleReference(node.moduleReference)) {
			literal(node.moduleReference.expression);
		} else if (ts.isCallExpression(node)) {
			const callee = node.expression;
			if (callee.kind === ts.SyntaxKind.ImportKeyword || (ts.isIdentifier(callee) && callee.text === 'require')) {
				literal(node.arguments[0]);
			}
		} else if (ts.isImportTypeNode(node) && ts.isLiteralTypeNode(node.argument)) {
			literal(node.argument.literal);
		}
		// forEachChild は JSDoc を辿らない。`@type {import('x').T}` もここで拾う
		const jsDoc = /** @type {{ jsDoc?: import('typescript').JSDoc[] }} */ (node).jsDoc;
		for (const doc of jsDoc ?? []) visit(doc);
		ts.forEachChild(node, visit);
	};
	visit(source);
	return found;
}

/**
 * @param {string} specifier
 * @returns {boolean}
 */
export function isMisskeyJs(specifier) {
	return specifier === 'misskey-js' || specifier.startsWith('misskey-js/');
}

/**
 * Checks the given files and returns the violations and the facts the lower bound needs.
 *
 * @param {{ path: string, text: string }[]} files paths relative to frontend/
 * @returns {{ violations: { path: string, specifier: string }[], malformed: { path: string, holder: string }[], elythiaFiles: number, specifiersOf: Map<string, string[]> }}
 */
export function check(files) {
	/** @type {{ path: string, specifier: string }[]} */
	const violations = [];
	/** @type {{ path: string, holder: string }[]} */
	const malformed = [];
	/** @type {Map<string, string[]>} */
	const specifiersOf = new Map();
	let elythiaFiles = 0;
	for (const { path, text } of files) {
		if (!EXTENSIONS.includes(extname(path))) continue;
		if (EXCLUDED_PREFIXES.some(prefix => path.startsWith(prefix))) continue;
		const holders = copyrightHolders(text, path);
		// 表記が揺れた Elythia の行は、黙って本家のファイルとして扱わずに落とす
		for (const holder of holders) {
			if (holder !== ELYTHIA_HOLDER && /elythia/i.test(holder)) malformed.push({ path, holder });
		}
		// 見出しの前にコードがある (`'use strict'` や、.vue で `<template>` が先) と、
		// 黙って対象から外れる。Elythia の著作権の行が見出しの外にしか無いファイルも落とす
		if (holders.length === 0 && ELYTHIA_COPYRIGHT_LINE.test(text)) {
			malformed.push({ path, holder: '(the SPDX header comes after code)' });
		}
		if (holders.length === 0 || !holders.every(h => h === ELYTHIA_HOLDER)) continue;
		elythiaFiles++;
		const specifiers = importSpecifiers(path, text);
		specifiersOf.set(path, specifiers);
		for (const specifier of specifiers) {
			if (isMisskeyJs(specifier)) violations.push({ path, specifier });
		}
	}
	return { violations, malformed, elythiaFiles, specifiersOf };
}

/**
 * Returns why the run cannot be trusted, or an empty array.
 *
 * @param {ReturnType<typeof check>} result
 * @returns {string[]}
 */
export function trustProblems(result) {
	/** @type {string[]} */
	const problems = result.malformed.map(({ path, holder }) => holder.startsWith('(')
		? `${path}: ${holder.slice(1, -1)}; move it to the top of the file`
		: `${path}: the copyright holder '${holder}' should be exactly '${ELYTHIA_HOLDER}'`);
	if (result.elythiaFiles < MIN_ELYTHIA_FILES) {
		problems.push(`only ${result.elythiaFiles} Elythia files were found (expected at least ${MIN_ELYTHIA_FILES})`);
	}
	for (const [path, specifier] of Object.entries(REQUIRED_ELYTHIA_FILES)) {
		const specifiers = result.specifiersOf.get(path);
		if (specifiers == null) {
			problems.push(`${path} was not recognized as an Elythia file`);
		} else if (!specifiers.includes(specifier)) {
			problems.push(`${path} should import ${specifier}, but the imports found were: ${specifiers.join(', ') || '(none)'}`);
		}
	}
	return problems;
}

/**
 * Runs the check on the files git tracks under root and returns the exit code.
 *
 * @param {string} root the frontend/ directory
 * @param {{ log: (line: string) => void, error: (line: string) => void }} out
 * @returns {number}
 */
export function run(root, out) {
	const paths = gitPaths(['-C', root, 'ls-files', '-z']).filter((path) => {
		// 作業ツリーで消したファイルと symlink は読まない (check-spdx.mjs と同じ)
		try {
			return lstatSync(join(root, path)).isFile();
		} catch {
			return false;
		}
	});
	const result = check(paths.map(path => ({ path, text: readFileSync(join(root, path), 'utf8') })));
	for (const { path, specifier } of result.violations) {
		out.error(`${path}: imports '${specifier}'. Import from 'elythia-js' instead (it re-exports misskey-js).`);
	}
	const problems = trustProblems(result);
	for (const problem of problems) out.error(`check-elythia-imports: ${problem}`);
	if (problems.length > 0) return 2;
	if (result.violations.length > 0) return 1;
	out.log(`check-elythia-imports: ${result.elythiaFiles} Elythia files, none imports misskey-js`);
	return 0;
}

// symlink を通して起動しても main を走らせる (import.meta.url は実体のパスになる)
if (process.argv[1] != null && import.meta.url === pathToFileURL(realpathSync(process.argv[1])).href) {
	let code;
	try {
		code = run(frontendRoot, { log: console.log, error: console.error });
	} catch (error) {
		console.error(`check-elythia-imports: ${error instanceof Error ? error.message : String(error)}`);
		code = 2;
	}
	process.exit(code);
}
