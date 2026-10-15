/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 未読の点滅 (_blink) が、「アニメーション」の設定と prefers-reduced-motion に従うことを固定する (#3594)。
// 点滅は CSS だけで動いているので、style.scss を実際にコンパイルした結果を見る。

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, test } from 'vitest';
import * as sass from 'sass-embedded';

const srcDir = join(__dirname, '../../src/');
const NO_ANIMATION_CLASS = '_noAnimation_';

function compileStyle(): string {
	return sass.compile(`${srcDir}style.scss`).css;
}

// `selector { body }` を、@media の中かどうかと一緒に抜き出す。
function blinkRules(css: string): { selector: string; body: string; media: string | null }[] {
	const rules: { selector: string; body: string; media: string | null }[] = [];
	const mediaRe = /@media([^{]+)\{((?:[^{}]*\{[^}]*\})*)\s*\}/g;
	for (const m of css.matchAll(mediaRe)) {
		for (const r of m[2].matchAll(/([^{}]+)\{([^}]*)\}/g)) {
			if (r[1].includes('._blink')) rules.push({ selector: r[1].trim(), body: r[2], media: m[1].trim() });
		}
	}
	const withoutMedia = css.replace(mediaRe, '');
	for (const r of withoutMedia.matchAll(/([^{}]+)\{([^}]*)\}/g)) {
		if (r[1].includes('._blink')) rules.push({ selector: r[1].trim(), body: r[2], media: null });
	}
	return rules;
}

describe('未読の点滅 (_blink)', () => {
	const rules = blinkRules(compileStyle());

	test('既定では点滅する (本家の挙動)', () => {
		const base = rules.find(r => r.selector === '._blink' && r.media == null);
		expect(base?.body).toMatch(/animation:\s*blink 1s infinite/);
	});

	test('「アニメーション」を切っているときは止める', () => {
		const off = rules.find(r => r.selector === `html.${NO_ANIMATION_CLASS} ._blink` && r.media == null);
		expect(off?.body).toMatch(/animation:\s*none/);
	});

	test('prefers-reduced-motion: reduce のときは止める', () => {
		const reduced = rules.find(r => r.selector === '._blink' && r.media === '(prefers-reduced-motion: reduce)');
		expect(reduced?.body).toMatch(/animation:\s*none/);
	});

	// CSS が見るクラスを、起動時に設定から付けていること。名前がずれると黙って効かなくなる。
	test('起動時に設定から同じ名前のクラスを付ける', () => {
		const boot = readFileSync(`${srcDir}boot/common.ts`, 'utf8');
		expect(boot).toMatch(new RegExp(`watch\\(prefer\\.r\\.animation,[\\s\\S]{0,200}classList\\.toggle\\('${NO_ANIMATION_CLASS}', !v\\)[\\s\\S]{0,40}immediate: true`));
	});
});
