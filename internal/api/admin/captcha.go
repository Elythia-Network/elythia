package admin

import (
	"errors"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/captcha"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/labstack/echo/v4"
)

// supportedCaptchaProviders mirrors upstream CaptchaService.supportedCaptchaProviders.
var supportedCaptchaProviders = map[string]struct{}{
	"none": {}, "hcaptcha": {}, "mcaptcha": {}, "recaptcha": {}, "turnstile": {}, "testcaptcha": {},
}

// CaptchaCurrent handles POST /api/admin/captcha/current.
//
// Returns the currently enabled provider plus every provider's site/secret keys
// in the nested shape upstream `CaptchaService.get` produces (current.ts res).
// 旧実装は flat な {provider, siteKey} のみで、frontend が res.hcaptcha.siteKey
// 等を読むと undefined になっていた。
func (h *Handler) CaptchaCurrent(c echo.Context) error {
	if h.metaRepo == nil {
		// frontend は res.hcaptcha.siteKey 等を無条件に参照するので、未配線でも
		// full nested shape (値は nil) を返して shape を崩さない。
		return c.JSON(http.StatusOK, captchaCurrentShape("none", &model.Meta{}))
	}
	meta, err := h.metaRepo.Fetch()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	provider := "none"
	switch {
	case meta.EnableHcaptcha:
		provider = "hcaptcha"
	case meta.EnableMcaptcha:
		provider = "mcaptcha"
	case meta.EnableRecaptcha:
		provider = "recaptcha"
	case meta.EnableTurnstile:
		provider = "turnstile"
	case meta.EnableTestcaptcha:
		provider = "testcaptcha"
	}
	return c.JSON(http.StatusOK, captchaCurrentShape(provider, meta))
}

// captchaCurrentShape builds the nested captcha/current response (upstream
// CaptchaService.get). nil *string fields serialize to JSON null (nullable:true).
func captchaCurrentShape(provider string, meta *model.Meta) map[string]any {
	return map[string]any{
		"provider": provider,
		"hcaptcha": map[string]any{
			"siteKey":   meta.HcaptchaSiteKey,
			"secretKey": meta.HcaptchaSecretKey,
		},
		"mcaptcha": map[string]any{
			"siteKey":     meta.McaptchaSiteKey,
			"secretKey":   meta.McaptchaSecretKey,
			"instanceUrl": meta.McaptchaInstanceURL,
		},
		"recaptcha": map[string]any{
			"siteKey":   meta.RecaptchaSiteKey,
			"secretKey": meta.RecaptchaSecretKey,
		},
		"turnstile": map[string]any{
			"siteKey":   meta.TurnstileSiteKey,
			"secretKey": meta.TurnstileSecretKey,
		},
	}
}

// CaptchaSave handles POST /api/admin/captcha/save.
//
// upstream paramDef は generic な {provider(required), captchaResult, sitekey,
// secret, instanceUrl}。選択 provider の captchaResult を検証し、pass した場合
// のみ per-provider の sitekey/secret/instanceUrl を保存する (CaptchaService.save)。
// 旧実装は per-provider key (hcaptchaSiteKey 等) を期待しており frontend の保存
// 値が DB に書かれず、captchaResult の検証も無かった。
func (h *Handler) CaptchaSave(c echo.Context) error {
	var req struct {
		Provider      string  `json:"provider"`
		CaptchaResult *string `json:"captchaResult"`
		Sitekey       *string `json:"sitekey"`
		Secret        *string `json:"secret"`
		InstanceURL   *string `json:"instanceUrl"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("Invalid parameters."))
	}
	if _, ok := supportedCaptchaProviders[req.Provider]; !ok {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PROVIDER", "Invalid provider.", "14bf7ae1-80cc-4363-acb2-4fd61d086af0"))
	}

	secret := strDeref(req.Secret)
	sitekey := strDeref(req.Sitekey)
	instanceURL := strDeref(req.InstanceURL)
	captchaResult := strDeref(req.CaptchaResult)

	// provider 別の必須 param 検証 + captchaResult 検証 (upstream CaptchaService.save)。
	needsVerify := false
	switch req.Provider {
	case "none":
		// 無効化。検証不要。
	case "mcaptcha":
		if secret == "" || sitekey == "" || instanceURL == "" || captchaResult == "" {
			return invalidCaptchaParams(c)
		}
		needsVerify = true
	case "testcaptcha":
		if captchaResult == "" {
			return invalidCaptchaParams(c)
		}
		needsVerify = true
	default: // hcaptcha / recaptcha / turnstile
		if secret == "" || captchaResult == "" {
			return invalidCaptchaParams(c)
		}
		needsVerify = true
	}
	if needsVerify {
		v := h.buildCaptchaVerifier(req.Provider, secret, sitekey, instanceURL)
		if v == nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("UNKNOWN", "unknown", "f868d509-e257-42a9-99c1-42614b031a97"))
		}
		if err := v.Verify(c.Request().Context(), captchaResult); err != nil {
			return mapCaptchaError(c, err)
		}
	}

	if h.metaRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	// enable flag は全 provider 分を上書きし、選択 provider のみ true にする。
	// per-provider の key は param が送られた (非 nil) ときだけ更新する
	// (upstream updateMeta の updateIfNotUndefined)。
	fields := map[string]any{
		"enableHcaptcha":    req.Provider == "hcaptcha",
		"enableMcaptcha":    req.Provider == "mcaptcha",
		"enableRecaptcha":   req.Provider == "recaptcha",
		"enableTurnstile":   req.Provider == "turnstile",
		"enableTestcaptcha": req.Provider == "testcaptcha",
	}
	switch req.Provider {
	case "hcaptcha":
		setIfProvided(fields, "hcaptchaSiteKey", req.Sitekey)
		setIfProvided(fields, "hcaptchaSecretKey", req.Secret)
	case "mcaptcha":
		// DB 列名は mcaptchaSitekey (lower k)。
		setIfProvided(fields, "mcaptchaSitekey", req.Sitekey)
		setIfProvided(fields, "mcaptchaSecretKey", req.Secret)
		setIfProvided(fields, "mcaptchaInstanceUrl", req.InstanceURL)
	case "recaptcha":
		setIfProvided(fields, "recaptchaSiteKey", req.Sitekey)
		setIfProvided(fields, "recaptchaSecretKey", req.Secret)
	case "turnstile":
		setIfProvided(fields, "turnstileSiteKey", req.Sitekey)
		setIfProvided(fields, "turnstileSecretKey", req.Secret)
	}
	if err := h.metaRepo.Update(fields); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	return c.NoContent(http.StatusNoContent)
}

// mapCaptchaError writes the upstream save.ts error response for a captcha
// verification error (sentinel errors from internal/core/captcha).
func mapCaptchaError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, captcha.ErrNoResponse):
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_RESPONSE_PROVIDED", "No response provided.", "40acbba8-0937-41fb-bb3f-474514d40afe"))
	case errors.Is(err, captcha.ErrRequestFailed):
		return c.JSON(http.StatusInternalServerError, apierr.Error("REQUEST_FAILED", "Request failed.", "0f4fe2f1-2c15-4d6e-b714-efbfcde231cd"))
	case errors.Is(err, captcha.ErrVerificationFail):
		return c.JSON(http.StatusBadRequest, apierr.Error("VERIFICATION_FAILED", "Verification failed.", "c41c067f-24f3-4150-84b2-b5a3ae8c2214"))
	default:
		return c.JSON(http.StatusInternalServerError, apierr.Error("UNKNOWN", "unknown", "f868d509-e257-42a9-99c1-42614b031a97"))
	}
}

// buildCaptchaVerifier returns the verifier for provider, using the injected
// factory when set (tests) or the real provider constructors otherwise.
func (h *Handler) buildCaptchaVerifier(provider, secret, sitekey, instanceURL string) captcha.Verifier {
	if h.captchaVerifierFactory != nil {
		return h.captchaVerifierFactory(provider, secret, sitekey, instanceURL)
	}
	switch provider {
	case "hcaptcha":
		return captcha.NewHcaptcha(secret)
	case "recaptcha":
		return captcha.NewRecaptcha(secret)
	case "turnstile":
		return captcha.NewTurnstile(secret)
	case "mcaptcha":
		return captcha.NewMcaptcha(instanceURL, sitekey, secret)
	case "testcaptcha":
		return captcha.NewTestcaptcha()
	}
	return nil
}

func invalidCaptchaParams(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAMETERS", "Invalid parameters.", "26654194-410e-44e2-b42e-460ff6f92476"))
}

// setIfProvided sets fields[col] to the dereferenced value only when the param
// pointer is non-nil (mirrors upstream updateIfNotUndefined).
func setIfProvided(fields map[string]any, col string, p *string) {
	if p != nil {
		fields[col] = *p
	}
}
