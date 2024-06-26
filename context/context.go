// Copyright (c) TFG Co. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package context

import (
	"context"
	"encoding/json"

	"github.com/pkg/errors"
	"github.com/topfreegames/pitaya/v2/logger"

	"github.com/topfreegames/pitaya/v2/constants"
)

// AddToPropagateCtx adds a key and value that will be propagated through RPC calls
func AddToPropagateCtx(ctx context.Context, key string, val any) context.Context {
	propagate := ToMap(ctx)
	propagate[key] = val
	return context.WithValue(ctx, constants.PropagateCtxKey, propagate)
}
func AddListToPropagateCtx(ctx context.Context, keyOrValList ...any) context.Context {
	if len(keyOrValList) == 0 {
		logger.Zap.Warn("keyOrValList cannot be empty")
		return ctx
	}
	propagate := ToMap(ctx)
	key := ""
	for i, keyOrVal := range keyOrValList {
		if i%2 == 0 {
			key = keyOrVal.(string)
		} else {
			propagate[key] = keyOrVal
		}
	}
	return context.WithValue(ctx, constants.PropagateCtxKey, propagate)
}

// GetFromPropagateCtx get a value from the propagate
func GetFromPropagateCtx(ctx context.Context, key string) any {
	propagate := toRaw(ctx)
	if val, ok := propagate[key]; ok {
		return val
	}
	return nil
}

// toRaw returns the values that will be propagated through RPC calls in map[string]any format
//
//	框架内部使用
//	@param ctx
//	@return map[string]any
func toRaw(ctx context.Context) map[string]any {
	if ctx == nil {
		return map[string]any{}
	}
	p := ctx.Value(constants.PropagateCtxKey)
	if p != nil {
		return p.(map[string]any)
	}
	return map[string]any{}
}

// ToMap returns the values that will be propagated through RPC calls in map[string]any format
func ToMap(ctx context.Context) map[string]any {
	if ctx == nil {
		return map[string]any{}
	}
	p := ctx.Value(constants.PropagateCtxKey)
	if p != nil {
		tmpP := p.(map[string]any)
		ret := map[string]any{}
		for k, v := range tmpP {
			ret[k] = v
		}
		return ret
	}
	return map[string]any{}
}

// FromMap creates a new context from a map with propagated values
func FromMap(val map[string]any) context.Context {
	return context.WithValue(context.Background(), constants.PropagateCtxKey, val)
}

// Encode returns the given propagatable context encoded in binary format
func Encode(ctx context.Context) ([]byte, error) {
	m := ToMap(ctx)
	if len(m) > 0 {
		return json.Marshal(m)
	}
	return nil, nil
}

// Decode returns a context given a binary encoded message
func Decode(m []byte) (context.Context, error) {
	if len(m) == 0 {
		// TODO maybe return an error
		return nil, nil
	}
	mp := make(map[string]any, 0)
	err := json.Unmarshal(m, &mp)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return FromMap(mp), nil
}
