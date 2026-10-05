package signupapplication

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/elythia-network/elythia/internal/misc/colfit"
)

// Form limits (#2570).
//
// **上限を置かないと管理者が自分で壊せる。** 項目を無制限に足せば申請ページが
// 使い物にならなくなり、最大長を無制限にすれば 1 件の申請で DB を膨らませられる。
const (
	// MaxFormFields bounds how many questions a form may have.
	MaxFormFields = 10
	// MaxFormLabelLength bounds a single question's label.
	MaxFormLabelLength = 100
	// MaxAnswerLength is the hard ceiling for one answer, regardless of what
	// the field declares.
	MaxAnswerLength = 2000
	// DefaultAnswerLength applies when a field omits maxLength.
	DefaultAnswerLength = 500
)

// Field types.
const (
	FieldTypeText     = "text"
	FieldTypeTextarea = "textarea"
)

var (
	// ErrInvalidForm is returned when a form definition is unusable.
	ErrInvalidForm = errors.New("signupapplication: invalid application form")
	// ErrAnswerRequired is returned when a required field is left blank.
	ErrAnswerRequired = errors.New("signupapplication: a required answer is missing")
	// ErrAnswerTooLong is returned when an answer exceeds its field's limit.
	ErrAnswerTooLong = errors.New("signupapplication: an answer is too long")
	// ErrAnswerCountMismatch is returned when the submitted answers do not line
	// up with the current form.
	//
	// **申請の途中で管理者がフォームを変えると起きる。** 黙って詰め合わせると
	// 答えと設問がずれるので、やり直してもらう。
	ErrAnswerCountMismatch = errors.New("signupapplication: answers do not match the current form")
)

// FormField is one question on the application form.
type FormField struct {
	Label     string `json:"label"`
	Type      string `json:"type"`
	Required  bool   `json:"required"`
	MaxLength int    `json:"maxLength,omitempty"`
}

// Answer is one submitted answer, carrying the label it was asked under.
//
// **ラベルを一緒に持つのが要点。** 定義を後から変えても、既存申請がどの設問への
// 答えだったか分かる。
type Answer struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// ParseForm reads a stored form definition. 壊れた JSON は空フォーム扱いにする
// (申請ページを 500 で潰さない)。
func ParseForm(raw []byte) []FormField {
	if len(raw) == 0 {
		return nil
	}
	var fields []FormField
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	return fields
}

// ValidateForm checks a definition an admin is trying to save.
func ValidateForm(fields []FormField) error {
	if len(fields) > MaxFormFields {
		return fmt.Errorf("%w: at most %d fields", ErrInvalidForm, MaxFormFields)
	}
	for i, f := range fields {
		label := strings.TrimSpace(f.Label)
		if label == "" {
			return fmt.Errorf("%w: field %d has no label", ErrInvalidForm, i+1)
		}
		if len([]rune(label)) > MaxFormLabelLength {
			return fmt.Errorf("%w: field %d label is too long", ErrInvalidForm, i+1)
		}
		if f.Type != FieldTypeText && f.Type != FieldTypeTextarea {
			return fmt.Errorf("%w: field %d has an unknown type %q", ErrInvalidForm, i+1, f.Type)
		}
		if f.MaxLength < 0 || f.MaxLength > MaxAnswerLength {
			return fmt.Errorf("%w: field %d maxLength must be between 0 and %d", ErrInvalidForm, i+1, MaxAnswerLength)
		}
	}
	return nil
}

// BuildAnswers pairs submitted values with the current form definition.
//
// **ラベルは定義から埋める。** クライアントに送らせると、申請者が審査画面に偽の
// ラベルを流し込める。値の順序が定義と一致していることを要求し、ずれていたら
// 詰め合わせずにやり直してもらう。
func BuildAnswers(fields []FormField, values []string) ([]Answer, error) {
	if len(values) != len(fields) {
		return nil, ErrAnswerCountMismatch
	}
	out := make([]Answer, 0, len(fields))
	for i, f := range fields {
		v := strings.TrimSpace(values[i])
		if f.Required && v == "" {
			return nil, fmt.Errorf("%w: %s", ErrAnswerRequired, f.Label)
		}
		limit := f.MaxLength
		if limit <= 0 {
			limit = DefaultAnswerLength
		}
		if limit > MaxAnswerLength {
			limit = MaxAnswerLength
		}
		// rune 単位で数える。**byte で見ると日本語が通らなくなる。**
		//
		// **NUL と不正な UTF-8 もここで落とす (#3037)。** 回答は jsonb 列へ
		// そのまま入るので、PostgreSQL は NUL エスケープを SQLSTATE 22P05 で
		// 拒否してクエリごと落とす。**この経路は未認証で叩ける**ので、
		// 誰でも 500 とエラーログを任意に生成できていた。#3022 と同じく既存の
		// 述語に畳んで既存の 400 に落とす。
		if !colfit.Fits(v, limit) {
			return nil, fmt.Errorf("%w: %s", ErrAnswerTooLong, f.Label)
		}
		out = append(out, Answer{Label: strings.TrimSpace(f.Label), Value: v})
	}
	return out, nil
}
