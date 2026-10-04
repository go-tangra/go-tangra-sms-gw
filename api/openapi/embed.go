// Package openapi embeds the sms-gw management API contract.
package openapi

import _ "embed"

// SMSGW is the OpenAPI document served and validated by the service; the
// gateway manifest and the UI types are generated from it.
//
//go:embed sms-gw.yaml
var SMSGW []byte
