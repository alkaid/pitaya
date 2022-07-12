package tracing

import (
	"context"

	"github.com/topfreegames/pitaya/v2/constants"
	pcontext "github.com/topfreegames/pitaya/v2/context"
	"github.com/topfreegames/pitaya/v2/logger"
	"go.opentelemetry.io/otel/propagation"
)

// assert that Carrier implements the TextMapCarrier interface
var _ propagation.TextMapCarrier = (*Carrier)(nil)

type Carrier map[string]string

func (s Carrier) Get(key string) string {
	return s[key]
}

func (s Carrier) Set(key, value string) {
	s[key] = value
}

func (s Carrier) Keys() []string {
	out := make([]string, 0, len(s))
	for key := range s {
		out = append(out, key)
	}
	return out
}

func CarrierFromPropagateCtx(ctx context.Context) Carrier {
	s := pcontext.GetFromPropagateCtx(ctx, constants.SpanPropagateCtxKey)
	if s == nil {
		return nil
	}
	switch sc := s.(type) {
	case map[string]any:
		carrier := make(Carrier)
		for k, v := range sc {
			carrier[k] = v.(string)
		}
		return carrier
	case Carrier:
		return sc
	default:
		// 不可能发生.防御log
		logger.Zap.Error("unSupport type for get carrie from context")
		return nil
	}
}
