package server

import (
	coreurlpreview "github.com/elythia-network/elythia/internal/core/urlpreview"
	"github.com/elythia-network/elythia/internal/model"
)

// urlPreviewConfig builds the URL preview settings from meta.
func urlPreviewConfig(m *model.Meta) coreurlpreview.Config {
	if m == nil {
		return coreurlpreview.Config{}
	}
	cfg := coreurlpreview.Config{
		Enabled:              m.URLPreviewEnabled,
		AllowRedirect:        m.URLPreviewAllowRedirect,
		TimeoutMs:            m.URLPreviewTimeout,
		MaxContentLength:     m.URLPreviewMaximumContentLength,
		RequireContentLength: m.URLPreviewRequireContentLength,
	}
	if m.URLPreviewUserAgent != nil {
		cfg.UserAgent = *m.URLPreviewUserAgent
	}
	if m.URLPreviewSummaryProxyURL != nil {
		cfg.SummaryProxyURL = *m.URLPreviewSummaryProxyURL
	}
	return cfg
}
