package router

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/render"

	"github.com/openmeterio/openmeter/api"
	"github.com/openmeterio/openmeter/internal/server/authenticator"
	"github.com/openmeterio/openmeter/pkg/contextx"
	"github.com/openmeterio/openmeter/pkg/models"
)

// CreatePortalToken creates a new portal token.
func (a *Router) CreatePortalToken(w http.ResponseWriter, r *http.Request) {
	ctx := contextx.WithAttr(r.Context(), "operation", "createPortalToken")

	if a.config.PortalTokenStrategy == nil {
		err := fmt.Errorf("not implemented: portal is not enabled")

		// TODO: caller error, no need to pass to error handler
		a.config.ErrorHandler.HandleContext(ctx, err)
		models.NewStatusProblem(ctx, err, http.StatusNotImplemented).Respond(slog.Default(), w, r)

		return
	}

	// Parse request body
	body := &api.CreatePortalTokenJSONRequestBody{}
	if err := render.DecodeJSON(r.Body, body); err != nil {
		err := fmt.Errorf("decode json: %w", err)

		// TODO: caller error, no need to pass to error handler
		a.config.ErrorHandler.HandleContext(ctx, err)
		models.NewStatusProblem(ctx, err, http.StatusBadRequest).Respond(slog.Default(), w, r)

		return
	}

	t, err := a.config.PortalTokenStrategy.Generate(body.Subject, body.AllowedMeterSlugs, body.ExpiresAt)
	if err != nil {
		err := fmt.Errorf("generate portal token: %w", err)

		a.config.ErrorHandler.HandleContext(ctx, err)
		models.NewStatusProblem(ctx, err, http.StatusInternalServerError).Respond(slog.Default(), w, r)

		return
	}

	render.JSON(w, r, api.PortalToken{
		Id:                t.Id,
		Token:             t.Token,
		ExpiresAt:         t.ExpiresAt,
		Subject:           t.Subject,
		AllowedMeterSlugs: t.AllowedMeterSlugs,
	})
}

func (a *Router) ListPortalTokens(w http.ResponseWriter, r *http.Request, params api.ListPortalTokensParams) {
	ctx := contextx.WithAttr(r.Context(), "operation", "listPortalTokens")

	err := fmt.Errorf("not implemented: portal token listing is an OpenMeter Cloud only feature")

	// TODO: caller error, no need to pass to error handler
	a.config.ErrorHandler.HandleContext(ctx, err)
	models.NewStatusProblem(r.Context(), err, http.StatusNotImplemented).Respond(slog.Default(), w, r)
}

func (a *Router) InvalidatePortalTokens(w http.ResponseWriter, r *http.Request) {
	ctx := contextx.WithAttr(r.Context(), "operation", "invalidatePortalTokens")

	err := fmt.Errorf("not implemented: portal token invalidation is an OpenMeter Cloud only feature")

	// TODO: caller error, no need to pass to error handler
	a.config.ErrorHandler.HandleContext(ctx, err)
	models.NewStatusProblem(r.Context(), err, http.StatusNotImplemented).Respond(slog.Default(), w, r)
}

func (a *Router) QueryPortalMeter(w http.ResponseWriter, r *http.Request, meterSlug string, params api.QueryPortalMeterParams) {
	ctx := contextx.WithAttr(r.Context(), "operation", "queryPortalMeter")
	ctx = contextx.WithAttr(ctx, "meterSlug", meterSlug)
	ctx = contextx.WithAttr(ctx, "params", params) // TODO: HOW ABOUT NO?

	subject := authenticator.GetAuthenticatedSubject(ctx)
	if subject == "" {
		err := fmt.Errorf("not authenticated")
		models.NewStatusProblem(ctx, err, http.StatusUnauthorized).Respond(slog.Default(), w, r)
		return
	}

	a.QueryMeter(w, r, meterSlug, api.QueryMeterParams{
		From:           params.From,
		To:             params.To,
		WindowSize:     params.WindowSize,
		WindowTimeZone: params.WindowTimeZone,
		Subject:        &[]string{subject},
		GroupBy:        params.GroupBy,
	})
}
