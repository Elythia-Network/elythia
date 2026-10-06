package l10n

import (
	"strconv"
	"strings"
)

// singleLangEmail maps bilingual and unknown locales to English for templates
// that upstream never sent bilingually (reset, signup, verify, footer).
// NOTE: the "en" case is currently equivalent to default; kept explicit so
// bilingual/unknown intentionally falling back to English is visible, and to
// give future en-specific branches a place to land.
func singleLangEmail(lang string) string {
	switch lang {
	case "ja":
		return "ja"
	case "en":
		return "en"
	default:
		return "en"
	}
}

// SignupConfirm returns localized signup confirmation email strings.
func SignupConfirm(lang, siteName string) (subject, lead, linkLabel string) {
	switch singleLangEmail(lang) {
	case "ja":
		return "アカウントの確認",
			siteName + "へようこそ！以下のリンクをクリックして登録を完了してください：",
			"登録を完了"
	default:
		return "Confirm your account",
			"Welcome to " + siteName + "! Click the link to complete your signup:",
			"Complete signup"
	}
}

// PasswordReset returns localized password reset email strings.
func PasswordReset(lang string) (subject, lead, linkLabel string) {
	switch singleLangEmail(lang) {
	case "ja":
		return "パスワードのリセット", "以下のリンクからパスワードをリセットしてください：", "パスワードをリセット"
	default:
		return "Password reset", "Use the following link to reset your password:", "Reset password"
	}
}

// VerifyEmail returns localized email-address verification strings.
func VerifyEmail(lang string) (subject, lead, linkLabel string) {
	switch singleLangEmail(lang) {
	case "ja":
		return "メールアドレスの確認", "以下のリンクをクリックしてメールアドレスを確認してください：", "メールアドレスを確認"
	default:
		return "Verify your email", "Click the link to verify your email:", "Verify email"
	}
}

// NewLogin returns localized new-login notification strings.
func NewLogin(lang string) (subject, body string) {
	switch lang {
	case "ja":
		return "ログインがありました",
			"新しいログインがありました。このログインに心当たりがない場合は、パスワードを変更するなど、アカウントのセキュリティ状態を更新してください。"
	case LangBilingual:
		return "New login / ログインがありました",
			"There is a new login. If you do not recognize this login, update the security status of your account, including changing your password. / 新しいログインがありました。このログインに心当たりがない場合は、パスワードを変更するなど、アカウントのセキュリティ状態を更新してください。"
	default:
		return "New login",
			"There is a new login. If you do not recognize this login, update the security status of your account, including changing your password."
	}
}

// EmailSettingsLabel is the footer link label in HTML transactional emails.
func EmailSettingsLabel(lang string) string {
	switch singleLangEmail(lang) {
	case "ja":
		return "メール設定"
	default:
		return "Email setting"
	}
}

// ModeratorInactivityWarning returns subject and plain-text body for the
// moderator inactivity warning email.
func ModeratorInactivityWarning(lang string, remainingDays, remainingHours int) (subject, body string) {
	switch lang {
	case LangBilingual:
		timeEn := formatRemaining("en", remainingDays, remainingHours)
		timeJa := formatRemaining("ja", remainingDays, remainingHours)
		subject = "Moderator Inactivity Warning / モデレーター不在の通知"
		body = strings.Join([]string{
			"To moderators,",
			"",
			"A moderator has been inactive for a period of time. If there are " + timeEn + " of inactivity left, it will switch to invitation only.",
			"If you do not want it to switch to invitation only, log in to Elythia to update your last active date.",
			"",
			"モデレーター各位",
			"",
			"モデレーターが一定期間活動していないようです。あと" + timeJa + "活動していない状態が続くと招待制に切り替わります。",
			"招待制に切り替わることを望まない場合は、Elythiaにログインして最終アクティブ日時を更新してください。",
		}, "\n")
		return subject, body
	case "ja":
		timeVariant := formatRemaining(lang, remainingDays, remainingHours)
		subject = "モデレーター不在の通知"
		body = strings.Join([]string{
			"モデレーター各位",
			"",
			"モデレーターが一定期間活動していないようです。あと" + timeVariant + "活動していない状態が続くと招待制に切り替わります。",
			"招待制に切り替わることを望まない場合は、Elythiaにログインして最終アクティブ日時を更新してください。",
		}, "\n")
	default:
		timeVariant := formatRemaining(lang, remainingDays, remainingHours)
		subject = "Moderator Inactivity Warning"
		body = strings.Join([]string{
			"To moderators,",
			"",
			"A moderator has been inactive for a period of time. If there are " + timeVariant + " of inactivity left, it will switch to invitation only.",
			"If you do not want it to switch to invitation only, log in to Elythia to update your last active date.",
		}, "\n")
	}
	return subject, body
}

// ModeratorInvitationOnlyChanged returns subject and plain-text body for the
// invitation-only switch notification.
func ModeratorInvitationOnlyChanged(lang string, inactivityLimitDays int) (subject, body string) {
	days := strconv.Itoa(inactivityLimitDays)
	switch lang {
	case LangBilingual:
		subject = "Change to Invitation-Only / 招待制に変更されました"
		body = strings.Join([]string{
			"To moderators,",
			"",
			"Changed to invitation only because no moderator activity was detected for " + days + " days.",
			"To turn off invitation only, you need to access the control panel.",
			"",
			"モデレーター各位",
			"",
			"モデレーターの活動が" + days + "日間検出されなかったため、招待制に変更されました。",
			"招待制を解除するには、コントロールパネルにアクセスする必要があります。",
		}, "\n")
		return subject, body
	case "ja":
		subject = "招待制に変更されました"
		body = strings.Join([]string{
			"モデレーター各位",
			"",
			"モデレーターの活動が" + days + "日間検出されなかったため、招待制に変更されました。",
			"招待制を解除するには、コントロールパネルにアクセスする必要があります。",
		}, "\n")
	default:
		subject = "Change to Invitation-Only"
		body = strings.Join([]string{
			"To moderators,",
			"",
			"Changed to invitation only because no moderator activity was detected for " + days + " days.",
			"To turn off invitation only, you need to access the control panel.",
		}, "\n")
	}
	return subject, body
}

func formatRemaining(lang string, days, hours int) string {
	if days == 0 {
		if lang == "ja" {
			return strconv.Itoa(hours) + "時間"
		}
		return strconv.Itoa(hours) + " hours"
	}
	if lang == "ja" {
		return strconv.Itoa(days) + "日間"
	}
	return strconv.Itoa(days) + " days"
}
