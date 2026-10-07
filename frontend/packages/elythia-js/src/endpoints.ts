/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import type * as Misskey from 'misskey-js';
import type { AdminSignupApplication, SignupApplicationView } from './entities.js';

/**
 * Elythia-specific endpoints that misskey-js does not know about.
 *
 * 型を足したら pending-endpoints.txt から外す。キーと Go のルートは
 * internal/entitycompat の TestElythiaJS_EndpointsMatchRouter が突き合わせるので、
 * キーは `'<path>': {` の形で 1 行に書く。
 */
export type ElythiaEndpoints = {
	'admin/signup-application/approve': {
		req: { applicationId: string };
		res: { ok: true };
	};
	'admin/signup-application/list': {
		req: {
			/** Defaults to `all`. `processed` is rejected, expired and completed. */
			filter?: 'all' | 'pending' | 'approved' | 'processed';
			/** Defaults to 30, capped at 100. */
			limit?: number;
			offset?: number;
		};
		res: {
			applications: AdminSignupApplication[];
			/** The number of applications that match the filter. */
			count: number;
		};
	};
	'admin/signup-application/reject': {
		req: { applicationId: string };
		res: { ok: true };
	};
	'signup-application/apply': {
		req: {
			/** One value per form field, in the order of `meta.signupApplicationForm`. */
			answers: string[];
			/** Required only while no real captcha provider is enabled. */
			formToken?: string;
			'hcaptcha-response'?: string | null;
			'g-recaptcha-response'?: string | null;
			'turnstile-response'?: string | null;
			'm-captcha-response'?: string | null;
			'testcaptcha-response'?: string | null;
		};
		res: {
			/** Shown only here; the server keeps just its hash. */
			claimCode: string;
			application: SignupApplicationView;
		};
	};
	'signup-application/form-token': {
		req: Misskey.entities.EmptyRequest;
		res: {
			/** An empty string when the server does not require a form token. */
			token: string;
			minWaitSeconds: number;
		};
	};
	'signup-application/register': {
		req: {
			claimCode: string;
			username: string;
			password: string;
			/** Required when `meta.emailRequiredForSignup` is set. */
			emailAddress?: string;
		};
		/**
		 * The new account, or `undefined` (204) when a confirmation email was sent
		 * instead.
		 */
		res: Misskey.entities.SignupResponse | undefined;
	};
	'signup-application/status': {
		req: { claimCode: string };
		res: { application: SignupApplicationView };
	};
};
