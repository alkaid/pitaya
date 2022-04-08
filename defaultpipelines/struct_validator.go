package defaultpipelines

import (
	"context"

	"github.com/topfreegames/pitaya/v2/route"
)

// StructValidator is the interface that must be implemented
// by a struct validator for the request arguments on pitaya.
//
// The default struct validator used by pitaya is https://github.com/go-playground/validator.
type StructValidator interface {
	Validate(context.Context, route.Route, interface{}) (context.Context, interface{}, error)
}

// StructValidatorInstance holds the default validator
// on start but can be overridden if needed.
var StructValidatorInstance StructValidator = &DefaultValidator{}
