/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/**
 * Theme variables applied to the entrance page subtree so that it always
 * renders as night-sky glass, regardless of the visitor's theme.
 *
 * @param blur Whether translucent panels may use a backdrop blur.
 */
export function entranceThemeVars(blur: boolean): Record<string, string> {
	// エントランスは利用者のテーマに依らず夜空にする。テーマの変数をこの部分木だけで
	// 上書きするので、登録・ログインのダイアログ (body 直下に出る) は既定のテーマのまま。
	// ぼかしを切っている人には、透けすぎて読みにくくならないよう濃いめの夜色を使う
	return {
		'color-scheme': 'dark',
		'--MI_THEME-bg': '#0a112e',
		'--MI_THEME-fg': '#e4e1f5',
		'--MI_THEME-fgHighlighted': '#ffffff',
		'--MI_THEME-fgOnAccent': '#ffffff',
		'--MI_THEME-panel': blur ? 'rgba(18, 22, 56, 0.55)' : 'rgba(18, 22, 56, 0.92)',
		'--MI_THEME-panelHighlight': 'rgba(204, 195, 247, 0.08)',
		// 長いノートの「閉じる」などは popup の色の上に fg を載せるので、夜色にそろえる
		'--MI_THEME-popup': '#1a1f4a',
		'--MI_THEME-divider': 'rgba(204, 195, 247, 0.14)',
		'--MI_THEME-accent': '#b3a9ff',
		'--MI_THEME-accentedBg': 'rgba(179, 169, 255, 0.15)',
		'--MI_THEME-link': '#c6bdff',
		'--MI_THEME-mention': '#cfc6ff',
		'--MI_THEME-mentionMe': '#cfc6ff',
		'--MI_THEME-hashtag': '#fdddcf',
		'--MI_THEME-buttonBg': 'rgba(204, 195, 247, 0.12)',
		'--MI_THEME-buttonHoverBg': 'rgba(204, 195, 247, 0.2)',
		'--MI_THEME-buttonGradateA': '#5a4fd0',
		'--MI_THEME-buttonGradateB': '#8f83f2',
		'--MI_THEME-infoBg': 'rgba(166, 155, 251, 0.14)',
		'--MI_THEME-infoFg': '#e4e1f5',
		'--MI_THEME-infoWarnBg': 'rgba(253, 221, 207, 0.12)',
		'--MI_THEME-infoWarnFg': '#fdddcf',
		'--MI_THEME-scrollbarHandle': 'rgba(204, 195, 247, 0.25)',
		'--MI_THEME-scrollbarHandleHover': 'rgba(204, 195, 247, 0.45)',
		'--ELYTHIA-panelBackdrop': blur ? 'blur(14px) saturate(1.2)' : 'none',
		'--ELYTHIA-panelBorder': 'solid 1px rgba(204, 195, 247, 0.28)',
		'--ELYTHIA-iconRadius': '22%',
	};
}
