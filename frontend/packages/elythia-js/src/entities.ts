/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import type * as Misskey from 'misskey-js';

/** A field of the signup application form (approval-based signup). */
export type SignupApplicationFormField = {
	label: string;
	type: 'text' | 'textarea';
	required: boolean;
	maxLength?: number;
};

/**
 * Elythia-specific fields that `/api/meta` adds to the upstream response.
 *
 * どれも追加の項目で、古い版のサーバーや、端末に残った古い meta では欠けうる
 * ので、すべて省略可能にしてある。
 */
export type ElythiaMetaFields = {
	/** The running Elythia version (`version` stays the compatible Misskey version). */
	mkGoVersion?: string;
	/** The short commit hash the server was built from, or an empty string. */
	mkGoCommit?: string;
	minimumUsernameLength?: number;
	approvalRequiredForSignup?: boolean;
	registrationClosed?: boolean;
	signupApplicationForm?: SignupApplicationFormField[];
	/** Present only when chunked upload is available. */
	chunkedUpload?: {
		chunkSize: number;
	};
};

export type MetaDetailed = Misskey.entities.MetaDetailed & ElythiaMetaFields;
