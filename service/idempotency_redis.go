package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alkaid/goerrors/apierrors"
	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/protos"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

const (
	defaultRedisIdempotencyKeyPrefix = "pitaya:rpc:idempotency:"
	redisIdempotencySchemaVersion    = "1"
)

const redisIdempotencyAcquireScript = `
local key = KEYS[1]
local fingerprint = ARGV[1]
local owner = ARGV[2]
local lease_ttl = tonumber(ARGV[3])
if not lease_ttl or lease_ttl <= 0 then
	return redis.error_reply("ERR idempotency lease TTL must be positive")
end
if redis.call("EXISTS", key) == 0 then
	redis.call("HSET", key,
		"schema", "1",
		"fingerprint", fingerprint,
		"state", "running",
		"owner", owner)
	redis.call("PEXPIRE", key, lease_ttl)
	return {"acquired"}
end
local schema = redis.call("HGET", key, "schema")
local stored_fingerprint = redis.call("HGET", key, "fingerprint")
local state = redis.call("HGET", key, "state")
if schema ~= "1" or not stored_fingerprint or not state then
	return {"corrupt"}
end
if stored_fingerprint ~= fingerprint then
	return {"conflict"}
end
if state == "done" then
	local has_response = redis.call("HGET", key, "has_response")
	local response = redis.call("HGET", key, "response")
	local had_error = redis.call("HGET", key, "had_error")
	if not has_response or not had_error then
		return {"corrupt"}
	end
	if has_response == "1" and not response then
		return {"corrupt"}
	end
	return {"done", has_response, response or "", had_error}
end
if state ~= "running" or not redis.call("HGET", key, "owner") then
	return {"corrupt"}
end
local ttl = redis.call("PTTL", key)
if ttl < 0 then
	return {"corrupt"}
end
return {"wait", tostring(ttl)}
`

const redisIdempotencyRenewScript = `
local lease_ttl = tonumber(ARGV[3])
if not lease_ttl or lease_ttl <= 0 then
	return redis.error_reply("ERR idempotency lease TTL must be positive")
end
if redis.call("HGET", KEYS[1], "schema") ~= "1"
	or redis.call("HGET", KEYS[1], "fingerprint") ~= ARGV[1]
	or redis.call("HGET", KEYS[1], "state") ~= "running"
	or redis.call("HGET", KEYS[1], "owner") ~= ARGV[2] then
	return 0
end
redis.call("PEXPIRE", KEYS[1], lease_ttl)
return 1
`

const redisIdempotencyCompleteScript = `
local result_ttl = tonumber(ARGV[3])
if not result_ttl or result_ttl <= 0 then
	return redis.error_reply("ERR idempotency result TTL must be positive")
end
if redis.call("HGET", KEYS[1], "schema") ~= "1"
	or redis.call("HGET", KEYS[1], "fingerprint") ~= ARGV[1]
	or redis.call("HGET", KEYS[1], "state") ~= "running"
	or redis.call("HGET", KEYS[1], "owner") ~= ARGV[2] then
	return 0
end
redis.call("HSET", KEYS[1],
	"state", "done",
	"has_response", ARGV[4],
	"response", ARGV[5],
	"had_error", ARGV[6])
redis.call("HDEL", KEYS[1], "owner")
redis.call("PEXPIRE", KEYS[1], result_ttl)
return 1
`

// RedisIdempotencyStoreOptions controls the durable RPC command state machine.
// HeartbeatInterval plus OperationTimeout must remain below LeaseTTL so a slow
// Redis operation cannot silently renew after the local lease deadline.
type RedisIdempotencyStoreOptions struct {
	KeyPrefix         string
	ResultTTL         time.Duration
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
	PollInterval      time.Duration
	OperationTimeout  time.Duration
}

func DefaultRedisIdempotencyStoreOptions() RedisIdempotencyStoreOptions {
	return RedisIdempotencyStoreOptions{
		KeyPrefix:         defaultRedisIdempotencyKeyPrefix,
		ResultTTL:         15 * time.Minute,
		LeaseTTL:          30 * time.Second,
		HeartbeatInterval: 10 * time.Second,
		PollInterval:      25 * time.Millisecond,
		OperationTimeout:  3 * time.Second,
	}
}

func (o RedisIdempotencyStoreOptions) validate() error {
	var errs []error
	if strings.TrimSpace(o.KeyPrefix) == "" {
		errs = append(errs, errors.New("idempotency Redis key prefix must not be empty"))
	}
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{name: "result TTL", value: o.ResultTTL},
		{name: "lease TTL", value: o.LeaseTTL},
		{name: "heartbeat interval", value: o.HeartbeatInterval},
		{name: "poll interval", value: o.PollInterval},
		{name: "operation timeout", value: o.OperationTimeout},
	} {
		if field.value <= 0 {
			errs = append(errs, fmt.Errorf("idempotency Redis %s must be positive, got %s", field.name, field.value))
		}
	}
	if o.HeartbeatInterval > 0 && o.OperationTimeout > 0 && o.LeaseTTL > 0 &&
		o.HeartbeatInterval+o.OperationTimeout >= o.LeaseTTL {
		errs = append(errs, errors.New("idempotency Redis heartbeat interval plus operation timeout must be less than lease TTL"))
	}
	return errors.Join(errs...)
}

type redisIdempotencyCommands interface {
	Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd
}

// RedisIdempotencyStore persists command ownership and completed responses in
// Redis. It deduplicates across processes, but cannot atomically commit effects
// in an unrelated business database. Lease expiry cancels fn cooperatively; a
// callback that does not stop can overlap its successor. Callers still need
// business-level idempotency for that case and the crash window between fn and
// the Redis completion CAS.
type RedisIdempotencyStore struct {
	client  redisIdempotencyCommands
	options RedisIdempotencyStoreOptions
}

func NewRedisIdempotencyStore(client redis.Cmdable, options RedisIdempotencyStoreOptions) (*RedisIdempotencyStore, error) {
	if client == nil {
		return nil, errors.New("idempotency Redis client must not be nil")
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	return &RedisIdempotencyStore{client: client, options: options}, nil
}

type redisIdempotencyClaimKind uint8

const (
	redisIdempotencyClaimAcquired redisIdempotencyClaimKind = iota + 1
	redisIdempotencyClaimWait
	redisIdempotencyClaimDone
)

type redisIdempotencyClaim struct {
	kind          redisIdempotencyClaimKind
	waitFor       time.Duration
	leaseDeadline time.Time
	response      *protos.Response
	err           error
}

func (s *RedisIdempotencyStore) Execute(
	ctx context.Context,
	key string,
	fingerprint string,
	fn func(context.Context) (*protos.Response, error),
) (*protos.Response, error) {
	if key == "" || len(key) > MaxIdempotencyKeyBytes {
		return nil, fmt.Errorf("%w: length %d", constants.ErrIdempotencyKeyInvalid, len(key))
	}
	if fn == nil {
		return nil, fmt.Errorf("%w: nil operation", constants.ErrIdempotencyKeyInvalid)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	redisKey := s.options.KeyPrefix + key
	owner := uuid.NewString()
	for {
		claim, err := s.claim(ctx, redisKey, fingerprint, owner)
		if err != nil {
			return nil, err
		}
		switch claim.kind {
		case redisIdempotencyClaimAcquired:
			return s.executeOwned(ctx, redisKey, fingerprint, owner, claim.leaseDeadline, fn)
		case redisIdempotencyClaimDone:
			return claim.response, claim.err
		case redisIdempotencyClaimWait:
			waitFor := s.options.PollInterval
			if claim.waitFor > 0 && claim.waitFor < waitFor {
				waitFor = claim.waitFor
			}
			if waitFor < time.Millisecond {
				waitFor = time.Millisecond
			}
			timer := time.NewTimer(waitFor)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return nil, ctx.Err()
			case <-timer.C:
			}
		default:
			return nil, constants.ErrIdempotencyStoreCorrupt
		}
	}
}

func (s *RedisIdempotencyStore) claim(
	ctx context.Context,
	redisKey string,
	fingerprint string,
	owner string,
) (redisIdempotencyClaim, error) {
	operationCtx, cancel := context.WithTimeout(ctx, s.options.OperationTimeout)
	defer cancel()
	claimStarted := time.Now()
	values, err := s.client.Eval(
		operationCtx,
		redisIdempotencyAcquireScript,
		[]string{redisKey},
		fingerprint,
		owner,
		durationMillis(s.options.LeaseTTL),
	).Slice()
	if err != nil {
		return redisIdempotencyClaim{}, fmt.Errorf("claim Redis idempotency key: %w", err)
	}
	if len(values) == 0 {
		return redisIdempotencyClaim{}, constants.ErrIdempotencyStoreCorrupt
	}
	state, err := redisIdempotencyString(values[0])
	if err != nil {
		return redisIdempotencyClaim{}, err
	}
	switch state {
	case "acquired":
		if len(values) != 1 {
			return redisIdempotencyClaim{}, constants.ErrIdempotencyStoreCorrupt
		}
		return redisIdempotencyClaim{
			kind:          redisIdempotencyClaimAcquired,
			leaseDeadline: claimStarted.Add(s.options.LeaseTTL),
		}, nil
	case "conflict":
		return redisIdempotencyClaim{}, constants.ErrIdempotencyConflict
	case "corrupt":
		return redisIdempotencyClaim{}, constants.ErrIdempotencyStoreCorrupt
	case "wait":
		if len(values) != 2 {
			return redisIdempotencyClaim{}, constants.ErrIdempotencyStoreCorrupt
		}
		ttlText, err := redisIdempotencyString(values[1])
		if err != nil {
			return redisIdempotencyClaim{}, err
		}
		ttlMillis, err := strconv.ParseInt(ttlText, 10, 64)
		if err != nil || ttlMillis < 0 {
			return redisIdempotencyClaim{}, constants.ErrIdempotencyStoreCorrupt
		}
		return redisIdempotencyClaim{kind: redisIdempotencyClaimWait, waitFor: time.Duration(ttlMillis) * time.Millisecond}, nil
	case "done":
		if len(values) != 4 {
			return redisIdempotencyClaim{}, constants.ErrIdempotencyStoreCorrupt
		}
		response, responseErr := decodeRedisIdempotencyResult(values[1], values[2], values[3])
		if responseErr != nil {
			return redisIdempotencyClaim{}, responseErr
		}
		return redisIdempotencyClaim{kind: redisIdempotencyClaimDone, response: response.response, err: response.err}, nil
	default:
		return redisIdempotencyClaim{}, constants.ErrIdempotencyStoreCorrupt
	}
}

type redisIdempotencyResult struct {
	response *protos.Response
	err      error
	encoded  []byte
	has      string
	hadError string
}

func encodeRedisIdempotencyResult(response *protos.Response, err error) (redisIdempotencyResult, error) {
	normalized := cloneRPCResponse(response)
	if err != nil && (normalized == nil || normalized.Status == nil) {
		normalized = rpcErrorResponse(err)
	}
	result := redisIdempotencyResult{response: normalized, has: "0", hadError: "0"}
	if err != nil {
		result.hadError = "1"
	}
	if normalized == nil {
		return result, nil
	}
	encoded, marshalErr := proto.Marshal(normalized)
	if marshalErr != nil {
		return redisIdempotencyResult{}, fmt.Errorf("marshal Redis idempotency response: %w", marshalErr)
	}
	result.has = "1"
	result.encoded = encoded
	if result.hadError == "1" {
		result.err = apierrors.FromStatus(normalized.Status)
	}
	return result, nil
}

func decodeRedisIdempotencyResult(hasValue, encodedValue, hadErrorValue any) (redisIdempotencyResult, error) {
	hasResponse, err := redisIdempotencyString(hasValue)
	if err != nil || (hasResponse != "0" && hasResponse != "1") {
		return redisIdempotencyResult{}, constants.ErrIdempotencyStoreCorrupt
	}
	hadError, err := redisIdempotencyString(hadErrorValue)
	if err != nil || (hadError != "0" && hadError != "1") {
		return redisIdempotencyResult{}, constants.ErrIdempotencyStoreCorrupt
	}
	encoded, err := redisIdempotencyBytes(encodedValue)
	if err != nil {
		return redisIdempotencyResult{}, err
	}
	result := redisIdempotencyResult{has: hasResponse, hadError: hadError, encoded: encoded}
	if hasResponse == "0" {
		if len(encoded) != 0 || hadError == "1" {
			return redisIdempotencyResult{}, constants.ErrIdempotencyStoreCorrupt
		}
		return result, nil
	}
	response := &protos.Response{}
	if len(encoded) == 0 || proto.Unmarshal(encoded, response) != nil {
		return redisIdempotencyResult{}, constants.ErrIdempotencyStoreCorrupt
	}
	result.response = response
	if hadError == "1" {
		if response.Status == nil {
			return redisIdempotencyResult{}, constants.ErrIdempotencyStoreCorrupt
		}
		result.err = apierrors.FromStatus(response.Status)
	}
	return result, nil
}

func (s *RedisIdempotencyStore) executeOwned(
	ctx context.Context,
	redisKey string,
	fingerprint string,
	owner string,
	leaseDeadline time.Time,
	fn func(context.Context) (*protos.Response, error),
) (response *protos.Response, err error) {
	executionCtx, cancelExecution := context.WithCancelCause(ctx)
	heartbeatCtx, cancelHeartbeat := context.WithCancel(context.Background())
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatDone <- s.renewLease(
			heartbeatCtx,
			redisKey,
			fingerprint,
			owner,
			leaseDeadline,
			cancelExecution,
		)
	}()

	defer func() {
		if recovered := recover(); recovered != nil {
			cancelHeartbeat()
			<-heartbeatDone
			failure, _ := encodeRedisIdempotencyResult(
				rpcErrorResponse(errIdempotentRPCOperationFailed),
				errIdempotentRPCOperationFailed,
			)
			_ = s.complete(redisKey, fingerprint, owner, failure)
			cancelExecution(errIdempotentRPCOperationFailed)
			panic(recovered)
		}
	}()

	response, err = fn(executionCtx)
	cancelHeartbeat()
	heartbeatErr := <-heartbeatDone
	result, encodeErr := encodeRedisIdempotencyResult(response, err)
	if encodeErr != nil {
		failure, failureEncodeErr := encodeRedisIdempotencyResult(nil, errIdempotentRPCOperationFailed)
		if failureEncodeErr != nil {
			commitErr := fmt.Errorf(
				"%w: stable failure result could not be encoded",
				constants.ErrIdempotencyCommitUnknown,
			)
			cancelExecution(commitErr)
			return rpcErrorResponse(commitErr), dominantIdempotencyError(
				commitErr,
				encodeErr,
				failureEncodeErr,
				heartbeatErr,
			)
		}
		completeErr := s.complete(redisKey, fingerprint, owner, failure)
		if completeErr != nil {
			cancelExecution(completeErr)
			return rpcErrorResponse(completeErr), dominantIdempotencyError(
				completeErr,
				failure.err,
				encodeErr,
				heartbeatErr,
			)
		}
		cancelExecution(encodeErr)
		return failure.response, dominantIdempotencyError(failure.err, encodeErr, heartbeatErr)
	}
	completeErr := s.complete(redisKey, fingerprint, owner, result)
	cancelExecution(completeErr)
	if completeErr != nil {
		return rpcErrorResponse(completeErr), dominantIdempotencyError(
			completeErr,
			result.err,
			heartbeatErr,
		)
	}
	return result.response, result.err
}

func dominantIdempotencyError(primary error, secondary ...error) error {
	if primary == nil {
		return errors.Join(secondary...)
	}
	causes := make([]error, 0, len(secondary)+1)
	causes = append(causes, primary)
	causes = append(causes, secondary...)
	return apierrors.FromError(primary).WithCause(errors.Join(causes...))
}

func (s *RedisIdempotencyStore) renewLease(
	ctx context.Context,
	redisKey string,
	fingerprint string,
	owner string,
	leaseDeadline time.Time,
	cancelExecution context.CancelCauseFunc,
) error {
	ticker := time.NewTicker(s.options.HeartbeatInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		deadlineTimer := time.NewTimer(time.Until(leaseDeadline))
		select {
		case <-ctx.Done():
			if !deadlineTimer.Stop() {
				<-deadlineTimer.C
			}
			return nil
		case <-deadlineTimer.C:
			err := fmt.Errorf("%w: %v", constants.ErrIdempotencyLeaseLost, lastErr)
			cancelExecution(err)
			return err
		case <-ticker.C:
			if !deadlineTimer.Stop() {
				<-deadlineTimer.C
			}
		}

		renewStarted := time.Now()
		operationCtx, cancel := context.WithTimeout(context.Background(), s.options.OperationTimeout)
		renewed, err := s.client.Eval(
			operationCtx,
			redisIdempotencyRenewScript,
			[]string{redisKey},
			fingerprint,
			owner,
			durationMillis(s.options.LeaseTTL),
		).Int64()
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if renewed != 1 {
			err = constants.ErrIdempotencyLeaseLost
			cancelExecution(err)
			return err
		}
		lastErr = nil
		// Redis starts the new TTL before its response reaches us. Anchoring the
		// local deadline to request start keeps our ownership estimate conservative.
		leaseDeadline = renewStarted.Add(s.options.LeaseTTL)
	}
}

func (s *RedisIdempotencyStore) complete(
	redisKey string,
	fingerprint string,
	owner string,
	result redisIdempotencyResult,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.options.OperationTimeout)
	defer cancel()
	completed, err := s.client.Eval(
		ctx,
		redisIdempotencyCompleteScript,
		[]string{redisKey},
		fingerprint,
		owner,
		durationMillis(s.options.ResultTTL),
		result.has,
		result.encoded,
		result.hadError,
	).Int64()
	if err != nil {
		return fmt.Errorf("%w: %v", constants.ErrIdempotencyCommitUnknown, err)
	}
	if completed != 1 {
		return constants.ErrIdempotencyLeaseLost
	}
	return nil
}

func redisIdempotencyString(value any) (string, error) {
	switch value := value.(type) {
	case string:
		return value, nil
	case []byte:
		return string(value), nil
	default:
		return "", constants.ErrIdempotencyStoreCorrupt
	}
}

func redisIdempotencyBytes(value any) ([]byte, error) {
	switch value := value.(type) {
	case string:
		return []byte(value), nil
	case []byte:
		return append([]byte(nil), value...), nil
	default:
		return nil, constants.ErrIdempotencyStoreCorrupt
	}
}

func durationMillis(duration time.Duration) int64 {
	if duration < time.Millisecond {
		return 1
	}
	return duration.Milliseconds()
}
