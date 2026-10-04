/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { i18n } from '@/i18n.js';

/**
 * The additive `signatureCapability` block mk-go attaches to federation
 * instance entities. Upstream Misskey does not emit it, so it is absent from
 * the misskey-js types and has to be received through a local type — the same
 * approach `admin/job-queue.vue` uses for the mk-go-only `runtime` block.
 *
 * 純正 backend では常に undefined になるので、参照側は必ず null 合流させること。
 */
export type SignatureCapability = {
	ed25519: boolean;
	ldSignature: boolean;
	inboundAlgorithm: string | null;
	ed25519DeclaredAt: string | null;
	ed25519AcceptedAt: string | null;
	inboundObservedAt: string | null;
	ldSignatureSeenAt: string | null;
};

/**
 * Reads the signature capability off an instance entity. Returns null when the
 * backend does not emit the field or has not observed the host yet.
 */
export function signatureCapabilityOf(instance: unknown): SignatureCapability | null {
	return (instance as { signatureCapability?: SignatureCapability | null } | null)?.signatureCapability ?? null;
}

export type SignatureLabel = {
	text: string;
	/** 表示の強さ。ed25519 のみ強調し、それ以外は控えめに出す。 */
	kind: 'ed25519' | 'ld' | 'rsa';
};

/**
 * Builds the badge list shown on instance cards.
 *
 * RSA は全サーバーが実装しているので、それ単体では情報量が無い。Ed25519 が
 * 立っているときは RSA バッジを出さず、一覧のノイズを減らす。
 */
export function signatureLabels(cap: SignatureCapability | null): SignatureLabel[] {
	if (cap == null) return [];
	const labels: SignatureLabel[] = [];
	if (cap.ed25519) {
		labels.push({ text: i18n.ts._signatureCapability.ed25519Supported, kind: 'ed25519' });
	} else if (cap.inboundAlgorithm != null) {
		labels.push({ text: i18n.ts._signatureCapability.rsaOnly, kind: 'rsa' });
	}
	if (cap.ldSignature) {
		labels.push({ text: i18n.ts._signatureCapability.ldSignature, kind: 'ld' });
	}
	return labels;
}
