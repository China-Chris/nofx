package api

import _ "embed"

// swaggerSpec holds the OpenAPI specification served at /swagger/doc.json.
//
//go:embed swagger/openapi.json
var swaggerSpec []byte
