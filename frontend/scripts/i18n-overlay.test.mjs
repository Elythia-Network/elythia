/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// @ts-check
import assert from 'node:assert/strict';
import { test } from 'node:test';
// build() と型の生成が使う実装そのものを試す (書き写すと食い違う)
import { overlayLocale } from '../packages/i18n/src/elythia.ts';

test('overlays words section by section and keeps the upstream words it does not touch', () => {
	const upstream = { ok: 'OK', _section: { a: 'A', b: 'B' } };
	const own = { _section: { b: 'B2', c: 'C' }, _own: { x: 'X' }, added: 'Added' };

	assert.deepEqual(overlayLocale(upstream, own), {
		ok: 'OK',
		_section: { a: 'A', b: 'B2', c: 'C' },
		_own: { x: 'X' },
		added: 'Added',
	});
});

test('an empty string removes the upstream word so that the language falls back', () => {
	const upstream = { _followApproval: { title: 'T', description: 'D' }, keep: 'K' };

	assert.deepEqual(overlayLocale(upstream, { _followApproval: { title: '' } }), {
		_followApproval: { description: 'D' },
		keep: 'K',
	});
});

test('keeps the upstream order and puts new words after it', () => {
	const upstream = { a: '1', b: '2', c: '3' };

	assert.deepEqual(Object.keys(overlayLocale(upstream, { z: '0', b: '9' })), ['a', 'b', 'c', 'z']);
});

test('a section replaces an upstream string of the same name, and a string replaces a section', () => {
	assert.deepEqual(overlayLocale({ a: 'A', s: { x: 'X' } }, { a: { b: 'B' }, s: 'S' }), { a: { b: 'B' }, s: 'S' });
});

test('does not modify its arguments', () => {
	const upstream = { s: { a: 'A' }, d: 'D' };
	const own = { s: { a: 'A2' }, d: '' };

	overlayLocale(upstream, own);

	assert.deepEqual(upstream, { s: { a: 'A' }, d: 'D' });
	assert.deepEqual(own, { s: { a: 'A2' }, d: '' });
});

test('an empty or missing file leaves the upstream words as they are', () => {
	const upstream = { a: 'A' };

	assert.deepEqual(overlayLocale(upstream, null), upstream);
	assert.deepEqual(overlayLocale(upstream, undefined), upstream);
	assert.deepEqual(overlayLocale(upstream, {}), upstream);
});
