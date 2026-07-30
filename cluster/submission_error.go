package cluster

import (
	"context"
	"errors"

	"github.com/alkaid/ants/v2"
	"github.com/alkaid/goerrors/apierrors"
)

// NormalizeTaskSubmissionError converts internal task admission failures into
// stable wire-safe errors while preserving the original cause.
func NormalizeTaskSubmissionError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return apierrors.ClientClosed("task_submission_canceled", "task submission canceled", "").WithCause(err)
	case errors.Is(err, context.DeadlineExceeded):
		return apierrors.GatewayTimeout("task_submission_timeout", "task submission timed out", "").WithCause(err)
	case errors.Is(err, ants.ErrPoolClosed), errors.Is(err, ants.ErrPoolOverload):
		return apierrors.ServiceUnavailable("task_submission_unavailable", "task submission unavailable", "").WithCause(err)
	default:
		return apierrors.InternalServer("task_submission_failed", "task submission failed", "").WithCause(err)
	}
}
