package scim

import (
	"errors"

	"github.com/artpar/api2go/v2"
	"github.com/daptin/daptin/server/resource"
)

// APIError is a transport-independent SCIM failure with HTTP status and
// scimType. Handlers translate it to an ErrorResponse.
type APIError struct {
	Status   int
	ScimType string
	Detail   string
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	return e.ScimType
}

func newAPIError(status int, scimType, detail string) *APIError {
	return &APIError{Status: status, ScimType: scimType, Detail: detail}
}

func notFound(detail string) *APIError {
	return newAPIError(404, "", detail)
}

func invalidValue(detail string) *APIError {
	return newAPIError(400, ScimTypeInvalidValue, detail)
}

func invalidSyntax(detail string) *APIError {
	return newAPIError(400, ScimTypeInvalidSyntax, detail)
}

func uniquenessConflict(detail string) *APIError {
	return newAPIError(409, ScimTypeUniqueness, detail)
}

func versionConflict(detail string) *APIError {
	return newAPIError(409, ScimTypeInvalidVers, detail)
}

func preconditionFailed(detail string) *APIError {
	return newAPIError(412, "", detail)
}

// mapWriteError converts resource-layer errors to SCIM API errors.
func mapWriteError(err error) error {
	if err == nil {
		return nil
	}
	var httpError api2go.HTTPError
	if errors.As(err, &httpError) && httpError.Status() == 409 {
		return uniquenessConflict("one or more unique attributes are already in use")
	}
	if errors.Is(err, resource.ErrVersionConflict) {
		return versionConflict("the resource was modified concurrently; re-read it and retry")
	}
	return err
}
