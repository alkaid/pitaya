//go:build integration

package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	redisIntegrationURLEnv          = "PITAYA_REDIS_URL"
	redisIntegrationAddressEnv      = "PITAYA_INTEGRATION_REDIS_ADDR"
	redisIntegrationOwnerKeyEnv     = "PITAYA_INTEGRATION_REDIS_OWNER_KEY"
	redisIntegrationOwnerTokenEnv   = "PITAYA_INTEGRATION_REDIS_OWNER_TOKEN"
	redisServiceProofPathEnv        = "PITAYA_INTEGRATION_SERVICE_PROOF_PATH"
	redisServiceProofTokenEnv       = "PITAYA_INTEGRATION_SERVICE_PROOF_TOKEN"
	redisCrashHelperModeEnv         = "PITAYA_IDEMPOTENCY_CRASH_HELPER"
	redisCrashHelperPrefixEnv       = "PITAYA_IDEMPOTENCY_CRASH_PREFIX"
	redisCrashHelperKeyEnv          = "PITAYA_IDEMPOTENCY_CRASH_KEY"
	redisCrashHelperFingerprint     = "PITAYA_IDEMPOTENCY_CRASH_FINGERPRINT"
	redisCrashHelperAuxKeyEnv       = "PITAYA_IDEMPOTENCY_CRASH_AUX_KEY"
	redisCrashHelperExitCode        = 87
	redisCrashAfterAcquire          = "after-acquire"
	redisCrashAfterBusinessWrite    = "after-business-write"
	redisIntegrationOwnerMaxTTL     = 2 * time.Minute
	redisIntegrationOperationMaxTTL = 5 * time.Second
)

type redisIdempotencyIntegrationFixture struct {
	primary   *redis.Client
	secondary *redis.Client
	options   RedisIdempotencyStoreOptions
	t         *testing.T
	ownerKey  string
	keys      []string
}

type redisIdempotencyExecutionResult struct {
	response *protos.Response
	err      error
}

type redisIdempotencyRenewFailingClient struct {
	redis.Cmdable
}

func (c *redisIdempotencyRenewFailingClient) Eval(
	ctx context.Context,
	script string,
	keys []string,
	args ...interface{},
) *redis.Cmd {
	if script == redisIdempotencyRenewScript {
		return redis.NewCmdResult(nil, fmt.Errorf("simulated lease renewal failure"))
	}
	return c.Cmdable.Eval(ctx, script, keys, args...)
}

func TestRedisIdempotencyStoreRedisIntegration(t *testing.T) {
	if mode := os.Getenv(redisCrashHelperModeEnv); mode != "" {
		runRedisIdempotencyCrashHelper(mode)
		return
	}

	if redisURL, ok := os.LookupEnv(redisIntegrationURLEnv); !ok || redisURL == "" {
		t.Skipf("%s is not set", redisIntegrationURLEnv)
	}
	address := redisIdempotencyRequiredIntegrationEnv(t, redisIntegrationAddressEnv)
	requireRedisIdempotencyLiteralLoopbackAddress(t, address)
	ownerKey := redisIdempotencyRequiredIntegrationEnv(t, redisIntegrationOwnerKeyEnv)
	ownerToken := redisIdempotencyRequiredIntegrationEnv(t, redisIntegrationOwnerTokenEnv)
	proofPath := redisIdempotencyRequiredIntegrationEnv(t, redisServiceProofPathEnv)
	proofToken := redisIdempotencyRequiredIntegrationEnv(t, redisServiceProofTokenEnv)
	registerRedisIdempotencyIntegrationProof(t, proofPath, proofToken)

	verificationClient := newRedisIdempotencyIntegrationClient(t, address)
	t.Cleanup(func() {
		if err := verificationClient.Close(); err != nil {
			t.Errorf("close Redis ownership verification client: %v", err)
		}
	})
	verifyRedisIdempotencyIntegrationOwner(t, verificationClient, ownerKey, ownerToken)

	t.Run("concurrent stores execute once and replay", func(t *testing.T) {
		fixture := newRedisIdempotencyIntegrationFixture(t, address, ownerKey)
		const logicalKey = "concurrent"
		fixture.trackLogicalKeys(logicalKey)
		first := fixture.store(t, fixture.primary)
		second := fixture.store(t, fixture.secondary)

		start := make(chan struct{})
		release := make(chan struct{})
		var calls atomic.Int32
		results := make(chan redisIdempotencyExecutionResult, 2)
		execute := func(store *RedisIdempotencyStore) {
			<-start
			response, err := store.Execute(context.Background(), logicalKey, "fingerprint", func(context.Context) (*protos.Response, error) {
				calls.Add(1)
				<-release
				return &protos.Response{Data: []byte("committed")}, nil
			})
			results <- redisIdempotencyExecutionResult{response: response, err: err}
		}
		go execute(first)
		go execute(second)
		close(start)

		require.Eventually(t, func() bool { return calls.Load() >= 1 }, 2*time.Second, 10*time.Millisecond)
		assert.Never(t, func() bool { return calls.Load() > 1 }, 150*time.Millisecond, 10*time.Millisecond)
		close(release)
		for range 2 {
			result := receiveRedisIdempotencyResult(t, results)
			require.NoError(t, result.err)
			require.NotNil(t, result.response)
			assert.Equal(t, []byte("committed"), result.response.Data)
		}
		assert.Equal(t, int32(1), calls.Load())

		replayed, err := second.Execute(context.Background(), logicalKey, "fingerprint", func(context.Context) (*protos.Response, error) {
			t.Fatal("completed command executed during replay")
			return nil, nil
		})
		require.NoError(t, err)
		require.NotNil(t, replayed)
		assert.Equal(t, []byte("committed"), replayed.Data)
	})

	t.Run("fingerprint conflict", func(t *testing.T) {
		fixture := newRedisIdempotencyIntegrationFixture(t, address, ownerKey)
		const logicalKey = "conflict"
		fixture.trackLogicalKeys(logicalKey)
		store := fixture.store(t, fixture.primary)

		_, err := store.Execute(context.Background(), logicalKey, "fingerprint-a", func(context.Context) (*protos.Response, error) {
			return &protos.Response{Data: []byte("first")}, nil
		})
		require.NoError(t, err)

		called := false
		_, err = fixture.store(t, fixture.secondary).Execute(context.Background(), logicalKey, "fingerprint-b", func(context.Context) (*protos.Response, error) {
			called = true
			return nil, nil
		})
		assert.ErrorIs(t, err, constants.ErrIdempotencyConflict)
		assert.False(t, called)
	})

	t.Run("heartbeat prevents takeover past lease", func(t *testing.T) {
		fixture := newRedisIdempotencyIntegrationFixture(t, address, ownerKey)
		const logicalKey = "heartbeat"
		fixture.trackLogicalKeys(logicalKey)
		owner := fixture.store(t, fixture.primary)
		waiter := fixture.store(t, fixture.secondary)

		ownerStarted := make(chan struct{})
		ownerRelease := make(chan struct{})
		var releaseOnce sync.Once
		releaseOwner := func() { releaseOnce.Do(func() { close(ownerRelease) }) }
		defer releaseOwner()
		ownerResult := make(chan redisIdempotencyExecutionResult, 1)
		go func() {
			response, err := owner.Execute(context.Background(), logicalKey, "fingerprint", func(ctx context.Context) (*protos.Response, error) {
				close(ownerStarted)
				select {
				case <-ownerRelease:
					return &protos.Response{Data: []byte("owner")}, nil
				case <-ctx.Done():
					return nil, context.Cause(ctx)
				}
			})
			ownerResult <- redisIdempotencyExecutionResult{response: response, err: err}
		}()
		requireChannelClosed(t, ownerStarted, 2*time.Second, "owner did not acquire the Redis lease")

		waiterCalled := make(chan struct{}, 1)
		waiterResult := make(chan redisIdempotencyExecutionResult, 1)
		go func() {
			response, err := waiter.Execute(context.Background(), logicalKey, "fingerprint", func(context.Context) (*protos.Response, error) {
				waiterCalled <- struct{}{}
				return &protos.Response{Data: []byte("takeover")}, nil
			})
			waiterResult <- redisIdempotencyExecutionResult{response: response, err: err}
		}()

		hold := time.NewTimer(fixture.options.LeaseTTL + fixture.options.LeaseTTL/2)
		select {
		case <-waiterCalled:
			t.Fatal("second store took over despite live heartbeats")
		case result := <-ownerResult:
			t.Fatalf("owner returned before release: %v", result.err)
		case result := <-waiterResult:
			t.Fatalf("waiter returned while owner was running: %v", result.err)
		case <-hold.C:
		}

		releaseOwner()
		for _, resultChannel := range []<-chan redisIdempotencyExecutionResult{ownerResult, waiterResult} {
			result := receiveRedisIdempotencyResult(t, resultChannel)
			require.NoError(t, result.err)
			require.NotNil(t, result.response)
			assert.Equal(t, []byte("owner"), result.response.Data)
		}
		select {
		case <-waiterCalled:
			t.Fatal("waiter callback executed after owner completion")
		default:
		}
	})

	t.Run("waiter cancellation does not cancel owner", func(t *testing.T) {
		fixture := newRedisIdempotencyIntegrationFixture(t, address, ownerKey)
		const logicalKey = "waiter-cancel"
		fixture.trackLogicalKeys(logicalKey)
		owner := fixture.store(t, fixture.primary)
		waiter := fixture.store(t, fixture.secondary)

		ownerStarted := make(chan context.Context, 1)
		ownerRelease := make(chan struct{})
		var releaseOnce sync.Once
		releaseOwner := func() { releaseOnce.Do(func() { close(ownerRelease) }) }
		defer releaseOwner()
		ownerResult := make(chan redisIdempotencyExecutionResult, 1)
		go func() {
			response, err := owner.Execute(context.Background(), logicalKey, "fingerprint", func(ctx context.Context) (*protos.Response, error) {
				ownerStarted <- ctx
				select {
				case <-ownerRelease:
					return &protos.Response{Data: []byte("owner")}, nil
				case <-ctx.Done():
					return nil, context.Cause(ctx)
				}
			})
			ownerResult <- redisIdempotencyExecutionResult{response: response, err: err}
		}()

		var ownerExecutionCtx context.Context
		select {
		case ownerExecutionCtx = <-ownerStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("owner did not acquire the Redis lease")
		}

		waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), fixture.options.OperationTimeout/2)
		defer cancelWaiter()
		var waiterCalls atomic.Int32
		_, err := waiter.Execute(waiterCtx, logicalKey, "fingerprint", func(context.Context) (*protos.Response, error) {
			waiterCalls.Add(1)
			return nil, nil
		})
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Zero(t, waiterCalls.Load())
		select {
		case <-ownerExecutionCtx.Done():
			t.Fatalf("waiter cancellation propagated to owner: %v", context.Cause(ownerExecutionCtx))
		default:
		}

		releaseOwner()
		result := receiveRedisIdempotencyResult(t, ownerResult)
		require.NoError(t, result.err)
		require.NotNil(t, result.response)
		assert.Equal(t, []byte("owner"), result.response.Data)
	})

	t.Run("live stale callback overlaps takeover without overwriting", func(t *testing.T) {
		fixture := newRedisIdempotencyIntegrationFixture(t, address, ownerKey)
		const logicalKey = "live-stale-callback"
		fixture.trackLogicalKeys(logicalKey)

		owner, err := NewRedisIdempotencyStore(
			&redisIdempotencyRenewFailingClient{Cmdable: fixture.primary},
			fixture.options,
		)
		require.NoError(t, err)
		takeover := fixture.store(t, fixture.secondary)

		ownerStarted := make(chan context.Context, 1)
		ownerRelease := make(chan struct{})
		var ownerReleaseOnce sync.Once
		releaseOwner := func() { ownerReleaseOnce.Do(func() { close(ownerRelease) }) }
		defer releaseOwner()
		ownerResult := make(chan redisIdempotencyExecutionResult, 1)
		go func() {
			response, executeErr := owner.Execute(context.Background(), logicalKey, "fingerprint", func(ctx context.Context) (*protos.Response, error) {
				ownerStarted <- ctx
				<-ownerRelease // Deliberately ignore cooperative lease-loss cancellation.
				return &protos.Response{Data: []byte("stale-owner")}, nil
			})
			ownerResult <- redisIdempotencyExecutionResult{response: response, err: executeErr}
		}()

		var ownerExecutionCtx context.Context
		select {
		case ownerExecutionCtx = <-ownerStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("owner did not acquire the Redis lease")
		}

		takeoverStarted := make(chan struct{})
		takeoverRelease := make(chan struct{})
		var takeoverReleaseOnce sync.Once
		releaseTakeover := func() { takeoverReleaseOnce.Do(func() { close(takeoverRelease) }) }
		defer releaseTakeover()
		takeoverResult := make(chan redisIdempotencyExecutionResult, 1)
		go func() {
			response, executeErr := takeover.Execute(context.Background(), logicalKey, "fingerprint", func(context.Context) (*protos.Response, error) {
				close(takeoverStarted)
				<-takeoverRelease
				return &protos.Response{Data: []byte("new-owner")}, nil
			})
			takeoverResult <- redisIdempotencyExecutionResult{response: response, err: executeErr}
		}()

		requireChannelClosed(t, takeoverStarted, 3*time.Second, "takeover callback did not start after the Redis lease expired")
		require.Eventually(t, func() bool {
			return errors.Is(ownerExecutionCtx.Err(), context.Canceled) &&
				errors.Is(context.Cause(ownerExecutionCtx), constants.ErrIdempotencyLeaseLost)
		}, 2*time.Second, 10*time.Millisecond, "lease loss did not cooperatively cancel the stale callback context")
		select {
		case result := <-ownerResult:
			t.Fatalf("stale callback returned before its explicit release: %v", result.err)
		default:
		}

		releaseTakeover()
		newResult := receiveRedisIdempotencyResult(t, takeoverResult)
		require.NoError(t, newResult.err)
		require.NotNil(t, newResult.response)
		assert.Equal(t, []byte("new-owner"), newResult.response.Data)

		releaseOwner()
		staleResult := receiveRedisIdempotencyResult(t, ownerResult)
		assert.ErrorIs(t, staleResult.err, constants.ErrIdempotencyLeaseLost)
		require.NotNil(t, staleResult.response)
		require.NotNil(t, staleResult.response.Status)
		assert.NotEqual(t, []byte("stale-owner"), staleResult.response.Data)

		replayed, err := fixture.store(t, fixture.primary).Execute(context.Background(), logicalKey, "fingerprint", func(context.Context) (*protos.Response, error) {
			t.Fatal("stale owner overwrote the takeover result")
			return nil, nil
		})
		require.NoError(t, err)
		require.NotNil(t, replayed)
		assert.Equal(t, []byte("new-owner"), replayed.Data)
	})

	t.Run("result TTL permits later execution", func(t *testing.T) {
		fixture := newRedisIdempotencyIntegrationFixture(t, address, ownerKey)
		fixture.options.ResultTTL = 250 * time.Millisecond
		const logicalKey = "result-ttl"
		fixture.trackLogicalKeys(logicalKey)
		store := fixture.store(t, fixture.primary)
		var calls atomic.Int32

		response, err := store.Execute(context.Background(), logicalKey, "fingerprint", func(context.Context) (*protos.Response, error) {
			call := calls.Add(1)
			return &protos.Response{Data: []byte(fmt.Sprintf("result-%d", call))}, nil
		})
		require.NoError(t, err)
		assert.Equal(t, []byte("result-1"), response.Data)

		redisKey := fixture.redisKey(logicalKey)
		require.Eventually(t, func() bool {
			exists, existsErr := fixture.primary.Exists(context.Background(), redisKey).Result()
			return existsErr == nil && exists == 0
		}, 3*time.Second, 20*time.Millisecond, "completed idempotency result did not expire")

		response, err = store.Execute(context.Background(), logicalKey, "fingerprint", func(context.Context) (*protos.Response, error) {
			call := calls.Add(1)
			return &protos.Response{Data: []byte(fmt.Sprintf("result-%d", call))}, nil
		})
		require.NoError(t, err)
		assert.Equal(t, []byte("result-2"), response.Data)
		assert.Equal(t, int32(2), calls.Load())
	})

	t.Run("stale owner cannot complete after takeover", func(t *testing.T) {
		fixture := newRedisIdempotencyIntegrationFixture(t, address, ownerKey)
		const logicalKey = "stale-owner"
		fixture.trackLogicalKeys(logicalKey)
		redisKey := fixture.redisKey(logicalKey)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		state := evalRedisIdempotencyClaim(t, ctx, fixture.primary, redisKey, "fingerprint", "old-owner", 200*time.Millisecond)
		assert.Equal(t, "acquired", state)
		require.Eventually(t, func() bool {
			exists, err := fixture.primary.Exists(ctx, redisKey).Result()
			return err == nil && exists == 0
		}, 3*time.Second, 20*time.Millisecond, "old owner lease did not expire")

		state = evalRedisIdempotencyClaim(t, ctx, fixture.secondary, redisKey, "fingerprint", "new-owner", 2*time.Second)
		assert.Equal(t, "acquired", state)
		oldResult, err := encodeRedisIdempotencyResult(&protos.Response{Data: []byte("stale")}, nil)
		require.NoError(t, err)
		completed, err := fixture.primary.Eval(
			ctx,
			redisIdempotencyCompleteScript,
			[]string{redisKey},
			"fingerprint",
			"old-owner",
			durationMillis(2*time.Second),
			oldResult.has,
			oldResult.encoded,
			oldResult.hadError,
		).Int64()
		require.NoError(t, err)
		assert.Zero(t, completed)

		fields, err := fixture.primary.HGetAll(ctx, redisKey).Result()
		require.NoError(t, err)
		assert.Equal(t, "running", fields["state"])
		assert.Equal(t, "new-owner", fields["owner"])
		assert.NotContains(t, fields, "response")

		newResult, err := encodeRedisIdempotencyResult(&protos.Response{Data: []byte("new-owner")}, nil)
		require.NoError(t, err)
		completed, err = fixture.secondary.Eval(
			ctx,
			redisIdempotencyCompleteScript,
			[]string{redisKey},
			"fingerprint",
			"new-owner",
			durationMillis(fixture.options.ResultTTL),
			newResult.has,
			newResult.encoded,
			newResult.hadError,
		).Int64()
		require.NoError(t, err)
		assert.Equal(t, int64(1), completed)

		replayed, err := fixture.store(t, fixture.primary).Execute(context.Background(), logicalKey, "fingerprint", func(context.Context) (*protos.Response, error) {
			t.Fatal("new owner's completed result was not replayed")
			return nil, nil
		})
		require.NoError(t, err)
		require.NotNil(t, replayed)
		assert.Equal(t, []byte("new-owner"), replayed.Data)
	})

	t.Run("process crash after acquire permits lease takeover", func(t *testing.T) {
		fixture := newRedisIdempotencyIntegrationFixture(t, address, ownerKey)
		const logicalKey = "crash-after-acquire"
		markerKey := fixture.options.KeyPrefix + "marker"
		fixture.trackLogicalKeys(logicalKey)
		fixture.trackRedisKeys(markerKey)

		runRedisIdempotencyCrashProcess(t, address, ownerKey, ownerToken, redisCrashAfterAcquire, fixture.options.KeyPrefix, logicalKey, "fingerprint", markerKey)
		marker, err := fixture.primary.Get(context.Background(), markerKey).Result()
		require.NoError(t, err)
		assert.Equal(t, "acquired", marker)
		fields, err := fixture.primary.HGetAll(context.Background(), fixture.redisKey(logicalKey)).Result()
		require.NoError(t, err)
		assert.Equal(t, "running", fields["state"])

		var recoveryCalls atomic.Int32
		response, err := fixture.store(t, fixture.secondary).Execute(context.Background(), logicalKey, "fingerprint", func(context.Context) (*protos.Response, error) {
			recoveryCalls.Add(1)
			return &protos.Response{Data: []byte("recovered")}, nil
		})
		require.NoError(t, err)
		require.NotNil(t, response)
		assert.Equal(t, []byte("recovered"), response.Data)
		assert.Equal(t, int32(1), recoveryCalls.Load())

		replayed, err := fixture.store(t, fixture.primary).Execute(context.Background(), logicalKey, "fingerprint", func(context.Context) (*protos.Response, error) {
			t.Fatal("recovered command executed during replay")
			return nil, nil
		})
		require.NoError(t, err)
		assert.Equal(t, []byte("recovered"), replayed.Data)
	})

	t.Run("crash after business write exposes at least once window", func(t *testing.T) {
		fixture := newRedisIdempotencyIntegrationFixture(t, address, ownerKey)
		const logicalKey = "crash-after-business-write"
		businessKey := fixture.options.KeyPrefix + "business-counter"
		fixture.trackLogicalKeys(logicalKey)
		fixture.trackRedisKeys(businessKey)

		runRedisIdempotencyCrashProcess(t, address, ownerKey, ownerToken, redisCrashAfterBusinessWrite, fixture.options.KeyPrefix, logicalKey, "fingerprint", businessKey)
		count, err := fixture.primary.Get(context.Background(), businessKey).Int64()
		require.NoError(t, err)
		assert.Equal(t, int64(1), count, "crashed process must commit the simulated business effect")

		response, err := fixture.store(t, fixture.secondary).Execute(context.Background(), logicalKey, "fingerprint", func(context.Context) (*protos.Response, error) {
			updated, incrementErr := fixture.secondary.Incr(context.Background(), businessKey).Result()
			if incrementErr != nil {
				return nil, incrementErr
			}
			return &protos.Response{Data: []byte(fmt.Sprintf("business-count-%d", updated))}, nil
		})
		require.NoError(t, err)
		require.NotNil(t, response)
		assert.Equal(t, []byte("business-count-2"), response.Data)
		count, err = fixture.primary.Get(context.Background(), businessKey).Int64()
		require.NoError(t, err)
		assert.Equal(t, int64(2), count, "business effect is at-least-once across the commit crash window")

		_, err = fixture.store(t, fixture.primary).Execute(context.Background(), logicalKey, "fingerprint", func(context.Context) (*protos.Response, error) {
			t.Fatal("completed recovery executed during replay")
			return nil, nil
		})
		require.NoError(t, err)
		count, err = fixture.primary.Get(context.Background(), businessKey).Int64()
		require.NoError(t, err)
		assert.Equal(t, int64(2), count)
	})
}

func redisIdempotencyRequiredIntegrationEnv(t *testing.T, name string) string {
	t.Helper()
	value, ok := os.LookupEnv(name)
	require.Truef(t, ok, "%s must be set by the integration harness", name)
	require.NotEmptyf(t, value, "%s must not be empty", name)
	return value
}

func requireRedisIdempotencyLiteralLoopbackAddress(t *testing.T, address string) {
	t.Helper()
	require.NoError(t, validateRedisIdempotencyLiteralLoopbackAddress(address))
}

func validateRedisIdempotencyLiteralLoopbackAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("Redis address must be a literal loopback host:port: %w", err)
	}
	if host != "127.0.0.1" {
		return fmt.Errorf("Redis address must use literal IPv4 loopback")
	}
	if portText == "" || portText == "0" || portText[0] == '0' {
		return fmt.Errorf("Redis port must be canonical and non-zero")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("Redis port must be in the range 1-65535")
	}
	if address != net.JoinHostPort(host, strconv.Itoa(port)) {
		return fmt.Errorf("Redis address must use canonical host:port form")
	}
	return nil
}

func registerRedisIdempotencyIntegrationProof(t *testing.T, path, token string) {
	t.Helper()
	require.True(t, filepath.IsAbs(path), "service integration proof path must be absolute")
	_, err := os.Lstat(path)
	require.True(t, os.IsNotExist(err), "service integration proof path must not already exist")

	t.Cleanup(func() {
		if t.Failed() || t.Skipped() {
			return
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Errorf("create service integration proof: %v", err)
			return
		}
		if _, err := fmt.Fprintln(file, token); err != nil {
			t.Errorf("write service integration proof: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Errorf("close service integration proof: %v", err)
		}
	})
}

func verifyRedisIdempotencyIntegrationOwner(t *testing.T, client *redis.Client, ownerKey, ownerToken string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	actualOwnerToken, err := client.Get(ctx, ownerKey).Result()
	require.NoError(t, err, "read Redis ownership sentinel")
	require.Equal(t, ownerToken, actualOwnerToken, "Redis ownership sentinel mismatch")
	ownerTTL, err := client.PTTL(ctx, ownerKey).Result()
	require.NoError(t, err)
	require.Greater(t, ownerTTL, time.Duration(0), "Redis ownership sentinel must be short-lived")
	require.LessOrEqual(t, ownerTTL, redisIntegrationOwnerMaxTTL)
}

func newRedisIdempotencyIntegrationFixture(t *testing.T, address, ownerKey string) *redisIdempotencyIntegrationFixture {
	t.Helper()
	fixture := &redisIdempotencyIntegrationFixture{
		primary:   newRedisIdempotencyIntegrationClient(t, address),
		secondary: newRedisIdempotencyIntegrationClient(t, address),
		options:   redisIdempotencyIntegrationOptions("pitaya:test:rpc-idempotency:" + uuid.NewString() + ":"),
		t:         t,
		ownerKey:  ownerKey,
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if len(fixture.keys) > 0 {
			if err := fixture.primary.Del(cleanupCtx, fixture.keys...).Err(); err != nil {
				t.Errorf("delete exact Redis integration keys: %v", err)
			}
		}
		if err := fixture.secondary.Close(); err != nil {
			t.Errorf("close secondary Redis client: %v", err)
		}
		if err := fixture.primary.Close(); err != nil {
			t.Errorf("close primary Redis client: %v", err)
		}
	})
	return fixture
}

func newRedisIdempotencyIntegrationClient(t *testing.T, address string) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{
		Addr:            address,
		MaxRetries:      -1,
		MinRetryBackoff: -1,
		MaxRetryBackoff: -1,
		DialTimeout:     2 * time.Second,
		ReadTimeout:     2 * time.Second,
		WriteTimeout:    2 * time.Second,
		PoolTimeout:     2 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, client.Ping(ctx).Err(), "ping harness-owned loopback Redis")
	return client
}

func redisIdempotencyIntegrationOptions(prefix string) RedisIdempotencyStoreOptions {
	return RedisIdempotencyStoreOptions{
		KeyPrefix:         prefix,
		ResultTTL:         redisIntegrationOperationMaxTTL,
		LeaseTTL:          800 * time.Millisecond,
		HeartbeatInterval: 100 * time.Millisecond,
		PollInterval:      10 * time.Millisecond,
		OperationTimeout:  200 * time.Millisecond,
	}
}

func (f *redisIdempotencyIntegrationFixture) store(t *testing.T, client *redis.Client) *RedisIdempotencyStore {
	t.Helper()
	store, err := NewRedisIdempotencyStore(client, f.options)
	require.NoError(t, err)
	return store
}

func (f *redisIdempotencyIntegrationFixture) redisKey(logicalKey string) string {
	return f.options.KeyPrefix + logicalKey
}

func (f *redisIdempotencyIntegrationFixture) trackLogicalKeys(logicalKeys ...string) {
	redisKeys := make([]string, 0, len(logicalKeys))
	for _, logicalKey := range logicalKeys {
		redisKeys = append(redisKeys, f.redisKey(logicalKey))
	}
	f.trackRedisKeys(redisKeys...)
}

func (f *redisIdempotencyIntegrationFixture) trackRedisKeys(keys ...string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, key := range keys {
		require.NotEmpty(f.t, key)
		require.NotEqual(f.t, f.ownerKey, key, "test key must not overlap the Redis ownership sentinel")
		exists, err := f.primary.Exists(ctx, key).Result()
		require.NoError(f.t, err)
		require.Zero(f.t, exists, "unique Redis integration key must not pre-exist")
		f.keys = append(f.keys, key)
	}
}

func receiveRedisIdempotencyResult(t *testing.T, results <-chan redisIdempotencyExecutionResult) redisIdempotencyExecutionResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Redis idempotency execution")
		return redisIdempotencyExecutionResult{}
	}
}

func requireChannelClosed(t *testing.T, channel <-chan struct{}, timeout time.Duration, message string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(timeout):
		t.Fatal(message)
	}
}

func evalRedisIdempotencyClaim(
	t *testing.T,
	ctx context.Context,
	client *redis.Client,
	redisKey string,
	fingerprint string,
	owner string,
	leaseTTL time.Duration,
) string {
	t.Helper()
	values, err := client.Eval(
		ctx,
		redisIdempotencyAcquireScript,
		[]string{redisKey},
		fingerprint,
		owner,
		durationMillis(leaseTTL),
	).Slice()
	require.NoError(t, err)
	require.Len(t, values, 1)
	state, err := redisIdempotencyString(values[0])
	require.NoError(t, err)
	return state
}

func runRedisIdempotencyCrashProcess(
	t *testing.T,
	address string,
	ownerKey string,
	ownerToken string,
	mode string,
	prefix string,
	logicalKey string,
	fingerprint string,
	auxKey string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRedisIdempotencyStoreRedisIntegration$", "-test.count=1")
	command.Env = append(
		os.Environ(),
		redisIntegrationAddressEnv+"="+address,
		redisIntegrationOwnerKeyEnv+"="+ownerKey,
		redisIntegrationOwnerTokenEnv+"="+ownerToken,
		redisCrashHelperModeEnv+"="+mode,
		redisCrashHelperPrefixEnv+"="+prefix,
		redisCrashHelperKeyEnv+"="+logicalKey,
		redisCrashHelperFingerprint+"="+fingerprint,
		redisCrashHelperAuxKeyEnv+"="+auxKey,
	)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAsf(t, err, &exitErr, "crash helper unexpectedly succeeded: %s", output)
	require.Equalf(t, redisCrashHelperExitCode, exitErr.ExitCode(), "crash helper failed before abrupt exit: %s", output)
}

func runRedisIdempotencyCrashHelper(mode string) {
	address := os.Getenv(redisIntegrationAddressEnv)
	if err := validateRedisIdempotencyLiteralLoopbackAddress(address); err != nil {
		redisIdempotencyCrashHelperFailure(err.Error())
	}
	client := redis.NewClient(&redis.Options{
		Addr:         address,
		MaxRetries:   -1,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	ownerKey := os.Getenv(redisIntegrationOwnerKeyEnv)
	ownerToken := os.Getenv(redisIntegrationOwnerTokenEnv)
	if ownerKey == "" || ownerToken == "" {
		redisIdempotencyCrashHelperFailure("missing Redis ownership sentinel input")
	}
	verificationCtx, cancelVerification := context.WithTimeout(context.Background(), 2*time.Second)
	actualOwnerToken, err := client.Get(verificationCtx, ownerKey).Result()
	if err != nil || actualOwnerToken != ownerToken {
		cancelVerification()
		redisIdempotencyCrashHelperFailure("Redis ownership sentinel mismatch")
	}
	ownerTTL, err := client.PTTL(verificationCtx, ownerKey).Result()
	cancelVerification()
	if err != nil || ownerTTL <= 0 || ownerTTL > redisIntegrationOwnerMaxTTL {
		redisIdempotencyCrashHelperFailure("Redis ownership sentinel TTL is invalid")
	}
	storeOptions := redisIdempotencyIntegrationOptions(os.Getenv(redisCrashHelperPrefixEnv))
	store, err := NewRedisIdempotencyStore(client, storeOptions)
	if err != nil {
		redisIdempotencyCrashHelperFailure("invalid idempotency store options")
	}
	logicalKey := os.Getenv(redisCrashHelperKeyEnv)
	fingerprint := os.Getenv(redisCrashHelperFingerprint)
	auxKey := os.Getenv(redisCrashHelperAuxKeyEnv)
	if logicalKey == "" || fingerprint == "" || auxKey == "" {
		redisIdempotencyCrashHelperFailure("missing crash helper input")
	}

	_, _ = store.Execute(context.Background(), logicalKey, fingerprint, func(context.Context) (*protos.Response, error) {
		opCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		switch mode {
		case redisCrashAfterAcquire:
			if err := client.Set(opCtx, auxKey, "acquired", redisIntegrationOperationMaxTTL).Err(); err != nil {
				redisIdempotencyCrashHelperFailure("failed to publish acquisition marker")
			}
		case redisCrashAfterBusinessWrite:
			if err := client.Incr(opCtx, auxKey).Err(); err != nil {
				redisIdempotencyCrashHelperFailure("failed to commit simulated business effect")
			}
		default:
			redisIdempotencyCrashHelperFailure("unknown crash helper mode")
		}
		os.Exit(redisCrashHelperExitCode)
		return nil, nil
	})
	redisIdempotencyCrashHelperFailure("idempotency execution returned before crash")
}

func redisIdempotencyCrashHelperFailure(message string) {
	_, _ = fmt.Fprintln(os.Stderr, message)
	os.Exit(2)
}
