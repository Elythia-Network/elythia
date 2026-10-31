/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test } from 'vitest';
import { chunkedStartRequest } from '@/utility/drive-chunked-start.js';

describe('chunkedStartRequest', () => {
	const file = new File(['abc'], 'a.png', { type: 'image/png' });

	test('空文字の folderId は送らず null にする (単発のアップロードと同じ)', () => {
		expect(chunkedStartRequest(file, { folderId: '' }).folderId).toBeNull();
	});

	test('folderId が無ければ null', () => {
		expect(chunkedStartRequest(file, {}).folderId).toBeNull();
		expect(chunkedStartRequest(file, { folderId: null }).folderId).toBeNull();
	});

	test('folderId があればそのまま送る', () => {
		expect(chunkedStartRequest(file, { folderId: 'abc123' }).folderId).toBe('abc123');
	});

	test('名前・大きさ・説明の既定値', () => {
		expect(chunkedStartRequest(file, {})).toEqual({
			name: 'a.png', size: 3, force: true, isSensitive: false, comment: null, folderId: null,
		});
		expect(chunkedStartRequest(new Blob(['x']), { name: 'n', isSensitive: true, caption: 'c' })).toEqual({
			name: 'n', size: 1, force: true, isSensitive: true, comment: 'c', folderId: null,
		});
	});
});
