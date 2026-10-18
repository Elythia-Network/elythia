package paramschema

import "github.com/elythia-network/elythia/internal/api/apierr"

// Response returns the error body upstream answers a failed validation with
// (endpoint-base.ts): INVALID_PARAM with id 3d81ceae-..., message
// "Invalid param.", kind "client" and info {param: errors[0].schemaPath,
// reason: errors[0].message}. The status is 400.
//
// It is apierr.InvalidParamClient, which handlers have so far called with
// hand-written ajv-like strings; here the strings are ajv's own.
func (e *Error) Response() map[string]any {
	return apierr.InvalidParamClient(e.SchemaPath, e.Message)
}
