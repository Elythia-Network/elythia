/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/**
 * Elythia: the guidance to the command line when a rollback from the admin
 * page is refused (#3463 (6d)).
 */

/** The kept database a rollback request is about. */
export type RollbackContext = {
	/** The kept database to swap back in. */
	name: string;
	/** The server's database (`db.db`). */
	database: string;
};

// 再認証は今のデータベースで行う。バックアップから戻した直後は、その後に設定した
// パスワードや二段階認証、管理者の役割、セッションが今のデータベースに無いことがあり、
// 画面からは戻せない。そのときに出るコード。REAUTHENTICATION_FAILED は入れない。
// パスワードの打ち間違いと見分けられず、打ち間違えただけの人に本体を止めるよう勧める
// ことになるため。
const CREDENTIAL_CODES: readonly string[] = [
	'TWO_FACTOR_REQUIRED',
	'PASSWORD_NOT_SET',
	'ROLE_PERMISSION_DENIED',
	'ACCESS_DENIED',
	'PASSKEY_UNAVAILABLE',
];

/**
 * Returns the `elythia backup restore -rollback` command to show when a
 * rollback request was refused with `code` because the current database
 * may lack the administrator's credentials, or null for any other refusal.
 */
export function rollbackCliCommand(code: string | null | undefined, context: RollbackContext | null | undefined): string | null {
	if (context == null || code == null || !CREDENTIAL_CODES.includes(code)) return null;
	return `elythia backup restore -rollback ${context.name} -confirm ${context.database}`;
}
