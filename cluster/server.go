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

package cluster

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"sync/atomic"

	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/alkaid/pitaya/v2/logger"
	"go.uber.org/zap"
)

const ownerEpochInvalidBit = uint64(1) << 63

// Server struct
type Server struct {
	ID                 string                    `json:"id"`
	Type               string                    `json:"type"`
	Metadata           map[string]string         `json:"metadata"`
	Frontend           bool                      `json:"frontend"`
	Hostname           string                    `json:"hostname"`
	SessionStickiness  bool                      `json:"stickiness"` // 是否可以绑定session，绑定后将保持session粘连
	Subscribe          map[string]*SubscribeItem `json:"subscribe"`  // 订阅的topic,key为topic,value为fork,fork指同一服务是否所有实例都能消费
	ownerEpoch         *atomic.Uint64
	statefulOwnerFence *StatefulOwnerFence
}

// OwnerEpoch returns the service-discovery generation which owns this server
// identity. Zero means that the discovery backend cannot provide fencing.
func (s *Server) OwnerEpoch() uint64 {
	epoch, _ := s.OwnerEpochSnapshot()
	return epoch
}

// OwnerEpochSnapshot returns the epoch and whether this process can currently
// prove it still owns that epoch. Discovery implementations without fencing
// use the valid zero value.
func (s *Server) OwnerEpochSnapshot() (uint64, bool) {
	if s == nil {
		return 0, true
	}
	if s.ownerEpoch == nil {
		return 0, true
	}
	state := s.ownerEpoch.Load()
	return state &^ ownerEpochInvalidBit, state&ownerEpochInvalidBit == 0
}

// WithOwnerFencingMetadata replaces hop-level owner metadata with this target's
// identity and epoch. Targets without fencing clear metadata from a prior hop.
func (s *Server) WithOwnerFencingMetadata(ctx context.Context) context.Context {
	return s.withOwnerFencingMetadata(ctx, s.OwnerEpoch())
}

func (s *Server) withOwnerFencingMetadata(ctx context.Context, epoch uint64) context.Context {
	propagation := pcontext.ToMap(ctx)
	_, hadOwnerID := propagation[constants.OwnerServerIDKey]
	_, hadOwnerEpoch := propagation[constants.OwnerEpochKey]
	if epoch == 0 && !hadOwnerID && !hadOwnerEpoch {
		return s.withStatefulOwnerFencingMetadata(ctx)
	}

	delete(propagation, constants.OwnerServerIDKey)
	delete(propagation, constants.OwnerEpochKey)
	if epoch > 0 {
		propagation[constants.OwnerServerIDKey] = s.ID
		propagation[constants.OwnerEpochKey] = strconv.FormatUint(epoch, 10)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, constants.PropagateCtxKey, propagation)
	return s.withStatefulOwnerFencingMetadata(ctx)
}

func withTargetOwnerFencingMetadata(ctx context.Context, target *Server) (context.Context, error) {
	if target == nil {
		return ctx, constants.ErrStaleOwnerEpoch
	}
	epoch, valid := target.OwnerEpochSnapshot()
	if !valid {
		return ctx, constants.ErrStaleOwnerEpoch
	}
	return target.withOwnerFencingMetadata(ctx, epoch), nil
}

// SetOwnerEpoch publishes the service-discovery generation. Discovery
// implementations must set it before exposing a Server to readers.
func (s *Server) SetOwnerEpoch(epoch uint64) {
	if s != nil {
		if s.ownerEpoch == nil {
			s.ownerEpoch = &atomic.Uint64{}
		}
		// The high bit is reserved for local invalidation. Treat an out-of-range
		// generation as invalid instead of silently truncating it to an unfenced
		// epoch.
		if epoch&ownerEpochInvalidBit != 0 {
			s.ownerEpoch.Store(ownerEpochInvalidBit)
			return
		}
		s.ownerEpoch.Store(epoch)
	}
}

// InvalidateOwnerEpoch makes fenced RPC admission fail closed until discovery
// proves and republishes an owned registration generation.
func (s *Server) InvalidateOwnerEpoch() {
	if s == nil {
		return
	}
	if s.ownerEpoch == nil {
		s.ownerEpoch = &atomic.Uint64{}
	}
	for {
		current := s.ownerEpoch.Load()
		if current&ownerEpochInvalidBit != 0 || s.ownerEpoch.CompareAndSwap(current, current|ownerEpochInvalidBit) {
			return
		}
	}
}

type SubscribeItem struct {
	Fork bool `json:"fork"`
}

// NewServer ctor
func NewServer(id, serverType string, frontend bool, metadata ...map[string]string) *Server {
	d := make(map[string]string)
	h, err := os.Hostname()
	if err != nil {
		logger.Zap.Error("failed to get hostname", zap.Error(err))
	}
	if len(metadata) > 0 {
		d = metadata[0]
	}
	return &Server{
		ID:         id,
		Type:       serverType,
		Metadata:   d,
		Frontend:   frontend,
		Hostname:   h,
		Subscribe:  map[string]*SubscribeItem{},
		ownerEpoch: &atomic.Uint64{},
	}
}

func (s *Server) fix() {
	if s.ownerEpoch == nil {
		s.ownerEpoch = &atomic.Uint64{}
	}
	if s.Metadata == nil {
		s.Metadata = make(map[string]string)
	}
	if s.Subscribe == nil {
		s.Subscribe = make(map[string]*SubscribeItem)
	}
}

// AsJSONString returns the server as a json string
func (s *Server) AsJSONString() string {
	str, err := json.Marshal(s)
	if err != nil {
		logger.Zap.Error("error getting server as json", zap.Error(err))
		return ""
	}
	return string(str)
}
func (s *Server) AddSubscribe(service string, method string, fork bool) {
	s.Subscribe[service+"-"+method] = &SubscribeItem{Fork: fork}
}
func (s *Server) GetSubscribe(service string, method string) *SubscribeItem {
	return s.Subscribe[service+"-"+method]
}
