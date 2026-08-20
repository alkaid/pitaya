package service

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type scriptedRedisIdempotencyClient struct {
	eval func(context.Context, string, []string, ...interface{}) *redis.Cmd
}

func (c *scriptedRedisIdempotencyClient) Eval(
	ctx context.Context,
	script string,
	keys []string,
	args ...interface{},
) *redis.Cmd {
	return c.eval(ctx, script, keys, args...)
}

func testRedisIdempotencyStore(client redisIdempotencyCommands) *RedisIdempotencyStore {
	return &RedisIdempotencyStore{client: client, options: DefaultRedisIdempotencyStoreOptions()}
}

func TestRedisIdempotencyStoreOptionsValidation(t *testing.T) {
	defaults := DefaultRedisIdempotencyStoreOptions()
	require.NoError(t, defaults.validate())
	assert.Equal(t, defaultRedisIdempotencyKeyPrefix, defaults.KeyPrefix)

	tests := []struct {
		name   string
		mutate func(*RedisIdempotencyStoreOptions)
	}{
		{name: "prefix", mutate: func(o *RedisIdempotencyStoreOptions) { o.KeyPrefix = " " }},
		{name: "result TTL", mutate: func(o *RedisIdempotencyStoreOptions) { o.ResultTTL = 0 }},
		{name: "lease TTL", mutate: func(o *RedisIdempotencyStoreOptions) { o.LeaseTTL = 0 }},
		{name: "heartbeat", mutate: func(o *RedisIdempotencyStoreOptions) { o.HeartbeatInterval = 0 }},
		{name: "poll", mutate: func(o *RedisIdempotencyStoreOptions) { o.PollInterval = 0 }},
		{name: "operation timeout", mutate: func(o *RedisIdempotencyStoreOptions) { o.OperationTimeout = 0 }},
		{name: "unsafe renewal budget", mutate: func(o *RedisIdempotencyStoreOptions) {
			o.HeartbeatInterval = o.LeaseTTL - o.OperationTimeout
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := defaults
			test.mutate(&options)
			require.Error(t, options.validate())
		})
	}

	_, err := NewRedisIdempotencyStore(nil, defaults)
	require.Error(t, err)
}

func TestRedisIdempotencyResultRoundTrip(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		encoded, err := encodeRedisIdempotencyResult(&protos.Response{Data: []byte("ok")}, nil)
		require.NoError(t, err)
		decoded, err := decodeRedisIdempotencyResult(encoded.has, encoded.encoded, encoded.hadError)
		require.NoError(t, err)
		require.NotNil(t, decoded.response)
		assert.Equal(t, []byte("ok"), decoded.response.Data)
		assert.NoError(t, decoded.err)
	})

	t.Run("error", func(t *testing.T) {
		encoded, err := encodeRedisIdempotencyResult(nil, errors.New("operation failed"))
		require.NoError(t, err)
		decoded, err := decodeRedisIdempotencyResult(encoded.has, encoded.encoded, encoded.hadError)
		require.NoError(t, err)
		require.NotNil(t, decoded.response)
		require.NotNil(t, decoded.response.Status)
		assert.Equal(t, int32(http.StatusInternalServerError), decoded.response.Status.Code)
		require.Error(t, decoded.err)
	})

	t.Run("nil success", func(t *testing.T) {
		encoded, err := encodeRedisIdempotencyResult(nil, nil)
		require.NoError(t, err)
		decoded, err := decodeRedisIdempotencyResult(encoded.has, encoded.encoded, encoded.hadError)
		require.NoError(t, err)
		assert.Nil(t, decoded.response)
		assert.NoError(t, decoded.err)
	})
}

func TestRedisIdempotencyResultRejectsCorruption(t *testing.T) {
	for _, values := range [][3]any{
		{"invalid", "", "0"},
		{"0", "unexpected", "0"},
		{"0", "", "1"},
		{"1", "", "0"},
		{"1", "not-protobuf", "0"},
		{"1", mustMarshalResponse(t, &protos.Response{}), "1"},
	} {
		_, err := decodeRedisIdempotencyResult(values[0], values[1], values[2])
		assert.ErrorIs(t, err, constants.ErrIdempotencyStoreCorrupt)
	}
}

func mustMarshalResponse(t *testing.T, response *protos.Response) []byte {
	t.Helper()
	encoded, err := proto.Marshal(response)
	require.NoError(t, err)
	return encoded
}

func TestRedisIdempotencyStoreExecutesAndCommitsOwnedCommand(t *testing.T) {
	var calls atomic.Int32
	client := &scriptedRedisIdempotencyClient{}
	client.eval = func(_ context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
		require.Equal(t, []string{defaultRedisIdempotencyKeyPrefix + "scope:key"}, keys)
		switch script {
		case redisIdempotencyAcquireScript:
			return redis.NewCmdResult([]interface{}{"acquired"}, nil)
		case redisIdempotencyCompleteScript:
			assert.Equal(t, "fingerprint", args[0])
			assert.Equal(t, "1", args[3])
			assert.Equal(t, "0", args[5])
			return redis.NewCmdResult(int64(1), nil)
		default:
			t.Fatalf("unexpected Redis script")
			return redis.NewCmdResult(nil, errors.New("unexpected script"))
		}
	}
	store := testRedisIdempotencyStore(client)
	response, err := store.Execute(context.Background(), "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
		calls.Add(1)
		return &protos.Response{Data: []byte("ok")}, nil
	})
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.Equal(t, []byte("ok"), response.Data)
	assert.Equal(t, int32(1), calls.Load())
}

func TestRedisIdempotencyClaimDeadlineExcludesResponseDelay(t *testing.T) {
	const responseDelay = 50 * time.Millisecond
	client := &scriptedRedisIdempotencyClient{eval: func(context.Context, string, []string, ...interface{}) *redis.Cmd {
		time.Sleep(responseDelay)
		return redis.NewCmdResult([]interface{}{"acquired"}, nil)
	}}
	store := testRedisIdempotencyStore(client)

	claim, err := store.claim(context.Background(), "scope:key", "fingerprint", "owner")
	finished := time.Now()
	require.NoError(t, err)
	require.Equal(t, redisIdempotencyClaimAcquired, claim.kind)
	assert.Less(t, claim.leaseDeadline, finished.Add(store.options.LeaseTTL-responseDelay/2))
}

func TestRedisIdempotencyStoreSanitizesClaimInfrastructureError(t *testing.T) {
	const secret = "redis://worker:claim-secret@redis.internal:6379"
	client := &scriptedRedisIdempotencyClient{eval: func(context.Context, string, []string, ...interface{}) *redis.Cmd {
		return redis.NewCmdResult(nil, errors.New(secret))
	}}
	store := testRedisIdempotencyStore(client)

	response, err := store.Execute(context.Background(), "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
		t.Fatal("operation executed without an idempotency claim")
		return nil, nil
	})

	require.Nil(t, response)
	require.ErrorIs(t, err, ErrIdempotencyStoreUnavailable)
	assert.Contains(t, err.Error(), secret)
	assertIdempotencyWireError(t, err, ErrIdempotencyStoreUnavailable, secret)
}

func TestRedisIdempotencyStorePreservesParentCancellationDuringClaim(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &scriptedRedisIdempotencyClient{eval: func(ctx context.Context, _ string, _ []string, _ ...interface{}) *redis.Cmd {
		return redis.NewCmdResult(nil, ctx.Err())
	}}
	store := testRedisIdempotencyStore(client)

	_, err := store.Execute(ctx, "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
		t.Fatal("operation executed after parent cancellation")
		return nil, nil
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrIdempotencyStoreUnavailable)
}

func TestRedisIdempotencyStoreReplaysDoneResponseWithoutExecuting(t *testing.T) {
	result, err := encodeRedisIdempotencyResult(&protos.Response{Data: []byte("cached")}, nil)
	require.NoError(t, err)
	client := &scriptedRedisIdempotencyClient{eval: func(
		_ context.Context,
		script string,
		_ []string,
		_ ...interface{},
	) *redis.Cmd {
		require.Equal(t, redisIdempotencyAcquireScript, script)
		return redis.NewCmdResult([]interface{}{"done", result.has, result.encoded, result.hadError}, nil)
	}}
	store := testRedisIdempotencyStore(client)
	response, err := store.Execute(context.Background(), "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
		t.Fatal("cached command executed")
		return nil, nil
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("cached"), response.Data)
}

func TestRedisIdempotencyStoreRejectsConflictAndCorruption(t *testing.T) {
	for _, test := range []struct {
		name  string
		state string
		err   error
	}{
		{name: "conflict", state: "conflict", err: constants.ErrIdempotencyConflict},
		{name: "corrupt", state: "corrupt", err: constants.ErrIdempotencyStoreCorrupt},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &scriptedRedisIdempotencyClient{eval: func(context.Context, string, []string, ...interface{}) *redis.Cmd {
				return redis.NewCmdResult([]interface{}{test.state}, nil)
			}}
			store := testRedisIdempotencyStore(client)
			called := false
			_, err := store.Execute(context.Background(), "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
				called = true
				return nil, nil
			})
			assert.ErrorIs(t, err, test.err)
			assert.False(t, called)
		})
	}
}

func TestRedisIdempotencyStoreWaitHonorsContext(t *testing.T) {
	client := &scriptedRedisIdempotencyClient{eval: func(context.Context, string, []string, ...interface{}) *redis.Cmd {
		return redis.NewCmdResult([]interface{}{"wait", "1000"}, nil)
	}}
	store := testRedisIdempotencyStore(client)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.Execute(ctx, "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
		t.Fatal("waiting command executed")
		return nil, nil
	})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestRedisIdempotencyStoreAbortsStaleAdmissionAfterWaitAndAllowsReclaim(t *testing.T) {
	var acquireCalls atomic.Int32
	var abortCalls atomic.Int32
	var completeCalls atomic.Int32
	var handoffDuringWait atomic.Bool
	var firstOwner string
	var reclaimedOwner string
	client := &scriptedRedisIdempotencyClient{eval: func(
		_ context.Context,
		script string,
		keys []string,
		args ...interface{},
	) *redis.Cmd {
		assert.Equal(t, []string{defaultRedisIdempotencyKeyPrefix + "scope:key"}, keys)
		switch script {
		case redisIdempotencyAcquireScript:
			call := acquireCalls.Add(1)
			owner, ok := args[1].(string)
			require.True(t, ok)
			switch call {
			case 1:
				firstOwner = owner
				handoffDuringWait.Store(true)
				return redis.NewCmdResult([]interface{}{"wait", "1"}, nil)
			case 2:
				assert.Equal(t, firstOwner, owner, "one Execute call must retain its claimant identity while polling")
				return redis.NewCmdResult([]interface{}{"acquired"}, nil)
			case 3:
				reclaimedOwner = owner
				return redis.NewCmdResult([]interface{}{"acquired"}, nil)
			default:
				t.Fatalf("unexpected acquire attempt %d", call)
				return redis.NewCmdResult(nil, errors.New("unexpected acquire"))
			}
		case redisIdempotencyAbortScript:
			abortCalls.Add(1)
			assert.Equal(t, "fingerprint", args[0])
			assert.Equal(t, firstOwner, args[1])
			return redis.NewCmdResult(int64(1), nil)
		case redisIdempotencyCompleteScript:
			completeCalls.Add(1)
			assert.Equal(t, "fingerprint", args[0])
			assert.Equal(t, reclaimedOwner, args[1])
			return redis.NewCmdResult(int64(1), nil)
		default:
			t.Fatalf("unexpected Redis script")
			return redis.NewCmdResult(nil, errors.New("unexpected script"))
		}
	}}
	store := testRedisIdempotencyStore(client)
	store.options.PollInterval = time.Millisecond

	var callbackCalls atomic.Int32
	resolverErr := errors.New("custom resolver unavailable after handoff")
	response, err := store.Execute(context.Background(), "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
		callbackCalls.Add(1)
		assert.True(t, handoffDuringWait.Load())
		rejection := markRPCAdmissionRejected(resolverErr)
		return rpcErrorResponse(rejection), rejection
	})
	require.ErrorIs(t, err, resolverErr)
	assert.True(t, IsRPCAdmissionRejected(err))
	require.NotNil(t, response)
	assert.Equal(t, int32(1), abortCalls.Load())
	assert.Zero(t, completeCalls.Load(), "a pre-handler admission rejection must not become a cached result")

	response, err = store.Execute(context.Background(), "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
		callbackCalls.Add(1)
		return &protos.Response{Data: []byte("accepted after reclaim")}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("accepted after reclaim"), response.Data)
	assert.NotEmpty(t, reclaimedOwner)
	assert.NotEqual(t, firstOwner, reclaimedOwner)
	assert.Equal(t, int32(3), acquireCalls.Load())
	assert.Equal(t, int32(1), abortCalls.Load())
	assert.Equal(t, int32(1), completeCalls.Load())
	assert.Equal(t, int32(2), callbackCalls.Load())
}

func TestRedisIdempotencyStoreReportsCommitUncertainty(t *testing.T) {
	for _, test := range []struct {
		name            string
		complete        *redis.Cmd
		expectedErr     error
		forbiddenDetail string
	}{
		{name: "transport error", complete: redis.NewCmdResult(nil, errors.New("connection lost")), expectedErr: constants.ErrIdempotencyCommitUnknown, forbiddenDetail: "connection lost"},
		{name: "owner lost", complete: redis.NewCmdResult(int64(0), nil), expectedErr: constants.ErrIdempotencyLeaseLost},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &scriptedRedisIdempotencyClient{eval: func(_ context.Context, script string, _ []string, _ ...interface{}) *redis.Cmd {
				if script == redisIdempotencyAcquireScript {
					return redis.NewCmdResult([]interface{}{"acquired"}, nil)
				}
				return test.complete
			}}
			store := testRedisIdempotencyStore(client)
			response, err := store.Execute(context.Background(), "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
				return &protos.Response{Data: []byte("performed")}, nil
			})
			require.NotNil(t, response)
			require.NotNil(t, response.Status)
			assert.Equal(t, int32(http.StatusInternalServerError), response.Status.Code)
			assert.NotEqual(t, []byte("performed"), response.Data)
			assert.ErrorIs(t, err, test.expectedErr)
			assertIdempotencyWireError(t, err, test.expectedErr, test.forbiddenDetail)
		})
	}
}

func TestRedisIdempotencyStoreSanitizesLeaseRenewalError(t *testing.T) {
	const secret = "redis TLS handshake failed for lease-secret.internal"
	var renewCalls atomic.Int32
	client := &scriptedRedisIdempotencyClient{eval: func(_ context.Context, script string, _ []string, _ ...interface{}) *redis.Cmd {
		require.Equal(t, redisIdempotencyRenewScript, script)
		renewCalls.Add(1)
		return redis.NewCmdResult(nil, errors.New(secret))
	}}
	store := testRedisIdempotencyStore(client)
	store.options.HeartbeatInterval = time.Millisecond
	executionCtx, cancelExecution := context.WithCancelCause(context.Background())

	err := store.renewLease(
		context.Background(),
		"scope:key",
		"fingerprint",
		"owner",
		time.Now().Add(200*time.Millisecond),
		cancelExecution,
	)

	require.Positive(t, renewCalls.Load())
	require.ErrorIs(t, err, constants.ErrIdempotencyLeaseLost)
	require.ErrorIs(t, context.Cause(executionCtx), constants.ErrIdempotencyLeaseLost)
	assert.Contains(t, err.Error(), secret)
	assertIdempotencyWireError(t, err, constants.ErrIdempotencyLeaseLost, secret)
}

func assertIdempotencyWireError(t *testing.T, err, expected error, forbiddenDetail string) {
	t.Helper()
	response := rpcErrorResponse(err)
	require.NotNil(t, response.Status)
	assert.Equal(t, expected.Error(), response.Status.Message)
	if forbiddenDetail != "" {
		assert.NotContains(t, response.Status.Message, forbiddenDetail)
	}
}

func TestRedisIdempotencyStorePersistsStableFailureWhenResponseEncodingFails(t *testing.T) {
	var completed *protos.Response
	var completeCalls atomic.Int32
	client := &scriptedRedisIdempotencyClient{eval: func(_ context.Context, script string, _ []string, args ...interface{}) *redis.Cmd {
		switch script {
		case redisIdempotencyAcquireScript:
			return redis.NewCmdResult([]interface{}{"acquired"}, nil)
		case redisIdempotencyCompleteScript:
			completeCalls.Add(1)
			completed = &protos.Response{}
			require.NoError(t, proto.Unmarshal(args[4].([]byte), completed))
			return redis.NewCmdResult(int64(1), nil)
		default:
			t.Fatalf("unexpected Redis script")
			return redis.NewCmdResult(nil, errors.New("unexpected script"))
		}
	}}
	store := testRedisIdempotencyStore(client)
	invalidUTF8 := string([]byte{0xff})

	response, err := store.Execute(context.Background(), "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
		return &protos.Response{Status: &apierrors.Status{
			Code:    http.StatusOK,
			Message: invalidUTF8,
		}}, nil
	})

	require.Error(t, err)
	require.NotNil(t, response)
	require.NotNil(t, response.Status)
	assert.Equal(t, int32(http.StatusInternalServerError), response.Status.Code)
	assert.NotContains(t, response.Status.Message, invalidUTF8)
	assert.Equal(t, int32(1), completeCalls.Load())
	require.NotNil(t, completed)
	require.NotNil(t, completed.Status)
	assert.True(t, proto.Equal(response.Status, completed.Status))
}

func TestRedisIdempotencyCommitFailureDominatesBusinessGRPCStatus(t *testing.T) {
	client := &scriptedRedisIdempotencyClient{eval: func(_ context.Context, script string, _ []string, _ ...interface{}) *redis.Cmd {
		if script == redisIdempotencyAcquireScript {
			return redis.NewCmdResult([]interface{}{"acquired"}, nil)
		}
		return redis.NewCmdResult(nil, errors.New("connection lost"))
	}}
	store := testRedisIdempotencyStore(client)
	businessResponse := &protos.Response{Status: &apierrors.Status{
		Code:    http.StatusBadRequest,
		Message: "business rejected command",
	}}
	businessErr := apierrors.FromStatus(businessResponse.Status)

	response, err := store.Execute(context.Background(), "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
		return businessResponse, businessErr
	})

	require.ErrorIs(t, err, constants.ErrIdempotencyCommitUnknown)
	assert.Equal(t, codes.Internal, status.Code(err))
	assert.NotContains(t, status.Convert(err).Message(), businessResponse.Status.Message)
	require.NotNil(t, response)
	require.NotNil(t, response.Status)
	assert.Equal(t, int32(http.StatusInternalServerError), response.Status.Code)
}

type redisPanicFormattingValue struct{}

func (redisPanicFormattingValue) Error() string  { panic("panic Error called") }
func (redisPanicFormattingValue) String() string { panic("panic String called") }

func TestRedisIdempotencyStorePanicCompletionDoesNotFormatPanicValue(t *testing.T) {
	var completed *protos.Response
	client := &scriptedRedisIdempotencyClient{eval: func(_ context.Context, script string, _ []string, args ...interface{}) *redis.Cmd {
		if script == redisIdempotencyAcquireScript {
			return redis.NewCmdResult([]interface{}{"acquired"}, nil)
		}
		if script == redisIdempotencyCompleteScript {
			completed = &protos.Response{}
			require.NoError(t, proto.Unmarshal(args[4].([]byte), completed))
			return redis.NewCmdResult(int64(1), nil)
		}
		t.Fatalf("unexpected Redis script")
		return redis.NewCmdResult(nil, errors.New("unexpected script"))
	}}
	store := testRedisIdempotencyStore(client)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = store.Execute(context.Background(), "scope:key", "fingerprint", func(context.Context) (*protos.Response, error) {
			panic(redisPanicFormattingValue{})
		})
	}()
	require.IsType(t, redisPanicFormattingValue{}, recovered)
	require.NotNil(t, completed)
	require.NotNil(t, completed.Status)
	assert.NotContains(t, completed.Status.Message, "panic Error called")
}
