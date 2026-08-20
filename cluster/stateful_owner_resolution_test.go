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
	"errors"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	rpccontext "github.com/alkaid/pitaya/v2/internal/rpccontext"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type statefulOwnerResolverStub struct {
	ServiceDiscovery
	resolve func(context.Context, string, string, string) (*Server, StatefulOwnerFence, error)
}

type legacyServiceDiscoveryStub struct {
	ServiceDiscovery
}

type statefulAdmissionContextKey struct{}

func (s statefulOwnerResolverStub) ResolveStatefulOwner(
	ctx context.Context,
	serverType string,
	routingIdentity string,
	candidateServerID string,
) (*Server, StatefulOwnerFence, error) {
	return s.resolve(ctx, serverType, routingIdentity, candidateServerID)
}

func (statefulOwnerResolverStub) ValidateStatefulOwner(
	context.Context,
	string,
	string,
	StatefulOwnerFence,
) error {
	return nil
}

func TestWithTargetRPCAdmissionClassifiesStatefulCalls(t *testing.T) {
	sess, _ := session.NewSessionPool().NewSession(nil, false, "user-1")
	backend := NewServer("room-1", "room", false)
	frontend := NewServer("connector-1", "connector", true)
	assigned := backend.WithStatefulOwnerFence(StatefulOwnerFence{
		AssignmentKey: "assignment-1",
		Epoch:         1,
		ServerID:      backend.ID,
	})

	tests := []struct {
		name     string
		ctx      context.Context
		sess     session.Session
		target   *Server
		wantMode rpccontext.StatefulRouteMode
		want     bool
		fanout   bool
	}{
		{name: "nil session", ctx: context.Background(), target: backend},
		{name: "nil target", ctx: context.Background(), sess: sess},
		{name: "frontend target", ctx: context.Background(), sess: sess, target: frontend},
		{
			name:   "fanout authority",
			ctx:    rpccontext.WithRPCFanout(context.Background()),
			sess:   sess,
			target: backend,
			fanout: true,
		},
		{
			name:     "existing authority",
			ctx:      rpccontext.WithStatefulRouteMode(context.Background(), rpccontext.StatefulRouteModeAssigned),
			sess:     sess,
			target:   backend,
			wantMode: rpccontext.StatefulRouteModeAssigned,
			want:     true,
		},
		{
			name:     "direct target",
			ctx:      context.Background(),
			sess:     sess,
			target:   backend,
			wantMode: rpccontext.StatefulRouteModeDirect,
			want:     true,
		},
		{
			name:     "assigned target",
			ctx:      context.Background(),
			sess:     sess,
			target:   assigned,
			wantMode: rpccontext.StatefulRouteModeAssigned,
			want:     true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deadline := time.Now().Add(time.Hour)
			ctx, cancel := context.WithDeadline(
				context.WithValue(test.ctx, statefulAdmissionContextKey{}, "preserved"),
				deadline,
			)
			defer cancel()

			actual := withTargetRPCAdmission(ctx, test.sess, test.target)
			mode, ok := rpccontext.StatefulRouteModeFromContext(actual)
			assert.Equal(t, test.want, ok)
			assert.Equal(t, test.wantMode, mode)
			assert.Equal(t, test.fanout, rpccontext.RPCFanoutFromContext(actual))
			assert.Equal(t, "preserved", actual.Value(statefulAdmissionContextKey{}))
			actualDeadline, hasDeadline := actual.Deadline()
			require.True(t, hasDeadline)
			assert.Equal(t, deadline, actualDeadline)
		})
	}
}

func TestResolveStatefulServerBypassesInapplicableTargets(t *testing.T) {
	sess, _ := session.NewSessionPool().NewSession(nil, false, "user-1")
	backend := NewServer("room-1", "room", false)
	frontend := NewServer("connector-1", "connector", true)
	resolver := statefulOwnerResolverStub{
		resolve: func(context.Context, string, string, string) (*Server, StatefulOwnerFence, error) {
			t.Fatal("resolver must not run for an inapplicable target")
			return nil, StatefulOwnerFence{}, nil
		},
	}

	tests := []struct {
		name      string
		discovery ServiceDiscovery
		target    *Server
		sess      session.Session
	}{
		{name: "nil target", discovery: resolver, sess: sess},
		{name: "frontend target", discovery: resolver, target: frontend, sess: sess},
		{name: "nil session", discovery: resolver, target: backend},
		{name: "legacy discovery", discovery: legacyServiceDiscoveryStub{}, target: backend, sess: sess},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual, err := ResolveStatefulServer(
				context.Background(),
				test.discovery,
				"room",
				test.target,
				test.sess,
			)
			require.NoError(t, err)
			assert.Same(t, test.target, actual)
		})
	}
}

func TestResolveStatefulServerRejectsInvalidRouteInputs(t *testing.T) {
	resolver := statefulOwnerResolverStub{
		resolve: func(context.Context, string, string, string) (*Server, StatefulOwnerFence, error) {
			t.Fatal("resolver must not run for an invalid route")
			return nil, StatefulOwnerFence{}, nil
		},
	}
	sess, _ := session.NewSessionPool().NewSession(nil, false, "user-1")

	actual, err := ResolveStatefulServer(
		context.Background(),
		resolver,
		"chat",
		NewServer("room-1", "room", false),
		sess,
	)
	assert.Nil(t, actual)
	assert.ErrorIs(t, err, constants.ErrStatefulOwnerAssignmentCorrupt)

	anonymous, _ := session.NewSessionPool().NewSession(nil, false)
	actual, err = ResolveStatefulServer(
		context.Background(),
		resolver,
		"room",
		NewServer("room-1", "room", false),
		anonymous,
	)
	assert.Nil(t, actual)
	assert.ErrorIs(t, err, constants.ErrStatefulRouteKeyNotFound)
}

func TestResolveStatefulServerValidatesPrefencedTargets(t *testing.T) {
	const serverType = "room"
	sess, _ := session.NewSessionPool().NewSession(nil, false, "user-1")
	routingIdentity, err := StatefulOwnerRoutingKey(sess.UID(), "", 0)
	require.NoError(t, err)
	expectedKey, err := StatefulOwnerAssignmentKey(serverType, routingIdentity)
	require.NoError(t, err)
	resolver := statefulOwnerResolverStub{
		resolve: func(context.Context, string, string, string) (*Server, StatefulOwnerFence, error) {
			t.Fatal("resolver must not replace a pre-fenced target")
			return nil, StatefulOwnerFence{}, nil
		},
	}

	tests := []struct {
		name    string
		fence   StatefulOwnerFence
		wantErr error
	}{
		{
			name: "valid",
			fence: StatefulOwnerFence{
				AssignmentKey: expectedKey,
				Epoch:         7,
				ServerID:      "room-1",
			},
		},
		{
			name: "wrong server",
			fence: StatefulOwnerFence{
				AssignmentKey: expectedKey,
				Epoch:         7,
				ServerID:      "room-2",
			},
			wantErr: constants.ErrStatefulOwnerAssignmentCorrupt,
		},
		{
			name: "missing assignment",
			fence: StatefulOwnerFence{
				Epoch:    7,
				ServerID: "room-1",
			},
			wantErr: constants.ErrStatefulOwnerAssignmentCorrupt,
		},
		{
			name: "zero epoch",
			fence: StatefulOwnerFence{
				AssignmentKey: expectedKey,
				ServerID:      "room-1",
			},
			wantErr: constants.ErrStatefulOwnerAssignmentCorrupt,
		},
		{
			name: "different assignment",
			fence: StatefulOwnerFence{
				AssignmentKey: "different-assignment",
				Epoch:         7,
				ServerID:      "room-1",
			},
			wantErr: constants.ErrStaleStatefulOwner,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := NewServer("room-1", serverType, false).WithStatefulOwnerFence(test.fence)
			actual, err := ResolveStatefulServer(
				context.Background(),
				resolver,
				serverType,
				target,
				sess,
			)
			if test.wantErr != nil {
				assert.Nil(t, actual)
				assert.ErrorIs(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Same(t, target, actual)
		})
	}

	emptyTypeTarget := NewServer("room-1", "", false).WithStatefulOwnerFence(StatefulOwnerFence{
		AssignmentKey: "assignment-1",
		Epoch:         1,
		ServerID:      "room-1",
	})
	actual, err := ResolveStatefulServer(
		context.Background(),
		resolver,
		"",
		emptyTypeTarget,
		sess,
	)
	assert.Nil(t, actual)
	assert.ErrorIs(t, err, constants.ErrStatefulOwnerAssignmentUnavailable)
}

func TestResolveStatefulServerValidatesResolverResult(t *testing.T) {
	const serverType = "room"
	sess, _ := session.NewSessionPool().NewSession(nil, false, "user-1")
	routingIdentity, err := StatefulOwnerRoutingKey(sess.UID(), "", 0)
	require.NoError(t, err)
	expectedKey, err := StatefulOwnerAssignmentKey(serverType, routingIdentity)
	require.NoError(t, err)
	resolverErr := errors.New("resolver unavailable")

	tests := []struct {
		name    string
		owner   *Server
		fence   StatefulOwnerFence
		err     error
		wantErr error
	}{
		{name: "resolver error", err: resolverErr, wantErr: resolverErr},
		{
			name:    "nil owner",
			fence:   StatefulOwnerFence{AssignmentKey: expectedKey, Epoch: 7, ServerID: "room-1"},
			wantErr: constants.ErrStatefulOwnerAssignmentCorrupt,
		},
		{
			name:    "owner and fence disagree",
			owner:   NewServer("room-1", serverType, false),
			fence:   StatefulOwnerFence{AssignmentKey: expectedKey, Epoch: 7, ServerID: "room-2"},
			wantErr: constants.ErrStatefulOwnerAssignmentCorrupt,
		},
		{
			name:    "wrong owner type",
			owner:   NewServer("room-1", "chat", false),
			fence:   StatefulOwnerFence{AssignmentKey: expectedKey, Epoch: 7, ServerID: "room-1"},
			wantErr: constants.ErrStatefulOwnerAssignmentCorrupt,
		},
		{
			name:    "wrong assignment",
			owner:   NewServer("room-1", serverType, false),
			fence:   StatefulOwnerFence{AssignmentKey: "different-assignment", Epoch: 7, ServerID: "room-1"},
			wantErr: constants.ErrStatefulOwnerAssignmentCorrupt,
		},
		{
			name:    "zero epoch",
			owner:   NewServer("room-1", serverType, false),
			fence:   StatefulOwnerFence{AssignmentKey: expectedKey, ServerID: "room-1"},
			wantErr: constants.ErrStatefulOwnerAssignmentCorrupt,
		},
		{
			name:  "valid",
			owner: NewServer("room-1", serverType, false),
			fence: StatefulOwnerFence{AssignmentKey: expectedKey, Epoch: 7, ServerID: "room-1"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := statefulOwnerResolverStub{
				resolve: func(
					_ context.Context,
					actualType string,
					actualIdentity string,
					candidateID string,
				) (*Server, StatefulOwnerFence, error) {
					assert.Equal(t, serverType, actualType)
					assert.Equal(t, routingIdentity, actualIdentity)
					assert.Equal(t, "candidate-1", candidateID)
					return test.owner, test.fence, test.err
				},
			}
			actual, err := ResolveStatefulServer(
				context.Background(),
				resolver,
				serverType,
				NewServer("candidate-1", serverType, false),
				sess,
			)
			if test.wantErr != nil {
				assert.Nil(t, actual)
				assert.ErrorIs(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, actual)
			assert.NotSame(t, test.owner, actual)
			actualFence, ok := actual.StatefulOwnerFenceSnapshot()
			require.True(t, ok)
			assert.Equal(t, test.fence, actualFence)
		})
	}

	emptyTypeResolver := statefulOwnerResolverStub{
		resolve: func(context.Context, string, string, string) (*Server, StatefulOwnerFence, error) {
			return NewServer("room-1", "", false), StatefulOwnerFence{
				AssignmentKey: "assignment-1",
				Epoch:         1,
				ServerID:      "room-1",
			}, nil
		},
	}
	actual, err := ResolveStatefulServer(
		context.Background(),
		emptyTypeResolver,
		"",
		NewServer("candidate-1", "", false),
		sess,
	)
	assert.Nil(t, actual)
	assert.ErrorIs(t, err, constants.ErrStatefulOwnerAssignmentUnavailable)
}

func TestStatefulRoutingKeysRequireCompleteIdentity(t *testing.T) {
	key, err := StatefulRoutingKey(nil)
	assert.Empty(t, key)
	assert.ErrorIs(t, err, constants.ErrStatefulRouteKeyNotFound)

	anonymous, _ := session.NewSessionPool().NewSession(nil, false)
	key, err = StatefulRoutingKey(anonymous)
	assert.Empty(t, key)
	assert.ErrorIs(t, err, constants.ErrStatefulRouteKeyNotFound)

	anonymous.SetFrontendData("connector-1", 42)
	key, err = StatefulRoutingKey(anonymous)
	require.NoError(t, err)
	assert.Equal(t, "connector-1-42", key)
	identified, _ := session.NewSessionPool().NewSession(nil, false, "user-1")
	key, err = StatefulRoutingKey(identified)
	require.NoError(t, err)
	assert.Equal(t, "user-1", key)

	key, err = StatefulOwnerRoutingKey("", "", 0)
	assert.Empty(t, key)
	assert.ErrorIs(t, err, constants.ErrStatefulRouteKeyNotFound)
	assert.Nil(t, (*Server)(nil).WithStatefulOwnerFence(StatefulOwnerFence{}))
}

func TestStatefulOwnerFencingMetadataReplacesPriorHop(t *testing.T) {
	server := NewServer("room-1", "room", false)
	ordinary := context.Background()
	assert.Equal(t, ordinary, server.withStatefulOwnerFencingMetadata(ordinary))

	spoofed := pcontext.AddListToPropagateCtx(
		ordinary,
		"tenant", "east",
		constants.StatefulOwnerKey, "stale-assignment",
		constants.StatefulOwnerEpochKey, "99",
	)
	cleaned := server.withStatefulOwnerFencingMetadata(spoofed)
	assert.Equal(t, "east", pcontext.GetFromPropagateCtx(cleaned, "tenant"))
	assert.Nil(t, pcontext.GetFromPropagateCtx(cleaned, constants.StatefulOwnerKey))
	assert.Nil(t, pcontext.GetFromPropagateCtx(cleaned, constants.StatefulOwnerEpochKey))

	fenced := server.WithStatefulOwnerFence(StatefulOwnerFence{
		AssignmentKey: "assignment-1",
		Epoch:         7,
		ServerID:      server.ID,
	})
	injected := fenced.withStatefulOwnerFencingMetadata(nil)
	assert.Equal(t, "assignment-1", pcontext.GetFromPropagateCtx(injected, constants.StatefulOwnerKey))
	assert.Equal(t, "7", pcontext.GetFromPropagateCtx(injected, constants.StatefulOwnerEpochKey))
}
