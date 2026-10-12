/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { describe, expect, test } from 'vitest';
import { rollbackCliCommand } from '@/utility/backup-rollback.js';

/**
 * Elythia: 画面からのロールバックが今のデータベースの資格情報で断られたとき、
 * コマンドでの戻し方を案内する (#3463 (6d))。
 */
describe('rollbackCliCommand', () => {
	const context = { name: 'elythia_before_restore_20261001000000', database: 'elythia' };

	test.each([
		'TWO_FACTOR_REQUIRED',
		'PASSWORD_NOT_SET',
		'ROLE_PERMISSION_DENIED',
		'ACCESS_DENIED',
		'PASSKEY_UNAVAILABLE',
	])('guides to the command line on %s', (code) => {
		expect(rollbackCliCommand(code, context)).toBe('elythia backup restore -rollback elythia_before_restore_20261001000000 -confirm elythia');
	});

	test.each([
		// パスワードの打ち間違いでも出るので、本体を止める手順は勧めない。
		'REAUTHENTICATION_FAILED',
		'TWO_FACTOR_CODE_ALREADY_USED',
		'RATE_LIMIT_EXCEEDED',
		'KEPT_DATABASE_TOO_NEW',
		'BACKUP_SERVICE_BUSY',
		'AUTHENTICATION_UNAVAILABLE',
	])('does not guide on %s', (code) => {
		expect(rollbackCliCommand(code, context)).toBeNull();
	});

	test('does not guide for a restore', () => {
		expect(rollbackCliCommand('TWO_FACTOR_REQUIRED', null)).toBeNull();
		expect(rollbackCliCommand('TWO_FACTOR_REQUIRED', undefined)).toBeNull();
	});

	test('does not guide without a code', () => {
		expect(rollbackCliCommand(undefined, context)).toBeNull();
		expect(rollbackCliCommand(null, context)).toBeNull();
	});
});
