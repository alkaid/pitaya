package tracing

import (
	"context"
	"sync"

	"github.com/topfreegames/pitaya/v2/constants"
	pcontext "github.com/topfreegames/pitaya/v2/context"
	"github.com/topfreegames/pitaya/v2/logger"
	"go.opentelemetry.io/otel/propagation"
)

//var _ propagation.TextMapCarrier = (*Carrier)(nil)

// assert that Carrier implements the TextMapCarrier interface

var _ propagation.TextMapCarrier = (*Hunter)(nil)

type Hunter struct {
	Carrier map[string]string
	sync.RWMutex
}

func (h Hunter) Get(key string) string {
	h.RLock()
	defer h.RUnlock()
	return h.Carrier[key]
}

func (h Hunter) Set(key, value string) {
	h.Lock()
	h.Carrier[key] = value
	defer h.Unlock()

}

func (h Hunter) Keys() []string {
	h.RLock()
	defer h.RUnlock()
	out := make([]string, 0, len(h.Carrier))
	for key := range h.Carrier {
		out = append(out, key)
	}
	return out
}

func CarrierFromPropagateCtx(ctx context.Context) *Hunter {
	s := pcontext.GetFromPropagateCtx(ctx, constants.SpanPropagateCtxKey)
	if s == nil {
		return nil
	}

	switch sc := s.(type) {
	case map[string]any:
		carrier := &Hunter{
			Carrier: map[string]string{},
		}
		for _, v := range sc {
			info := v.(map[string]interface{})
			for key, val := range info {
				carrier.Set(key, val.(string))
			}
		}
		return carrier
	case *Hunter:
		return sc
	default:
		// 不可能发生.防御log
		logger.Zap.Error("unSupport type for get carrie from context")
		return nil
	}
}

//type Carrier map[string]string
//
//func (s Carrier) Get(key string) string {
//
//	return s[key]
//}
//
//func (s Carrier) Set(key, value string) {
//	s[key] = value
//}
//
//func (s Carrier) Keys() []string {
//	out := make([]string, 0, len(s))
//	for key := range s {
//		out = append(out, key)
//	}
//	return out
//}
//
//func CarrierFromPropagateCtx(ctx context.Context) Carrier {
//	s := pcontext.GetFromPropagateCtx(ctx, constants.SpanPropagateCtxKey)
//	if s == nil {
//		return nil
//	}
//	switch sc := s.(type) {
//	case map[string]any:
//		carrier := make(Carrier)
//		for k, v := range sc {
//			fmt.Println("----------------------+++ ,%s", k)
//			fmt.Println("----------------------+++ , %v", v)
//
//			carrier[k] = v.(string)
//		}
//		return carrier
//	case Carrier:
//		return sc
//	default:
//		// 不可能发生.防御log
//		logger.Zap.Error("unSupport type for get carrie from context")
//		return nil
//	}
//}
