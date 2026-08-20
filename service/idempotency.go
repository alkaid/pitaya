package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/alkaid/pitaya/v2/cluster"
	"github.com/alkaid/pitaya/v2/constants"
	pcontext "github.com/alkaid/pitaya/v2/context"
	rpccontext "github.com/alkaid/pitaya/v2/internal/rpccontext"
	"github.com/alkaid/pitaya/v2/protos"
	"google.golang.org/protobuf/proto"
)

const MaxIdempotencyKeyBytes = 256

var errIdempotentRPCOperationFailed = errors.New("idempotent RPC operation failed")

// ErrIdempotencyStoreUnavailable is safe to return across an RPC boundary when
// the durable idempotency store cannot complete an admission operation.
var ErrIdempotencyStoreUnavailable = errors.New("RPC idempotency store is unavailable")

// IdempotencyStore owns command coalescing and completed-result retention.
// Implementations must not start another fn while the current ownership record
// remains valid. Lease-based stores may execute fn again after ownership expires;
// callback cancellation is cooperative, so business effects still need a
// transaction-level unique key or fencing check. Reusing a key with another
// fingerprint must fail with constants.ErrIdempotencyConflict. Execute receives
// the request's cancellation and deadline after RPC admission metadata has been
// consumed; implementations must use the explicit key and fingerprint instead
// of reading reserved values from propagation metadata.
type IdempotencyStore interface {
	Execute(
		ctx context.Context,
		key string,
		fingerprint string,
		fn func(context.Context) (*protos.Response, error),
	) (*protos.Response, error)
}

type rpcIdempotencyInfoContextKey struct{}

// RPCIdempotencyInfo identifies the current logical command at the local
// admission boundary. StoreKey is already opaque and bounded. A business that
// needs exactly-once effects can persist StoreKey and Fingerprint in the same
// transaction as its own state, reject conflicts, and make duplicate delivery
// return the transaction's existing outcome.
type RPCIdempotencyInfo struct {
	StoreKey    string
	Fingerprint string
}

// RPCIdempotencyInfoFromContext returns local command identity. The value is
// not propagation metadata and is intentionally removed at the next RPC hop.
func RPCIdempotencyInfoFromContext(ctx context.Context) (RPCIdempotencyInfo, bool) {
	if ctx == nil {
		return RPCIdempotencyInfo{}, false
	}
	info, ok := ctx.Value(rpcIdempotencyInfoContextKey{}).(RPCIdempotencyInfo)
	return info, ok
}

func withRPCIdempotencyInfo(ctx context.Context, storeKey, fingerprint string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, rpcIdempotencyInfoContextKey{}, RPCIdempotencyInfo{
		StoreKey:    storeKey,
		Fingerprint: fingerprint,
	})
}

const idempotencyStoreKeyVersion = "v1"

type MemoryIdempotencyStoreOptions struct {
	TTL        time.Duration
	MaxEntries int
}

func DefaultIdempotencyStoreOptions() MemoryIdempotencyStoreOptions {
	return MemoryIdempotencyStoreOptions{TTL: 15 * time.Minute, MaxEntries: 10_000}
}

type memoryIdempotencyEntry struct {
	fingerprint string
	response    *protos.Response
	err         error
	done        chan struct{}
	completed   bool
	expiresAt   time.Time
}

// MemoryIdempotencyStore is a bounded, process-local implementation. It
// coalesces concurrent retries; applications needing failover-safe exactly-once
// effects should inject a durable store and transact it with business state.
type MemoryIdempotencyStore struct {
	mu         sync.Mutex
	entries    map[string]*memoryIdempotencyEntry
	ttl        time.Duration
	maxEntries int
}

func NewMemoryIdempotencyStore(options MemoryIdempotencyStoreOptions) *MemoryIdempotencyStore {
	defaults := DefaultIdempotencyStoreOptions()
	if options.TTL <= 0 {
		options.TTL = defaults.TTL
	}
	if options.MaxEntries <= 0 {
		options.MaxEntries = defaults.MaxEntries
	}
	return &MemoryIdempotencyStore{
		entries:    make(map[string]*memoryIdempotencyEntry),
		ttl:        options.TTL,
		maxEntries: options.MaxEntries,
	}
}

func (s *MemoryIdempotencyStore) Execute(
	ctx context.Context,
	key string,
	fingerprint string,
	fn func(context.Context) (*protos.Response, error),
) (response *protos.Response, err error) {
	if key == "" || len(key) > MaxIdempotencyKeyBytes {
		return nil, fmt.Errorf("%w: length %d", constants.ErrIdempotencyKeyInvalid, len(key))
	}
	if fn == nil {
		return nil, fmt.Errorf("%w: nil operation", constants.ErrIdempotencyKeyInvalid)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	now := time.Now()
	s.mu.Lock()
	s.pruneExpiredLocked(now)
	if existing := s.entries[key]; existing != nil {
		if existing.fingerprint != fingerprint {
			s.mu.Unlock()
			return nil, constants.ErrIdempotencyConflict
		}
		done := existing.done
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
		}
		s.mu.Lock()
		response = cloneRPCResponse(existing.response)
		err = existing.err
		s.mu.Unlock()
		return response, err
	}
	if len(s.entries) >= s.maxEntries {
		s.evictOldestCompletedLocked()
		if len(s.entries) >= s.maxEntries {
			s.mu.Unlock()
			return nil, constants.ErrIdempotencyStoreFull
		}
	}

	entry := &memoryIdempotencyEntry{fingerprint: fingerprint, done: make(chan struct{})}
	s.entries[key] = entry
	s.mu.Unlock()

	defer func() {
		if recovered := recover(); recovered != nil {
			s.mu.Lock()
			entry.err = errIdempotentRPCOperationFailed
			entry.response = rpcErrorResponse(errIdempotentRPCOperationFailed)
			entry.completed = true
			entry.expiresAt = time.Now().Add(s.ttl)
			close(entry.done)
			s.mu.Unlock()
			panic(recovered)
		}
	}()
	response, err = fn(ctx)

	s.mu.Lock()
	entry.response = cloneRPCResponse(response)
	entry.err = err
	entry.completed = true
	entry.expiresAt = time.Now().Add(s.ttl)
	close(entry.done)
	s.mu.Unlock()
	return response, err
}

func (s *MemoryIdempotencyStore) pruneExpiredLocked(now time.Time) {
	for key, entry := range s.entries {
		if entry.completed && !entry.expiresAt.After(now) {
			delete(s.entries, key)
		}
	}
}

func (s *MemoryIdempotencyStore) evictOldestCompletedLocked() {
	var oldestKey string
	var oldest time.Time
	for key, entry := range s.entries {
		if !entry.completed {
			continue
		}
		if oldestKey == "" || entry.expiresAt.Before(oldest) {
			oldestKey, oldest = key, entry.expiresAt
		}
	}
	if oldestKey != "" {
		delete(s.entries, oldestKey)
	}
}

func cloneRPCResponse(response *protos.Response) *protos.Response {
	if response == nil {
		return nil
	}
	return proto.Clone(response).(*protos.Response)
}

func idempotencyKeyFromContext(ctx context.Context) (string, error) {
	value := pcontext.GetFromPropagateCtx(ctx, constants.IdempotencyKey)
	if value == nil {
		return "", nil
	}
	key, ok := value.(string)
	if !ok || key == "" || len(key) > MaxIdempotencyKeyBytes {
		return "", fmt.Errorf("%w: expected non-empty string up to %d bytes", constants.ErrIdempotencyKeyInvalid, MaxIdempotencyKeyBytes)
	}
	return key, nil
}

// idempotencyStoreKey returns an opaque, bounded key for storage. Ordinary
// routed calls share a server-type scope so a durable store can deduplicate
// across owner failover. Fanout commands add the local server ID because every
// target must execute the command independently.
func idempotencyStoreKey(ctx context.Context, server *cluster.Server, key string) (string, error) {
	fanout, err := fanoutIdempotencyScope(ctx)
	if err != nil {
		return "", err
	}

	fields := []string{idempotencyStoreKeyVersion, "routed", serverType(server), key}
	if fanout {
		fields[1] = "fanout"
		fields = append(fields, serverID(server))
	}
	return hashIdempotencyFields(fields...), nil
}

func hashIdempotencyFields(fields ...string) string {
	hash := sha256.New()
	for _, field := range fields {
		_, _ = fmt.Fprintf(hash, "%d:", len(field))
		_, _ = hash.Write([]byte(field))
	}
	return idempotencyStoreKeyVersion + ":" + hex.EncodeToString(hash.Sum(nil))
}

func fanoutIdempotencyScope(ctx context.Context) (bool, error) {
	value := pcontext.GetFromPropagateCtx(ctx, constants.IdempotencyFanoutKey)
	if value == nil {
		return false, nil
	}
	marker, ok := value.(string)
	if !ok || marker != "1" {
		return false, fmt.Errorf("%w: invalid fanout scope", constants.ErrIdempotencyKeyInvalid)
	}
	return true, nil
}

func withFanoutIdempotencyScope(ctx context.Context) context.Context {
	return pcontext.AddToPropagateCtx(ctx, constants.IdempotencyFanoutKey, "1")
}

// consumeRPCAdmissionMetadata removes metadata whose authority ends at the
// current RPC admission boundary. A handler that emits another logical command
// must attach that command's own idempotency key explicitly.
func consumeRPCAdmissionMetadata(ctx context.Context) (context.Context, error) {
	if _, err := fanoutIdempotencyScope(ctx); err != nil {
		return ctx, err
	}
	propagated := pcontext.ToMap(ctx)
	changed := false
	for _, metadataKey := range []string{
		constants.OwnerServerIDKey,
		constants.OwnerEpochKey,
		constants.IdempotencyKey,
		constants.IdempotencyFanoutKey,
		rpccontext.ReliableJobKey,
		rpccontext.ReliableJobMetadataDigestKey,
	} {
		if _, ok := propagated[metadataKey]; ok {
			delete(propagated, metadataKey)
			changed = true
		}
	}
	if changed {
		if ctx == nil {
			ctx = context.Background()
		}
		ctx = context.WithValue(ctx, constants.PropagateCtxKey, propagated)
	}
	return rpccontext.WithoutReliableRPCJob(ctx), nil
}

func validateOwnerFence(ctx context.Context, server *cluster.Server) error {
	localEpoch, ownerValid := server.OwnerEpochSnapshot()
	if !ownerValid {
		return fmt.Errorf("%w: local owner registration is not valid", constants.ErrStaleOwnerEpoch)
	}
	value := pcontext.GetFromPropagateCtx(ctx, constants.OwnerEpochKey)
	if value == nil {
		if localEpoch > 0 {
			return fmt.Errorf("%w: owner epoch is required", constants.ErrStaleOwnerEpoch)
		}
		return nil
	}
	epochText, ok := value.(string)
	if !ok {
		return fmt.Errorf("%w: owner epoch has type %T", constants.ErrStaleOwnerEpoch, value)
	}
	expected, err := strconv.ParseUint(epochText, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: invalid owner epoch", constants.ErrStaleOwnerEpoch)
	}
	if expected == 0 && localEpoch == 0 {
		return nil
	}
	ownerID, _ := pcontext.GetFromPropagateCtx(ctx, constants.OwnerServerIDKey).(string)
	if server == nil || ownerID != server.ID || expected != localEpoch {
		return fmt.Errorf(
			"%w: target=%s/%d local=%s/%d",
			constants.ErrStaleOwnerEpoch,
			ownerID,
			expected,
			serverID(server),
			localEpoch,
		)
	}
	return nil
}

func serverID(server *cluster.Server) string {
	if server == nil {
		return ""
	}
	return server.ID
}

func serverType(server *cluster.Server) string {
	if server == nil {
		return ""
	}
	return server.Type
}

func reliableRPCJobMetadata(values map[string]any, key string) (string, bool, error) {
	markerValue, markerFound := values[rpccontext.ReliableJobKey]
	digestValue, digestFound := values[rpccontext.ReliableJobMetadataDigestKey]
	if !markerFound && !digestFound {
		return "", false, nil
	}
	marker, markerOK := markerValue.(string)
	digest, digestOK := digestValue.(string)
	wireKey, wireKeyOK := values[constants.IdempotencyKey].(string)
	if !markerFound || !digestFound || !markerOK || !digestOK ||
		key == "" || marker != key || !wireKeyOK || wireKey != key || len(digest) != sha256.Size*2 {
		return "", false, fmt.Errorf(
			"%w: invalid reliable RPC job metadata",
			constants.ErrIdempotencyKeyInvalid,
		)
	}
	decodedDigest, digestErr := hex.DecodeString(digest)
	if digestErr != nil || len(decodedDigest) != sha256.Size || hex.EncodeToString(decodedDigest) != digest {
		return "", false, fmt.Errorf(
			"%w: invalid reliable RPC job metadata",
			constants.ErrIdempotencyKeyInvalid,
		)
	}
	return digest, true, nil
}

func rpcRequestFingerprint(request *protos.Request, key string, reliableJob bool, reliableDigest string) (string, error) {
	if request == nil {
		if reliableJob {
			return "", fmt.Errorf("%w: reliable RPC request is nil", constants.ErrIdempotencyKeyInvalid)
		}
		sum := sha256.Sum256(nil)
		return hex.EncodeToString(sum[:]), nil
	}
	command := proto.Clone(request).(*protos.Request)
	metadata, err := normalizedRPCMetadata(command.Metadata, key, reliableJob, reliableDigest)
	if err != nil {
		return "", err
	}
	if reliableJob {
		// A marked ReliableRPC retry may run on another worker. These fields name
		// that executor, while the matching JID identifies the logical command.
		command.FrontendID = ""
	}
	command.Metadata = metadata
	if command.Msg != nil {
		command.Msg.Reply = ""
	}
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(command)
	if err != nil {
		return "", fmt.Errorf("fingerprint RPC request: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func validateUnkeyedReliableRPCMetadata(request *protos.Request) error {
	if request == nil {
		return nil
	}
	metadata := request.GetMetadata()
	// Normal encoders emit literal reserved keys. A Unicode escape requires a
	// structured decode because JSON object keys may legally be escaped.
	if !bytes.Contains(metadata, []byte(`"`+rpccontext.ReliableJobKey+`"`)) &&
		!bytes.Contains(metadata, []byte(`"`+rpccontext.ReliableJobMetadataDigestKey+`"`)) &&
		!bytes.Contains(metadata, []byte(`\u`)) {
		return nil
	}
	_, err := normalizedRPCMetadata(metadata, "", false, "")
	return err
}

func normalizedRPCMetadata(metadata []byte, key string, reliableJob bool, reliableDigest string) ([]byte, error) {
	if len(metadata) == 0 {
		if reliableJob {
			return nil, fmt.Errorf(
				"%w: reliable RPC metadata is missing from the request",
				constants.ErrIdempotencyKeyInvalid,
			)
		}
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(metadata))
	decoder.UseNumber()
	values := map[string]any{}
	err := decoder.Decode(&values)
	if err != nil {
		return nil, fmt.Errorf("fingerprint RPC metadata: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("fingerprint RPC metadata: trailing JSON value")
	}
	requestDigest, requestReliableJob, err := reliableRPCJobMetadata(values, key)
	if err != nil {
		return nil, err
	}
	if requestReliableJob != reliableJob || requestDigest != reliableDigest {
		return nil, fmt.Errorf(
			"%w: request and admission reliable RPC metadata differ",
			constants.ErrIdempotencyKeyInvalid,
		)
	}
	for _, key := range []string{
		constants.IdempotencyKey,
		constants.IdempotencyFanoutKey,
		constants.OwnerServerIDKey,
		constants.OwnerEpochKey,
		constants.RequestTimeout,
		constants.RequestIDKey,
		constants.StartTimeKey,
		constants.RouteKey,
		constants.MetricTagsKey,
		constants.SpanPropagateCtxKey,
	} {
		delete(values, key)
	}
	if reliableJob {
		delete(values, constants.PeerIDKey)
		delete(values, constants.PeerServiceKey)
	}
	normalized, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("fingerprint RPC metadata: %w", err)
	}
	return normalized, nil
}
