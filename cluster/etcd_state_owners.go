package cluster

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeromicro/go-zero/core/hash"
	"golang.org/x/sync/semaphore"
)

// etcdLifecycleOwner owns lifecycle admission, worker accounting, and terminal
// publication. No lifecycle goroutine may be added without holding mu.
type etcdLifecycleOwner struct {
	mu               sync.Mutex
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	state            etcdLifecycleState
	running          atomic.Bool
	initDone         chan struct{}
	initErr          error
	waitingCallbacks atomic.Bool
	shutdownOnce     sync.Once
	shutdownErr      error
	beforeStopOnce   sync.Once
	appDieChan       chan bool
}

// etcdLeaseOwner owns the currently published registration generation and all
// contexts/channels used to replace or renew it.
type etcdLeaseOwner struct {
	id                 atomic.Int64
	publicationGate    *semaphore.Weighted
	registration       atomic.Pointer[selfRegistrationSnapshot]
	renewal            chan struct{}
	ctx                context.Context
	cancel             context.CancelFunc
	flushCtx           context.Context
	flushCancel        context.CancelFunc
	heartbeatTTL       time.Duration
	logHeartbeat       bool
	lastHeartbeatTime  time.Time
	revokeTimeout      time.Duration
	grantTimeout       time.Duration
	grantMaxRetries    int
	grantRetryInterval time.Duration
}

// etcdWatchOwner owns the membership snapshot, watch revision, consistent-hash
// cache, and callback lane derived from watch events.
type etcdWatchOwner struct {
	syncInterval time.Duration
	lastSync     atomic.Int64

	byTypeMu sync.RWMutex
	byType   map[string]map[string]*Server
	byID     sync.Map

	listenersMu sync.RWMutex
	listeners   []SDListener
	callbacks   *callbackDispatcher

	fetchMu          sync.Mutex
	reconcileMu      sync.Mutex
	revisions        map[string]int64
	revisionFloor    int64
	watchRevision    int64
	blacklist        []string
	parallelism      int
	consistentHashMu sync.RWMutex
	consistentHash   map[string]*hash.ConsistentHash
}

// etcdElectionOwner owns election cancellation, generation admission, leader
// state, and the leader callback lane. Lifecycle admission remains coordinated
// with etcdLifecycleOwner.mu before a cancel function is installed.
type etcdElectionOwner struct {
	enabled        bool
	name           string
	resumeLeader   bool
	reconnectDelay time.Duration

	cancelMu sync.Mutex
	cancel   context.CancelFunc

	admissionMu sync.Mutex
	generation  uint64
	leaderMu    sync.RWMutex
	leaderID    string
	listener    LeaderChangeListener
	callbacks   *callbackDispatcher
}
