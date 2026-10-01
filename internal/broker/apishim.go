package broker

import (
	"context"
	"errors"

	"github.com/tinyorbitvn/tinycdi/internal/api"
)

// PublicIssuer adapts *Broker to the public API's api.ConnectionIssuer
// contract. It lives in this package — which already depends on api for
// Principal — so internal/api stays free of a broker import (no cycle).
type PublicIssuer struct {
	B *Broker
}

// IssueTicket issues a launch ticket and translates domain errors into the
// public error model (api.Error). The ticket token is returned verbatim to
// the handler and must only ever appear in the 201 response body.
func (s PublicIssuer) IssueTicket(ctx context.Context, p api.Principal, workspaceUID string, takeover bool) (api.IssuedTicket, *api.Error) {
	tk, err := s.B.IssueTicket(ctx, p, PlatformID(workspaceUID), takeover)
	if err != nil {
		return api.IssuedTicket{}, PublicIssueError(err)
	}
	return api.IssuedTicket{
		WorkspaceID: string(tk.WorkspaceID),
		Token:       tk.Token,
		ExpiresAt:   tk.ExpiresAt,
	}, nil
}

// PublicIssueError maps broker domain errors onto the public error model for
// POST /v1/workspaces/{id}/connections (openapi.yaml createConnection).
func PublicIssueError(err error) *api.Error {
	switch {
	case errors.Is(err, ErrNotFound):
		return api.NewError(api.CodeNotFound, "workspace not found")
	case errors.Is(err, ErrDenied):
		return api.NewError(api.CodeForbidden, "not allowed to connect to this workspace")
	case errors.Is(err, ErrNotReady):
		return api.NewError(api.CodeInvalidState, "workspace is not ready for connections")
	case errors.Is(err, ErrConnectionInUse):
		return api.NewError(api.CodeConnectionInUse, "workspace already has an active connection")
	case errors.Is(err, ErrFreshness):
		return api.NewError(api.CodeUnavailable, "workspace state is stale; retry shortly")
	default:
		return api.NewError(api.CodeInternal, "internal error")
	}
}
