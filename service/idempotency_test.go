package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	rpccontext "github.com/alkaid/pitaya/v2/internal/rpccontext"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/alkaid/pitaya/v2/protos/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type RPCIdempotencyInfoRemote struct {
	component.Base
	observed       chan RPCIdempotencyInfo
	nextHopHasInfo chan bool
}

func (r *RPCIdempotencyInfoRemote) Inspect(ctx context.Context, input *test.SomeStruct) (*test.SomeStruct, error) {
	info, ok := RPCIdempotencyInfoFromContext(ctx)
	if !ok {
		return nil, errors.New("RPC idempotency info missing from current handler")
	}
	r.observed <- info

	metadata, err := pcontext.Encode(ctx)
	if err != nil {
		return nil, err
	}
	nextHop, err := pcontext.Decode(metadata)
	if err != nil {
		return nil, err
	}
	_, nextHopHasInfo := RPCIdempotencyInfoFromContext(nextHop)
	r.nextHopHasInfo <- nextHopHasInfo
	return input, nil
}

func TestRPCIdempotencyInfoIsVisibleOnlyAtCurrentRPCBoundary(t *testing.T) {
	for _, ctx := range []context.Context{nil, context.Background()} {
		info, ok := RPCIdempotencyInfoFromContext(ctx)
		assert.False(t, ok)
		assert.Empty(t, info)
	}

	server := cluster.NewServer("game-1", "game", false)
	remote := newRemoteCallService(server)
	handler := &RPCIdempotencyInfoRemote{
		observed:       make(chan RPCIdempotencyInfo, 1),
		nextHopHasInfo: make(chan bool, 1),
	}
	require.NoError(t, remote.Register(handler, nil))

	data, err := proto.Marshal(&test.SomeStruct{B: "accepted"})
	require.NoError(t, err)
	request := &protos.Request{
		Type: protos.RPCType_User,
		Msg: &protos.Msg{
			Route: "RPCIdempotencyInfoRemote.Inspect",
			Data:  data,
		},
	}
	const key = "command-1"
	ctx := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.IdempotencyKey, key,
		"tenant", "east",
	)
	wantStoreKey, err := idempotencyStoreKey(ctx, server, key)
	require.NoError(t, err)
	wantFingerprint, err := rpcRequestFingerprint(request, key, false, "")
	require.NoError(t, err)

	response, err := remote.Call(ctx, request)
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.Nil(t, response.Status)
	assert.Equal(t, RPCIdempotencyInfo{
		StoreKey:    wantStoreKey,
		Fingerprint: wantFingerprint,
	}, <-handler.observed)
	assert.False(t, <-handler.nextHopHasInfo, "local command identity must not enter propagated RPC metadata")
}

func TestMemoryIdempotencyStoreCoalescesConcurrentCommands(t *testing.T) {
	store := NewMemoryIdempotencyStore(MemoryIdempotencyStoreOptions{TTL: time.Minute, MaxEntries: 32})
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	operation := func(context.Context) (*protos.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return &protos.Response{Data: []byte("ok")}, nil
	}

	const callers = 16
	responses := make(chan *protos.Response, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := store.Execute(context.Background(), "scope:key", "fingerprint", operation)
			responses <- response
			errs <- err
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(responses)
	close(errs)

	assert.Equal(t, int32(1), calls.Load())
	for err := range errs {
		assert.NoError(t, err)
	}
	for response := range responses {
		require.NotNil(t, response)
		assert.Equal(t, []byte("ok"), response.Data)
		response.Data[0] = 'x'
	}
}

func TestMemoryIdempotencyStoreRejectsFingerprintConflict(t *testing.T) {
	store := NewMemoryIdempotencyStore(DefaultIdempotencyStoreOptions())
	_, err := store.Execute(context.Background(), "scope:key", "one", func(context.Context) (*protos.Response, error) {
		return &protos.Response{Data: []byte("first")}, nil
	})
	require.NoError(t, err)

	called := false
	_, err = store.Execute(context.Background(), "scope:key", "two", func(context.Context) (*protos.Response, error) {
		called = true
		return nil, nil
	})
	assert.ErrorIs(t, err, constants.ErrIdempotencyConflict)
	assert.False(t, called)
}

func TestMemoryIdempotencyStoreReplaysWithNilContext(t *testing.T) {
	store := NewMemoryIdempotencyStore(DefaultIdempotencyStoreOptions())
	_, err := store.Execute(context.Background(), "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
		return &protos.Response{Data: []byte("cached")}, nil
	})
	require.NoError(t, err)

	response, err := store.Execute(nil, "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
		t.Fatal("completed command executed during nil-context replay")
		return nil, nil
	})
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.Equal(t, []byte("cached"), response.Data)
}

func TestMemoryIdempotencyStoreWaitHonorsContext(t *testing.T) {
	store := NewMemoryIdempotencyStore(DefaultIdempotencyStoreOptions())
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = store.Execute(context.Background(), "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
			close(started)
			<-release
			return &protos.Response{}, nil
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.Execute(ctx, "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
		return nil, errors.New("must not execute")
	})
	assert.ErrorIs(t, err, context.Canceled)
	close(release)
	<-done
}

func TestMemoryIdempotencyStoreRejectsNewKeyWhenInflightCapacityIsFull(t *testing.T) {
	store := NewMemoryIdempotencyStore(MemoryIdempotencyStoreOptions{TTL: time.Minute, MaxEntries: 1})
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = store.Execute(context.Background(), "scope:first", "fingerprint", func(context.Context) (*protos.Response, error) {
			close(started)
			<-release
			return &protos.Response{}, nil
		})
	}()
	<-started

	called := false
	_, err := store.Execute(context.Background(), "scope:second", "fingerprint", func(context.Context) (*protos.Response, error) {
		called = true
		return &protos.Response{}, nil
	})
	assert.ErrorIs(t, err, constants.ErrIdempotencyStoreFull)
	assert.False(t, called)

	close(release)
	<-done
}

func TestValidateOwnerFence(t *testing.T) {
	server := cluster.NewServer("owner-1", "game", false)
	assert.NoError(t, validateOwnerFence(context.Background(), server))
	server.SetOwnerEpoch(42)
	valid := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.OwnerServerIDKey, "owner-1",
		constants.OwnerEpochKey, "42",
	)
	assert.NoError(t, validateOwnerFence(valid, server))
	server.InvalidateOwnerEpoch()
	assert.ErrorIs(t, validateOwnerFence(valid, server), constants.ErrStaleOwnerEpoch)
	server.SetOwnerEpoch(42)
	assert.NoError(t, validateOwnerFence(valid, server))

	stale := pcontext.AddToPropagateCtx(valid, constants.OwnerEpochKey, "41")
	assert.ErrorIs(t, validateOwnerFence(stale, server), constants.ErrStaleOwnerEpoch)
	wrongOwner := pcontext.AddToPropagateCtx(valid, constants.OwnerServerIDKey, "owner-2")
	assert.ErrorIs(t, validateOwnerFence(wrongOwner, server), constants.ErrStaleOwnerEpoch)
	assert.ErrorIs(t, validateOwnerFence(context.Background(), server), constants.ErrStaleOwnerEpoch)
	zero := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.OwnerServerIDKey, "owner-1",
		constants.OwnerEpochKey, "0",
	)
	assert.ErrorIs(t, validateOwnerFence(zero, server), constants.ErrStaleOwnerEpoch)
}

func TestRPCRequestFingerprintKeepsOrdinaryCallerIdentity(t *testing.T) {
	metadata, err := pcontext.Encode(pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.IdempotencyKey, "command-1",
		constants.OwnerServerIDKey, "game-1",
		constants.OwnerEpochKey, "7",
		constants.RequestTimeout, "5s",
		constants.RequestIDKey, "request-1",
		constants.PeerIDKey, "worker-1",
		constants.PeerServiceKey, "worker-a",
		"tenant", "east",
	))
	require.NoError(t, err)
	request := &protos.Request{
		Type:       protos.RPCType_User,
		Session:    &protos.Session{Id: 1, Uid: "user-1", Data: []byte("one"), Backends: map[string]string{"room": "room-1"}, Ip: "127.0.0.1"},
		Msg:        &protos.Msg{Route: "game.player.move", Data: []byte("north"), Reply: "reply-1"},
		FrontendID: "frontend-1",
		Metadata:   metadata,
	}
	first, err := rpcRequestFingerprint(request, "command-1", false, "")
	require.NoError(t, err)
	request.Msg.Reply = "reply-2"
	request.Metadata, err = pcontext.Encode(pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.IdempotencyKey, "command-1",
		constants.OwnerServerIDKey, "game-2",
		constants.OwnerEpochKey, "8",
		constants.RequestTimeout, "10s",
		constants.RequestIDKey, "request-2",
		constants.PeerIDKey, "worker-1",
		constants.PeerServiceKey, "worker-a",
		"tenant", "east",
	))
	require.NoError(t, err)
	retry, err := rpcRequestFingerprint(request, "command-1", false, "")
	require.NoError(t, err)
	assert.Equal(t, first, retry)

	for name, mutate := range map[string]func(*protos.Request){
		"frontend ID": func(candidate *protos.Request) {
			candidate.FrontendID = "frontend-2"
		},
		"peer ID": func(candidate *protos.Request) {
			candidate.Metadata, err = pcontext.Encode(pcontext.AddListToPropagateCtx(
				context.Background(),
				constants.IdempotencyKey, "command-1",
				constants.PeerIDKey, "worker-2",
				constants.PeerServiceKey, "worker-a",
				"tenant", "east",
			))
			require.NoError(t, err)
		},
		"peer service": func(candidate *protos.Request) {
			candidate.Metadata, err = pcontext.Encode(pcontext.AddListToPropagateCtx(
				context.Background(),
				constants.IdempotencyKey, "command-1",
				constants.PeerIDKey, "worker-1",
				constants.PeerServiceKey, "worker-b",
				"tenant", "east",
			))
			require.NoError(t, err)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := proto.Clone(request).(*protos.Request)
			mutate(candidate)
			fingerprint, err := rpcRequestFingerprint(candidate, "command-1", false, "")
			require.NoError(t, err)
			assert.NotEqual(t, first, fingerprint)
		})
	}

	request.FrontendID = "frontend-2"
	request.Metadata, err = pcontext.Encode(pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.IdempotencyKey, "command-1",
		constants.PeerIDKey, "worker-2",
		constants.PeerServiceKey, "worker-b",
		"tenant", "east",
	))
	require.NoError(t, err)
	changed, err := rpcRequestFingerprint(request, "command-1", false, "")
	require.NoError(t, err)
	assert.NotEqual(t, first, changed)

	request.FrontendID = "frontend-1"
	request.Metadata, err = pcontext.Encode(pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.IdempotencyKey, "command-1",
		constants.PeerIDKey, "worker-1",
		constants.PeerServiceKey, "worker",
		"tenant", "west",
	))
	require.NoError(t, err)
	changed, err = rpcRequestFingerprint(request, "command-1", false, "")
	require.NoError(t, err)
	assert.NotEqual(t, first, changed)
	request.Metadata = metadata
	request.Msg.Data = []byte("south")
	changed, err = rpcRequestFingerprint(request, "command-1", false, "")
	require.NoError(t, err)
	assert.NotEqual(t, first, changed)
	request.Msg.Data = []byte("north")
	request.Session.Data = []byte("two")
	changed, err = rpcRequestFingerprint(request, "command-1", false, "")
	require.NoError(t, err)
	assert.NotEqual(t, first, changed)
}

func TestRPCRequestFingerprintMakesReliableRetriesExecutorNeutral(t *testing.T) {
	const key = "job-1"
	digest := strings.Repeat("a", 64)
	metadata := func(peerID, peerService string) []byte {
		encoded, err := pcontext.Encode(pcontext.AddListToPropagateCtx(
			context.Background(),
			constants.IdempotencyKey, key,
			constants.OwnerServerIDKey, "game-1",
			constants.OwnerEpochKey, "7",
			constants.RequestIDKey, "request-1",
			constants.PeerIDKey, peerID,
			constants.PeerServiceKey, peerService,
			rpccontext.ReliableJobKey, key,
			rpccontext.ReliableJobMetadataDigestKey, digest,
			"tenant", "east",
		))
		require.NoError(t, err)
		return encoded
	}
	request := &protos.Request{
		Type:       protos.RPCType_User,
		Msg:        &protos.Msg{Route: "game.player.move", Data: []byte("north"), Reply: "reply-1"},
		FrontendID: "frontend-1",
		Metadata:   metadata("worker-1", "worker-a"),
	}
	first, err := rpcRequestFingerprint(request, key, true, digest)
	require.NoError(t, err)

	request.FrontendID = "frontend-2"
	request.Msg.Reply = "reply-2"
	request.Metadata = metadata("worker-2", "worker-b")
	retry, err := rpcRequestFingerprint(request, key, true, digest)
	require.NoError(t, err)
	assert.Equal(t, first, retry)

	otherDigest := strings.Repeat("b", 64)
	request.Metadata, err = pcontext.Encode(pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.IdempotencyKey, key,
		rpccontext.ReliableJobKey, key,
		rpccontext.ReliableJobMetadataDigestKey, otherDigest,
		"tenant", "east",
	))
	require.NoError(t, err)
	changed, err := rpcRequestFingerprint(request, key, true, otherDigest)
	require.NoError(t, err)
	assert.NotEqual(t, first, changed)
}

func TestRPCRequestFingerprintDistinguishesLargeJSONIntegers(t *testing.T) {
	request := &protos.Request{Metadata: []byte(`{"sequence":9007199254740992}`)}
	first, err := rpcRequestFingerprint(request, "", false, "")
	require.NoError(t, err)
	request.Metadata = []byte(`{"sequence":9007199254740993}`)
	second, err := rpcRequestFingerprint(request, "", false, "")
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
}

func TestRPCRequestFingerprintRejectsMalformedMetadata(t *testing.T) {
	_, err := rpcRequestFingerprint(&protos.Request{Metadata: []byte("not-json")}, "", false, "")
	assert.ErrorContains(t, err, "fingerprint RPC metadata")
}

func TestNewMemoryIdempotencyStoreAppliesDefaults(t *testing.T) {
	store := NewMemoryIdempotencyStore(MemoryIdempotencyStoreOptions{})
	defaults := DefaultIdempotencyStoreOptions()
	assert.Equal(t, defaults.TTL, store.ttl)
	assert.Equal(t, defaults.MaxEntries, store.maxEntries)
}

func TestMemoryIdempotencyStoreValidatesInputs(t *testing.T) {
	store := NewMemoryIdempotencyStore(DefaultIdempotencyStoreOptions())
	operation := func(context.Context) (*protos.Response, error) { return &protos.Response{}, nil }

	for _, key := range []string{"", strings.Repeat("x", MaxIdempotencyKeyBytes+1)} {
		_, err := store.Execute(context.Background(), key, "fingerprint", operation)
		assert.ErrorIs(t, err, constants.ErrIdempotencyKeyInvalid)
	}
	_, err := store.Execute(context.Background(), "valid", "fingerprint", nil)
	assert.ErrorIs(t, err, constants.ErrIdempotencyKeyInvalid)
}

func TestMemoryIdempotencyStoreExpiresAndEvictsCompletedResults(t *testing.T) {
	t.Run("expires", func(t *testing.T) {
		store := NewMemoryIdempotencyStore(MemoryIdempotencyStoreOptions{TTL: time.Millisecond, MaxEntries: 2})
		var calls atomic.Int32
		operation := func(context.Context) (*protos.Response, error) {
			calls.Add(1)
			return &protos.Response{Data: []byte("result")}, nil
		}

		_, err := store.Execute(context.Background(), "key", "first", operation)
		require.NoError(t, err)
		time.Sleep(5 * time.Millisecond)
		_, err = store.Execute(context.Background(), "key", "second", operation)
		require.NoError(t, err)
		assert.Equal(t, int32(2), calls.Load())
	})

	t.Run("evicts oldest completed entry", func(t *testing.T) {
		store := NewMemoryIdempotencyStore(MemoryIdempotencyStoreOptions{TTL: time.Minute, MaxEntries: 1})
		operation := func(context.Context) (*protos.Response, error) { return &protos.Response{}, nil }

		_, err := store.Execute(context.Background(), "first", "fingerprint", operation)
		require.NoError(t, err)
		_, err = store.Execute(context.Background(), "second", "fingerprint", operation)
		require.NoError(t, err)
		assert.NotContains(t, store.entries, "first")
		assert.Contains(t, store.entries, "second")
	})
}

func TestMemoryIdempotencyStoreRecordsPanicsForWaitingRetries(t *testing.T) {
	store := NewMemoryIdempotencyStore(DefaultIdempotencyStoreOptions())
	require.PanicsWithValue(t, "boom", func() {
		_, _ = store.Execute(context.Background(), "key", "fingerprint", func(context.Context) (*protos.Response, error) {
			panic("boom")
		})
	})

	called := false
	response, err := store.Execute(context.Background(), "key", "fingerprint", func(context.Context) (*protos.Response, error) {
		called = true
		return &protos.Response{}, nil
	})
	require.NotNil(t, response)
	require.NotNil(t, response.Status)
	assert.Equal(t, int32(http.StatusInternalServerError), response.Status.Code)
	assert.Contains(t, response.Status.Message, "idempotent RPC operation failed")
	assert.NotContains(t, response.Status.Message, "boom")
	assert.ErrorIs(t, err, errIdempotentRPCOperationFailed)
	assert.NotContains(t, err.Error(), "boom")
	assert.False(t, called)
}

func TestMemoryIdempotencyStoreCachesNilResponse(t *testing.T) {
	store := NewMemoryIdempotencyStore(DefaultIdempotencyStoreOptions())
	var calls atomic.Int32
	operation := func(context.Context) (*protos.Response, error) {
		calls.Add(1)
		return nil, nil
	}

	for range 2 {
		response, err := store.Execute(context.Background(), "key", "fingerprint", operation)
		require.NoError(t, err)
		assert.Nil(t, response)
	}
	assert.Equal(t, int32(1), calls.Load())
}

func TestIdempotencyKeyFromContextValidation(t *testing.T) {
	tests := []struct {
		name    string
		ctx     context.Context
		want    string
		wantErr bool
	}{
		{name: "missing", ctx: context.Background()},
		{name: "valid", ctx: pcontext.AddToPropagateCtx(context.Background(), constants.IdempotencyKey, "command-1"), want: "command-1"},
		{name: "wrong type", ctx: pcontext.AddToPropagateCtx(context.Background(), constants.IdempotencyKey, 1), wantErr: true},
		{name: "empty", ctx: pcontext.AddToPropagateCtx(context.Background(), constants.IdempotencyKey, ""), wantErr: true},
		{name: "too long", ctx: pcontext.AddToPropagateCtx(context.Background(), constants.IdempotencyKey, strings.Repeat("x", MaxIdempotencyKeyBytes+1)), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := idempotencyKeyFromContext(tt.ctx)
			if tt.wantErr {
				assert.ErrorIs(t, err, constants.ErrIdempotencyKeyInvalid)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, key)
		})
	}
}

func TestIdempotencyStoreKeyScopesRoutedFailoverAndFanout(t *testing.T) {
	first := cluster.NewServer("game-1", "game", false)
	second := cluster.NewServer("game-2", "game", false)
	key := strings.Repeat("x", MaxIdempotencyKeyBytes)

	routedFirst, err := idempotencyStoreKey(context.Background(), first, key)
	require.NoError(t, err)
	routedSecond, err := idempotencyStoreKey(context.Background(), second, key)
	require.NoError(t, err)
	assert.Equal(t, routedFirst, routedSecond)
	assert.True(t, strings.HasPrefix(routedFirst, idempotencyStoreKeyVersion+":"))
	assert.LessOrEqual(t, len(routedFirst), MaxIdempotencyKeyBytes)

	fanoutCtx := withFanoutIdempotencyScope(context.Background())
	fanoutFirst, err := idempotencyStoreKey(fanoutCtx, first, key)
	require.NoError(t, err)
	fanoutSecond, err := idempotencyStoreKey(fanoutCtx, second, key)
	require.NoError(t, err)
	assert.NotEqual(t, fanoutFirst, fanoutSecond)
	assert.NotEqual(t, routedFirst, fanoutFirst)

	for _, marker := range []any{"0", true} {
		invalid := pcontext.AddToPropagateCtx(context.Background(), constants.IdempotencyFanoutKey, marker)
		_, err := idempotencyStoreKey(invalid, first, key)
		assert.ErrorIs(t, err, constants.ErrIdempotencyKeyInvalid)
	}
}

func TestValidateOwnerFenceRejectsMalformedMetadata(t *testing.T) {
	server := cluster.NewServer("owner-1", "game", false)
	server.SetOwnerEpoch(42)

	wrongType := pcontext.AddToPropagateCtx(context.Background(), constants.OwnerEpochKey, uint64(42))
	assert.ErrorIs(t, validateOwnerFence(wrongType, server), constants.ErrStaleOwnerEpoch)
	invalidEpoch := pcontext.AddToPropagateCtx(context.Background(), constants.OwnerEpochKey, "not-a-number")
	assert.ErrorIs(t, validateOwnerFence(invalidEpoch, server), constants.ErrStaleOwnerEpoch)

	zero := pcontext.AddToPropagateCtx(context.Background(), constants.OwnerEpochKey, "0")
	assert.NoError(t, validateOwnerFence(zero, cluster.NewServer("owner-2", "game", false)))

	missingServer := pcontext.AddListToPropagateCtx(
		context.Background(),
		constants.OwnerServerIDKey, "owner-1",
		constants.OwnerEpochKey, "42",
	)
	assert.ErrorIs(t, validateOwnerFence(missingServer, nil), constants.ErrStaleOwnerEpoch)
}

func TestRPCRequestFingerprintHandlesNilRequest(t *testing.T) {
	first, err := rpcRequestFingerprint(nil, "", false, "")
	require.NoError(t, err)
	second, err := rpcRequestFingerprint(nil, "", false, "")
	require.NoError(t, err)
	assert.Equal(t, first, second)
}
