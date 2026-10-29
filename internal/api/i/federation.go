package i

import (
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/core/localfederation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// Federation returns the authenticated account's federation state and first external reference.
func (h *Handler) Federation(c echo.Context) error {
	user := middleware.GetUser(c)
	if user == nil || h.localFederationService == nil {
		return apierr.JSONInternalError(c)
	}
	status, err := h.localFederationService.GetStatus(user.ID)
	if err != nil {
		if err == localfederation.ErrNotLocalAccount {
			return apierr.JSONInvalidParam(c)
		}
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, status)
}

// UpdateFederation changes whether the authenticated local account may federate.
func (h *Handler) UpdateFederation(c echo.Context) error {
	user := middleware.GetUser(c)
	if user == nil || h.localFederationService == nil {
		return apierr.JSONInternalError(c)
	}
	var req struct {
		FederationStatus model.FederationStatus `json:"federationStatus"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	status, err := h.localFederationService.ChangeStatus(user.ID, req.FederationStatus)
	if err != nil {
		switch err {
		case localfederation.ErrFederationAlreadyReferenced:
			return c.JSON(http.StatusBadRequest, apierr.Error("FEDERATION_ALREADY_REFERENCED", "Federation cannot be disabled after the account is referenced externally.", "2cdb926d-44f0-48a4-8bde-4e4653f3a1df"))
		case localfederation.ErrSystemAccount:
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1fb7cb09-d46a-4fff-b8df-057708cce513"))
		case localfederation.ErrInvalidFederationStatus, localfederation.ErrNotLocalAccount:
			return apierr.JSONInvalidParam(c)
		default:
			return apierr.JSONInternalError(c)
		}
	}
	h.invalidateUserTokenCache(user.ID)
	h.publishMeUpdatedPartial(user.ID, map[string]any{"federationStatus": status.FederationStatus})
	return c.JSON(http.StatusOK, status)
}
