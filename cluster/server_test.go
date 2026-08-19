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
	"testing"

	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var svTestTables = []struct {
	id       string
	svType   string
	metadata map[string]string
	frontend bool
}{
	{"someid-1", "somesvtype", map[string]string{"bla": "ola"}, true},
	{"someid-2", "somesvtype", nil, true},
	{"someid-3", "somesvtype", map[string]string{"sv": "game"}, false},
	{"someid-4", "somesvtype", map[string]string{}, false},
}

func TestNewServer(t *testing.T) {
	t.Parallel()
	for _, table := range svTestTables {
		t.Run(table.id, func(t *testing.T) {
			s := NewServer(table.id, table.svType, table.frontend, table.metadata)
			assert.Equal(t, table.id, s.ID)
			assert.Equal(t, table.metadata, s.Metadata)
			assert.Equal(t, table.frontend, s.Frontend)
			assert.NotNil(t, s.Hostname)
		})
	}
}

func TestAsJSONString(t *testing.T) {
	t.Parallel()
	for _, table := range svTestTables {
		t.Run(table.id, func(t *testing.T) {
			s := NewServer(table.id, table.svType, table.frontend, table.metadata)
			b, err := json.Marshal(s)
			assert.NoError(t, err)
			assert.Equal(t, string(b), s.AsJSONString())
		})
	}
}

func TestServerOwnershipAndSubscriptions(t *testing.T) {
	var nilServer *Server
	assert.Zero(t, nilServer.OwnerEpoch())
	nilServer.SetOwnerEpoch(99)

	server := &Server{}
	assert.Zero(t, server.OwnerEpoch())
	server.SetOwnerEpoch(41)
	assert.Equal(t, uint64(41), server.OwnerEpoch())
	server.SetOwnerEpoch(42)
	assert.Equal(t, uint64(42), server.OwnerEpoch())
	epoch, valid := server.OwnerEpochSnapshot()
	assert.Equal(t, uint64(42), epoch)
	assert.True(t, valid)
	server.InvalidateOwnerEpoch()
	epoch, valid = server.OwnerEpochSnapshot()
	assert.Equal(t, uint64(42), epoch)
	assert.False(t, valid)
	server.SetOwnerEpoch(43)
	epoch, valid = server.OwnerEpochSnapshot()
	assert.Equal(t, uint64(43), epoch)
	assert.True(t, valid)
	server.SetOwnerEpoch(ownerEpochInvalidBit)
	epoch, valid = server.OwnerEpochSnapshot()
	assert.Zero(t, epoch)
	assert.False(t, valid)

	server.fix()
	require.NotNil(t, server.Metadata)
	require.NotNil(t, server.Subscribe)
	server.AddSubscribe("chat", "send", true)
	assert.Equal(t, &SubscribeItem{Fork: true}, server.GetSubscribe("chat", "send"))
	assert.Nil(t, server.GetSubscribe("chat", "missing"))
}

func TestServerWithOwnerFencingMetadataReplacesPriorHop(t *testing.T) {
	_, err := withTargetOwnerFencingMetadata(context.Background(), nil)
	require.ErrorIs(t, err, constants.ErrStaleOwnerEpoch)

	target := &Server{ID: "room-2", Type: "room"}
	target.SetOwnerEpoch(42)
	ctx := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.OwnerServerIDKey, "room-1",
		constants.OwnerEpochKey, "41",
		"application-key", "preserved",
	)

	ctx = target.WithOwnerFencingMetadata(ctx)
	assert.Equal(t, "room-2", pcontext.GetFromPropagateCtx(ctx, constants.OwnerServerIDKey))
	assert.Equal(t, "42", pcontext.GetFromPropagateCtx(ctx, constants.OwnerEpochKey))
	assert.Equal(t, "preserved", pcontext.GetFromPropagateCtx(ctx, "application-key"))

	unfenced := &Server{ID: "local", Type: "local"}
	ctx = unfenced.WithOwnerFencingMetadata(ctx)
	assert.Nil(t, pcontext.GetFromPropagateCtx(ctx, constants.OwnerServerIDKey))
	assert.Nil(t, pcontext.GetFromPropagateCtx(ctx, constants.OwnerEpochKey))
	assert.Equal(t, "preserved", pcontext.GetFromPropagateCtx(ctx, "application-key"))
}
