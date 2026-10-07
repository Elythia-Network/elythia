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

/**
 * The state of a signup application (approval-based signup).
 *
 * rejected / expired / completed は終端で、再申請は別の申請になる。
 */
export type SignupApplicationStatus = 'pending' | 'approved' | 'rejected' | 'expired' | 'completed';

/** What the applicant may see about their own application. */
export type SignupApplicationView = {
	status: SignupApplicationStatus;
	createdAt: string;
	expiresAt: string;
};

/**
 * An answer to the application form, stored with the label it was asked under.
 *
 * ラベルを同梱するので、フォームの定義を後から変えても既存の申請が読める (#2570)。
 */
export type SignupApplicationAnswer = {
	label: string;
	value: string;
};

/**
 * A signup application as moderators see it.
 *
 * クレームコードは hash しか持たないので、ここにも出てこない。
 */
export type AdminSignupApplication = {
	id: string;
	status: SignupApplicationStatus;
	answers: SignupApplicationAnswer[];
	createdAt: string;
	updatedAt: string;
	expiresAt: string;
	processedById: string | null;
	processedAt: string | null;
	usedById: string | null;
};

export type MetaDetailed = Misskey.entities.MetaDetailed & ElythiaMetaFields;
