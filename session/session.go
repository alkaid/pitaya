// Copyright (c) nano Author and TFG Co. All Rights Reserved.
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

package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alkaid/pitaya/v2/co"
	"github.com/pkg/errors"
	"github.com/samber/lo"

	"github.com/alkaid/pitaya/v2/util"

	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/networkentity"
	"github.com/alkaid/pitaya/v2/protos"
	nats "github.com/nats-io/nats.go"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

const (
	cacheKeyPitayaSession  = "pit:u:%s:" // pitaya session key = pitaya:session:<uid>
	fieldKeyFrontendID     = "f"         // frontend id
	fieldKeyFrontendSessID = "s"         // frontend session id
	fieldKeyOnline         = "o"         // 是否在线
	fieldKeyBackends       = "bs"        // backends id list
	fieldKeyIP             = "ip"        // ip
	fieldKeyData           = "u"         // 用户自定义数据 json存储
)

type CloseReason = int // 关闭原因

const (
	CloseReasonNormal CloseReason = iota // 默认关闭
)
const (
	CloseReasonKickMin    CloseReason = 100
	CloseReasonKickRebind             = 101 // 重新绑定,同一session在其他设备登录时发生
	CloseReasonKickManual             = 102 // 手动被踢(封号)
	CloseReasonKickMax    CloseReason = 1000
)

type OnSessionBindFunc func(ctx context.Context, s Session, callback map[string]string) error
type OnSessionCloseFunc func(s Session, callback map[string]string, reason CloseReason)
type OnSessionBindBackendFunc func(ctx context.Context, s Session, serverType, serverId string, callback map[string]string) error
type OnSessionKickBackendFunc func(ctx context.Context, s Session, serverType, serverId string, callback map[string]string, reason CloseReason) error

// BackendRollback rolls back the tentative local backend mutation when it is
// still owned by the current transaction.
type BackendRollback func() bool

type BackendBindCoordinatorFunc func(ctx context.Context, s Session, serverType, serverId string, callback map[string]string, rollback BackendRollback) error
type BackendKickCoordinatorFunc func(ctx context.Context, s Session, serverType, serverId string, callback map[string]string, reason CloseReason, rollback BackendRollback) error

// BackendCoordinator owns the fallible framework side effects for backend
// binding mutations. Application extensions belong in the after callbacks.
type BackendCoordinator struct {
	Bind BackendBindCoordinatorFunc
	Kick BackendKickCoordinatorFunc
}

type BoundData struct {
	FrontendID string            `json:"_fid"` // 绑定的网关ID
	UID        string            `json:"_uid"`
	Backends   map[string]string `json:"_backends"` // 绑定的后端ID表 key:serverType value:serverID
}

type handshakeValidatorRegistry struct {
	mu         sync.RWMutex
	validators map[string]func(data *HandshakeData) error
}

func (r *handshakeValidatorRegistry) add(name string, validator func(data *HandshakeData) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.validators == nil {
		r.validators = make(map[string]func(data *HandshakeData) error)
	}
	r.validators[name] = validator
}

func (r *handshakeValidatorRegistry) snapshot() map[string]func(data *HandshakeData) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	validators := make(map[string]func(data *HandshakeData) error, len(r.validators))
	for name, validator := range r.validators {
		validators[name] = validator
	}
	return validators
}

type sessionPoolImpl struct {
	sessionBindCallbacks []OnSessionBindFunc
	afterBindCallbacks   []OnSessionBindFunc
	handshakeValidators  handshakeValidatorRegistry
	sessionsMutex        sync.Mutex
	// SessionCloseCallbacks contains global session close callbacks
	SessionCloseCallbacks []OnSessionCloseFunc
	sessionsByUID         sync.Map
	sessionsByID          sync.Map
	sessionIDSvc          *sessionIDService
	// SessionCount keeps the current number of sessions
	SessionCount              int64
	UserCount                 int64
	storage                   CacheInterface
	backendCoordinator        atomic.Pointer[BackendCoordinator]
	afterBindBackendCallbacks []OnSessionBindBackendFunc
	afterKickBackendCallbacks []OnSessionKickBackendFunc
}

// SessionPool centralizes all sessions within a Pitaya app
type SessionPool interface {
	// NewSession returns a new session instance 或者缓存
	// a networkentity.NetworkEntity is a low-level network instance
	NewSession(entity networkentity.NetworkEntity, frontend bool, UID ...string) (s Session, isNew bool)
	GetSessionCount() int64
	GetUserCount() int64
	GetSessionCloseCallbacks() []OnSessionCloseFunc
	GetSessionByUID(uid string) Session
	GetSessionByID(id int64) Session
	OnSessionBind(f OnSessionBindFunc)
	OnAfterSessionBind(f OnSessionBindFunc)
	OnSessionClose(f OnSessionCloseFunc)
	OnAfterBindBackend(f OnSessionBindBackendFunc)
	OnAfterKickBackend(f OnSessionKickBackendFunc)
	CloseAll()
	ForceCloseAll()
	AddHandshakeValidator(name string, f func(data *HandshakeData) error)
	EncodeSessionData(data map[string]interface{}) ([]byte, error)
	DecodeSessionData(encodedData []byte) (map[string]interface{}, error)
	// StoreSessionLocal 将session存储到本的缓存 session必须已经绑定过UID
	//  @param session
	//  @return error
	StoreSessionLocal(session Session) error
	// RemoveSessionLocal 将session从本地缓存删除
	//  @param session
	RemoveSessionLocal(session Session)
	// SetClusterCache 设置后端缓存存储服务
	//  @param storage
	SetClusterCache(storage CacheInterface)
	RangeUsers(f func(uid string, sess SessPublic) bool)
	RangeSessions(f func(sid int64, sess SessPublic) bool)
}

// BackendCoordinatorInstaller is the narrow framework capability used to
// install backend transaction coordination without expanding SessionPool.
type BackendCoordinatorInstaller interface {
	SetBackendCoordinator(coordinator BackendCoordinator)
}

// InstallBackendCoordinator installs the framework-owned coordinator when the
// configured SessionPool supports transactional backend mutations.
func InstallBackendCoordinator(pool SessionPool, coordinator BackendCoordinator) error {
	installer, ok := pool.(BackendCoordinatorInstaller)
	if !ok {
		return errors.Errorf("session pool %T does not support backend coordination", pool)
	}
	installer.SetBackendCoordinator(coordinator)
	return nil
}

// HandshakeClientData represents information about the client sent on the handshake.
type HandshakeClientData struct {
	Platform    string `json:"platform"`
	LibVersion  string `json:"libVersion"`
	BuildNumber string `json:"clientBuildNumber"`
	Version     string `json:"clientVersion"`
}

// HandshakeData represents information about the handshake sent by the client.
// `sys` corresponds to information independent from the app and `user` information
// that depends on the app and is customized by the user.
type HandshakeData struct {
	Sys  HandshakeClientData    `json:"sys"`
	User map[string]interface{} `json:"user,omitempty"`
}

type sessionIdentity struct {
	uid    string
	uidInt int64
	online bool
}

type backendMutationToken struct {
	serverType string
	id         string
	generation uint64
}

// backendBindings owns the backend binding map and the mutation generations
// used to make coordinator rollbacks conditional on the mutation they started.
// Its zero value is ready for use.
type backendBindings struct {
	mu          sync.RWMutex
	backends    map[string]string
	generations map[string]uint64
}

// sessionLifecycleOwner owns identity publication, bind/close admission and
// resources that must be detached exactly once when the session closes.
type sessionLifecycleOwner struct {
	lifecycleMutex   sync.RWMutex
	binding          bool
	closed           bool
	identity         atomic.Pointer[sessionIdentity]
	registeredUID    string
	lastTime         int64
	OnCloseCallbacks []func()
	Subscriptions    []*nats.Subscription
}

// sessionDataOwner owns the mutable client/session snapshot. Its embedded lock
// intentionally preserves the existing sessionImpl Lock/RLock method set.
type sessionDataOwner struct {
	sync.RWMutex
	data              map[string]any
	handshakeData     *HandshakeData
	encodedData       []byte
	IsFrontend        bool
	frontendID        string
	frontendSessionID int64
	ip                string
}

type sessionDataSnapshot struct {
	isFrontend        bool
	frontendID        string
	frontendSessionID int64
	ip                string
	encodedData       []byte
}

func (o *sessionDataOwner) snapshot() sessionDataSnapshot {
	o.RLock()
	defer o.RUnlock()
	return sessionDataSnapshot{
		isFrontend:        o.IsFrontend,
		frontendID:        o.frontendID,
		frontendSessionID: o.frontendSessionID,
		ip:                o.ip,
		encodedData:       append([]byte(nil), o.encodedData...),
	}
}

type sessionImpl struct {
	sessionLifecycleOwner
	sessionDataOwner
	id               int64                       // session global unique id
	entity           networkentity.NetworkEntity // low-level network entity
	backendBindings  backendBindings             // 绑定的backends
	requestsInFlight ReqInFlight
	pool             *sessionPoolImpl
}

type ReqInFlight struct {
	m  map[string]string
	mu *sync.RWMutex
}

// SessPublic 供业务层使用的 Session
type SessPublic interface {
	GetIsFrontend() bool
	GetFrontendID() string
	GetFrontendSessionID() int64
	Push(route string, v interface{}) error
	ID() int64
	UID() string
	UIDInt() int64
	// Online 是否在线,用于cluster类型session的在线判断
	//  @return bool
	Online() bool
	// Bind 绑定session到他当前所在的frontend
	//  @param ctx
	//  @param uid
	//  @param custSessData 自定义session数据,可以nil,功能和 PushToFront 一样,但比它少N次IO
	//  @param callback 回调数据,通知其他服务时透传,可以nil
	//  @return error
	Bind(ctx context.Context, uid string, custSessData map[string]any, callback map[string]string) error
	// BindBackend
	//  @Description: bind session in stateful backend 注意业务层若当前服务是frontend时请勿调用。frontend时仅框架内自己调用
	//  @param ctx
	//  @param targetServerType 要绑定的stateful backend服务
	//  @param targetServerID 要绑定的stateful backend id
	//  @param callback 回调数据,通知其他服务时透传
	//  @return error
	BindBackend(ctx context.Context, targetServerType string, targetServerID string, callback map[string]string) error
	// Kick 踢出session,先给客户端发送一个kick数据包然后 Close
	//  @param ctx
	//  @param callback 回调数据,通知其他服务时透传
	//  @param reason
	//  @return error
	Kick(ctx context.Context, callback map[string]string, reason ...CloseReason) error
	// KickBackend 解绑backend
	//  @param ctx
	//  @param targetServerType 目标服
	//  @param callback 回调数据,通知其他服务时透传
	//  @param reason
	//  @return error
	KickBackend(ctx context.Context, targetServerType string, callback map[string]string, reason ...CloseReason) error
	OnClose(c func()) error
	RemoteAddr() net.Addr
	// RemoteIPWithoutCache 实时获取客户端ip,而非缓存.非特殊业务场景请使用 RemoteIPText 或 RemoteIP
	//  @Description:
	//  @return netip.Addr
	//
	RemoteIPWithoutCache() netip.Addr
	RemoteIP() netip.Addr
	RemoteIPText() string
	Remove(key string) error
	// Set 设置用户自定义数据
	//  @param key
	//  @param value
	//  @return error
	Set(key string, value interface{}) error
	SetData(data map[string]interface{}) error
	HasKey(key string) bool
	// Get 获取用户自定义数据
	//  @param key
	//  @return interface{}
	Get(key string) interface{}
	GetData() map[string]interface{}
	Int(key string) int
	Int8(key string) int8
	Int16(key string) int16
	Int32(key string) int32
	Int64(key string) int64
	Uint(key string) uint
	Uint8(key string) uint8
	Uint16(key string) uint16
	Uint32(key string) uint32
	Uint64(key string) uint64
	Float32(key string) float32
	Float64(key string) float64
	String(key string) string
	Value(key string) interface{}
	// PushToFront
	//  推送session数据给网关,网关会同步本地session数据并刷新云端缓存
	PushToFront(ctx context.Context) error
	// GetBackends 获取绑定的后端backends
	//  @return map[string]string
	GetBackends() map[string]string
	// GetBackendID
	//  @Description:获取绑定的后端服务id
	//  @param svrType
	//  @return string
	GetBackendID(svrType string) string
	// Go 派发到该session独立线程,若已绑定uid则派发到user独立线程
	//  @see co.GoWithUser
	//  @param task
	//  @return error
	Go(ctx context.Context, task func(ctx context.Context)) error
}

// Session represents a client session, which can store data during the connection.
// All data is released when the low-level connection is broken.
// Session instance related to the client will be passed to Handler method in the
// context parameter.
//
//	仅限于框架内部使用
type Session interface {
	SessPublic
	// Deprecated: 用不到,除非定制frontend
	//  只有在frontend调用才有用
	GetOnCloseCallbacks() []func()
	GetSubscriptions() []*nats.Subscription
	// Deprecated: 用不到,除非定制frontend
	//  只有在frontend调用才有用
	//  @param callbacks
	SetOnCloseCallbacks(callbacks []func())
	SetIsFrontend(isFrontend bool)
	SetSubscriptions(subscriptions []*nats.Subscription)
	HasRequestsInFlight() bool
	GetRequestsInFlight() ReqInFlight
	SetRequestInFlight(reqID string, reqData string, inFlight bool)

	ResponseMID(ctx context.Context, mid uint, v interface{}, err ...bool) error
	// Deprecated: 内部方法请勿调用.上层请自行封装玩家数据,勿使用 Session 内部data.内部data的功能已改用于cluster session(redis)
	// GetData() map[string]interface{}
	// Deprecated: 内部方法请勿调用.上层请自行封装玩家数据,勿使用 Session 内部data.内部data的功能已改用于cluster session(redis)
	// SetData(data map[string]interface{}) error

	// GetDataEncoded 框架内部使用,请勿调用
	//  @private pitaya
	//  @return []byte
	GetDataEncoded() []byte
	// SetDataEncoded  框架内部使用,请勿调用
	//  @private pitaya
	//  @param encodedData
	//  @return error
	SetDataEncoded(encodedData []byte) error
	// SetFrontendData  框架内部使用,请勿调用
	//  @private pitaya
	//  @param frontendID
	//  @param frontendSessionID
	SetFrontendData(frontendID string, frontendSessionID int64)
	SetIP(ip string)
	// Close  框架内部使用,请勿调用.Use Kick instead
	//  @private pitaya
	//  @param callback 回调数据,通知其他服务时透传
	//  @param reason
	Close(callback map[string]string, reason ...CloseReason)
	Clear()
	SetHandshakeData(data *HandshakeData)
	GetHandshakeData() *HandshakeData
	ValidateHandshake(data *HandshakeData) error
	GetHandshakeValidators() map[string]func(data *HandshakeData) error
	// SendRequestToFrontend 发送请求到网关,会携带session数据
	//  @param ctx
	//  @param route
	//  @param msg
	//  @return *protos.Response
	//  @return error
	SendRequestToFrontend(ctx context.Context, route string, msg proto.Message) (*protos.Response, error)
	ClusterStorageKey() string
	GetClusterStorage() CacheInterface
	// FlushFrontendData 网关的绑定关系/ip/online写入cache
	//  @return error
	FlushFrontendData() error
	// FlushOnline 是否在线写入cahce 在线1离线0
	//  @return error
	FlushOnline() error
	// FlushBackendData backends绑定数据写入cache
	//  @return error
	FlushBackendData() error
	// FlushUserData 用户自定义数据写入cache
	//  @return error
	FlushUserData() error
	// ObtainFromCluster 从存储服务获取并解包session数据
	//  @receiver s
	//  @return error
	ObtainFromCluster() error
	// InitialFromCluster 从存储服务获取并解包session数据,排除不允许初始化的数据,仅用于bind时调用
	//  @receiver s
	//  @return error
	InitialFromCluster() error
	SetBackends(bs map[string]string)
	SetBackendID(svrType string, id string)
	RemoveBackendID(svrType string)
}

type sessionIDService struct {
	sid int64
}

func newSessionIDService() *sessionIDService {
	return &sessionIDService{
		sid: 0,
	}
}

// SessionID returns the session id
func (c *sessionIDService) sessionID() int64 {
	return atomic.AddInt64(&c.sid, 1)
}

// NewSession
//
//	@implement SessionPool.NewSession
func (pool *sessionPoolImpl) NewSession(entity networkentity.NetworkEntity, frontend bool, UID ...string) (sess Session, isNew bool) {
	// stateful类型的backend服务会绑定并缓存session 所以这里有缓存直接取缓存
	if len(UID) > 0 {
		sess := pool.GetSessionByUID(UID[0])
		if sess != nil {
			return sess, false
		}
	}
	uid := ""
	uidInt := int64(0)
	if len(UID) > 0 {
		uid = UID[0]
		uidInt = util.ForceIdStrToInt(uid)
	}
	s := &sessionImpl{
		sessionLifecycleOwner: sessionLifecycleOwner{
			lastTime:         time.Now().Unix(),
			OnCloseCallbacks: []func(){},
		},
		sessionDataOwner: sessionDataOwner{
			data:       make(map[string]any),
			IsFrontend: frontend,
		},
		id:               pool.sessionIDSvc.sessionID(),
		entity:           entity,
		pool:             pool,
		requestsInFlight: ReqInFlight{m: make(map[string]string), mu: new(sync.RWMutex)},
	}
	s.identity.Store(&sessionIdentity{uid: uid, uidInt: uidInt, online: true})
	// sessionstick 的 backend，在 BindBackend()时保存,这里只处理 frontend
	if frontend {
		pool.storeSessionByID(s)
	}
	return s, true
}

// NewSessionPool returns a new session pool instance
func NewSessionPool() SessionPool {
	return &sessionPoolImpl{
		sessionBindCallbacks: make([]OnSessionBindFunc, 0),
		afterBindCallbacks:   make([]OnSessionBindFunc, 0),
		handshakeValidators: handshakeValidatorRegistry{
			validators: make(map[string]func(data *HandshakeData) error),
		},
		SessionCloseCallbacks:     make([]OnSessionCloseFunc, 0),
		sessionIDSvc:              newSessionIDService(),
		afterBindBackendCallbacks: make([]OnSessionBindBackendFunc, 0),
		afterKickBackendCallbacks: make([]OnSessionKickBackendFunc, 0),
	}
}

func (pool *sessionPoolImpl) GetSessionCount() int64 {
	return atomic.LoadInt64(&pool.SessionCount)
}
func (pool *sessionPoolImpl) GetUserCount() int64 {
	return atomic.LoadInt64(&pool.UserCount)
}

func (pool *sessionPoolImpl) GetSessionCloseCallbacks() []OnSessionCloseFunc {
	return pool.SessionCloseCallbacks
}

// GetSessionByUID return a session bound to an user id
func (pool *sessionPoolImpl) GetSessionByUID(uid string) Session {
	// TODO: Block this operation in backend servers
	if val, ok := pool.sessionsByUID.Load(uid); ok {
		return val.(Session)
	}
	return nil
}

// GetSessionByID return a session bound to a frontend server id
func (pool *sessionPoolImpl) GetSessionByID(id int64) Session {
	// TODO: Block this operation in backend servers
	if val, ok := pool.sessionsByID.Load(id); ok {
		return val.(Session)
	}
	return nil
}

// OnSessionBind adds a method to be called when a session is bound
// same function cannot be added twice!
func (pool *sessionPoolImpl) OnSessionBind(f OnSessionBindFunc) {
	// Prevents the same function to be added twice in onSessionBind
	sf1 := reflect.ValueOf(f)
	for _, fun := range pool.sessionBindCallbacks {
		sf2 := reflect.ValueOf(fun)
		if sf1.Pointer() == sf2.Pointer() {
			return
		}
	}
	pool.sessionBindCallbacks = append(pool.sessionBindCallbacks, f)
}
func (pool *sessionPoolImpl) SetBackendCoordinator(coordinator BackendCoordinator) {
	if coordinator.Bind == nil || coordinator.Kick == nil {
		panic("session: backend coordinator requires bind and kick handlers")
	}
	if !pool.backendCoordinator.CompareAndSwap(nil, &coordinator) {
		panic("session: backend coordinator already registered")
	}
}
func (pool *sessionPoolImpl) OnAfterBindBackend(f OnSessionBindBackendFunc) {
	// Prevents the same function to be added twice in onSessionBind
	sf1 := reflect.ValueOf(f)
	for _, fun := range pool.afterBindBackendCallbacks {
		sf2 := reflect.ValueOf(fun)
		if sf1.Pointer() == sf2.Pointer() {
			return
		}
	}
	pool.afterBindBackendCallbacks = append(pool.afterBindBackendCallbacks, f)
}
func (pool *sessionPoolImpl) OnAfterKickBackend(f OnSessionKickBackendFunc) {
	// Prevents the same function to be added twice in onSessionBind
	sf1 := reflect.ValueOf(f)
	for _, fun := range pool.afterKickBackendCallbacks {
		sf2 := reflect.ValueOf(fun)
		if sf1.Pointer() == sf2.Pointer() {
			return
		}
	}
	pool.afterKickBackendCallbacks = append(pool.afterKickBackendCallbacks, f)
}

// OnAfterSessionBind adds a method to be called when session is bound and after all sessionBind callbacks
func (pool *sessionPoolImpl) OnAfterSessionBind(f OnSessionBindFunc) {
	// Prevents the same function to be added twice in onSessionBind
	sf1 := reflect.ValueOf(f)
	for _, fun := range pool.afterBindCallbacks {
		sf2 := reflect.ValueOf(fun)
		if sf1.Pointer() == sf2.Pointer() {
			return
		}
	}
	pool.afterBindCallbacks = append(pool.afterBindCallbacks, f)
}

// OnSessionClose adds a method that will be called when every session closes
func (pool *sessionPoolImpl) OnSessionClose(f OnSessionCloseFunc) {
	sf1 := reflect.ValueOf(f)
	for _, fun := range pool.SessionCloseCallbacks {
		sf2 := reflect.ValueOf(fun)
		if sf1.Pointer() == sf2.Pointer() {
			return
		}
	}
	pool.SessionCloseCallbacks = append(pool.SessionCloseCallbacks, f)
}

// CloseAll calls Close on all sessions
func (pool *sessionPoolImpl) CloseAll() {
	pool.closeAll(false)
}

// ForceCloseAll closes every current session without waiting for in-flight
// requests. App shutdown calls this only after connection admission and all
// connection handlers have stopped, so no new frontend session can race in.
func (pool *sessionPoolImpl) ForceCloseAll() {
	pool.closeAll(true)
}

func (pool *sessionPoolImpl) closeAll(force bool) {
	logger.Log.Infof("closing all sessions, %d sessions", pool.GetSessionCount())
	for pool.GetSessionCount() > 0 {
		pool.sessionsByID.Range(func(_, value interface{}) bool {
			s := value.(Session)
			if !force && s.HasRequestsInFlight() {
				reqsInFlight := s.GetRequestsInFlight()
				reqsInFlight.mu.RLock()
				for _, route := range reqsInFlight.m {
					logger.Log.Debugf("Session for user %s is waiting on a response for route %s from a remote server. Delaying session close.", s.UID(), route)
				}
				reqsInFlight.mu.RUnlock()
				return true
			}
			s.Close(nil)
			return true
		})
		remaining := pool.GetSessionCount()
		logger.Log.Debugf("%d sessions remaining", remaining)
		if force {
			if remaining > 0 {
				logger.Log.Warnf("force close returned with %d sessions still registered", remaining)
			}
			break
		}
		if remaining > 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	logger.Log.Info("finished closing sessions")
}

// AddHandshakeValidator allows adds validation functions that will run when
// handshake packets are processed. Errors will be raised with the given name.
func (pool *sessionPoolImpl) AddHandshakeValidator(name string, f func(data *HandshakeData) error) {
	pool.handshakeValidators.add(name, f)
}

func (pool *sessionPoolImpl) EncodeSessionData(data map[string]interface{}) ([]byte, error) {
	return json.Marshal(data)
}

func (pool *sessionPoolImpl) DecodeSessionData(encodedData []byte) (map[string]interface{}, error) {
	var data map[string]interface{}
	if len(encodedData) == 0 {
		return data, nil
	}
	err := json.Unmarshal(encodedData, &data)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return data, nil
}

// StoreSessionLocal
// @implement SessionPool.StoreSessionLocal
//
//	@receiver pool
//	@param session
//	@return error
func (pool *sessionPoolImpl) StoreSessionLocal(session Session) error {
	if local, ok := session.(*sessionImpl); ok {
		local.lifecycleMutex.Lock()
		defer local.lifecycleMutex.Unlock()
		if local.closed {
			return errors.WithStack(constants.ErrCloseClosedSession)
		}
		if local.binding {
			return errors.WithStack(constants.ErrSessionAlreadyBound)
		}
		identity := local.identitySnapshot()
		if identity.uid == "" {
			return errors.WithStack(constants.ErrEmptyUID)
		}
		pool.sessionsMutex.Lock()
		defer pool.sessionsMutex.Unlock()
		if local.registeredUID != "" && local.registeredUID != identity.uid {
			pool.removeSessionByUIDLocked(local.registeredUID, session)
		}
		pool.storeSessionByIDLocked(local.id, session)
		pool.storeSessionByUIDLocked(identity.uid, session)
		local.registeredUID = identity.uid
		return nil
	}
	uid := session.UID()
	if uid == "" {
		return errors.WithStack(constants.ErrEmptyUID)
	}
	return pool.storeSessionLocal(uid, session.ID(), session)
}

func (pool *sessionPoolImpl) storeSessionLocal(uid string, id int64, session Session) error {
	if uid == "" {
		return errors.WithStack(constants.ErrEmptyUID)
	}
	pool.sessionsMutex.Lock()
	defer pool.sessionsMutex.Unlock()
	pool.storeSessionByIDLocked(id, session)
	pool.storeSessionByUIDLocked(uid, session)
	return nil
}

// RemoveSessionLocal
// @implement SessionPool.RemoveSessionLocal
//
//	@receiver pool
//	@param session
func (pool *sessionPoolImpl) RemoveSessionLocal(session Session) {
	if local, ok := session.(*sessionImpl); ok {
		local.lifecycleMutex.Lock()
		defer local.lifecycleMutex.Unlock()
		if local.binding {
			return
		}
		pool.removeSessionLocal(local.registeredUID, local.id, session)
		local.registeredUID = ""
		return
	}
	uid := session.UID()
	id := session.ID()
	pool.removeSessionLocal(uid, id, session)
}

func (pool *sessionPoolImpl) removeSessionLocal(uid string, id int64, session Session) {
	pool.sessionsMutex.Lock()
	defer pool.sessionsMutex.Unlock()
	if uid != "" {
		pool.removeSessionByUIDLocked(uid, session)
	}
	pool.removeSessionByIDLocked(id, session)
}

func (pool *sessionPoolImpl) storeSessionByUIDLocked(uid string, session Session) (Session, bool) {
	previous, loaded := pool.sessionsByUID.Swap(uid, session)
	if !loaded {
		atomic.AddInt64(&pool.UserCount, 1)
		return nil, false
	}
	return previous.(Session), true
}

func (pool *sessionPoolImpl) removeSessionByUID(uid string, session Session) bool {
	pool.sessionsMutex.Lock()
	defer pool.sessionsMutex.Unlock()
	return pool.removeSessionByUIDLocked(uid, session)
}

func (pool *sessionPoolImpl) removeSessionByUIDLocked(uid string, session Session) bool {
	if !pool.sessionsByUID.CompareAndDelete(uid, session) {
		return false
	}
	atomic.AddInt64(&pool.UserCount, -1)
	return true
}

func (pool *sessionPoolImpl) storeSessionByID(session Session) {
	id := session.ID()
	pool.sessionsMutex.Lock()
	defer pool.sessionsMutex.Unlock()
	pool.storeSessionByIDLocked(id, session)
}

func (pool *sessionPoolImpl) storeSessionByIDLocked(id int64, session Session) {
	_, loaded := pool.sessionsByID.Swap(id, session)
	if !loaded {
		atomic.AddInt64(&pool.SessionCount, 1)
	}
}

func (pool *sessionPoolImpl) removeSessionByID(session Session) bool {
	id := session.ID()
	pool.sessionsMutex.Lock()
	defer pool.sessionsMutex.Unlock()
	return pool.removeSessionByIDLocked(id, session)
}

func (pool *sessionPoolImpl) removeSessionByIDLocked(id int64, session Session) bool {
	if !pool.sessionsByID.CompareAndDelete(id, session) {
		return false
	}
	atomic.AddInt64(&pool.SessionCount, -1)
	return true
}

func (pool *sessionPoolImpl) SetClusterCache(storage CacheInterface) {
	if storage == nil {
		panic("pitaya/session: cluster cache must not be nil")
	}
	if storage.CacheTTL() <= 0 {
		panic("pitaya/session: cache TTL must be greater than zero")
	}
	pool.storage = storage
}

func (pool *sessionPoolImpl) RangeUsers(f func(uid string, sess SessPublic) bool) {
	pool.sessionsByUID.Range(func(k, v any) bool {
		return f(k.(string), v.(SessPublic))
	})
}
func (pool *sessionPoolImpl) RangeSessions(f func(id int64, sess SessPublic) bool) {
	pool.sessionsByID.Range(func(k, v any) bool {
		return f(k.(int64), v.(SessPublic))
	})
}

func (pool *sessionPoolImpl) getSessionStorageKey(uid string) string {
	return fmt.Sprintf(cacheKeyPitayaSession, uid)
}

func (s *sessionImpl) updateEncodedData() error {
	var b []byte
	b, err := s.pool.EncodeSessionData(s.data)
	if err != nil {
		return errors.WithStack(err)
	}
	s.encodedData = b
	return nil
}

// GetOnCloseCallbacks ...
func (s *sessionImpl) GetOnCloseCallbacks() []func() {
	s.lifecycleMutex.RLock()
	defer s.lifecycleMutex.RUnlock()
	return append([]func(){}, s.OnCloseCallbacks...)
}

// GetIsFrontend ...
func (s *sessionImpl) GetIsFrontend() bool {
	s.RLock()
	defer s.RUnlock()
	return s.IsFrontend
}
func (s *sessionImpl) GetFrontendID() string {
	s.RLock()
	defer s.RUnlock()
	return s.frontendID
}
func (s *sessionImpl) GetFrontendSessionID() int64 {
	s.RLock()
	defer s.RUnlock()
	return s.frontendSessionID
}

// GetSubscriptions ...
func (s *sessionImpl) GetSubscriptions() []*nats.Subscription {
	s.lifecycleMutex.RLock()
	defer s.lifecycleMutex.RUnlock()
	return append([]*nats.Subscription(nil), s.Subscriptions...)
}

// SetOnCloseCallbacks ...
func (s *sessionImpl) SetOnCloseCallbacks(callbacks []func()) {
	s.lifecycleMutex.Lock()
	defer s.lifecycleMutex.Unlock()
	s.OnCloseCallbacks = append([]func(){}, callbacks...)
}

// SetIsFrontend ...
func (s *sessionImpl) SetIsFrontend(isFrontend bool) {
	s.Lock()
	defer s.Unlock()
	s.IsFrontend = isFrontend
}

// SetSubscriptions ...
func (s *sessionImpl) SetSubscriptions(subscriptions []*nats.Subscription) {
	s.lifecycleMutex.Lock()
	if !s.closed {
		s.Subscriptions = append([]*nats.Subscription(nil), subscriptions...)
		s.lifecycleMutex.Unlock()
		return
	}
	uid := s.identitySnapshot().uid
	s.lifecycleMutex.Unlock()
	s.drainSubscriptions(uid, subscriptions)
}

// Push message to client
func (s *sessionImpl) Push(route string, v interface{}) error {
	return s.entity.Push(route, v)
}

// ResponseMID responses message to client, mid is
// request message ID
func (s *sessionImpl) ResponseMID(ctx context.Context, mid uint, v interface{}, err ...bool) error {
	return s.entity.ResponseMID(ctx, mid, v, err...)
}

// ID returns the session id
func (s *sessionImpl) ID() int64 {
	return s.id
}

// UID returns uid that bind to current session
func (s *sessionImpl) UID() string {
	return s.identitySnapshot().uid
}
func (s *sessionImpl) UIDInt() int64 {
	return s.identitySnapshot().uidInt
}

// GetData gets the data
func (s *sessionImpl) GetData() map[string]interface{} {
	s.RLock()
	defer s.RUnlock()
	data := map[string]interface{}{}
	for k, v := range s.data {
		data[k] = v
	}
	return data
}

// SetData 设置用户自定义数据
//
//	@receiver s
//	@param data
//	@return error
func (s *sessionImpl) SetData(data map[string]interface{}) error {
	s.Lock()
	defer s.Unlock()

	s.data = map[string]any{}
	for k, v := range data {
		s.data[k] = v
	}
	return s.updateEncodedData()
}

// GetDataEncoded returns the session data as an encoded value
func (s *sessionImpl) GetDataEncoded() []byte {
	s.RLock()
	defer s.RUnlock()
	return append([]byte(nil), s.encodedData...)
}

// SetDataEncoded sets the whole session data from an encoded value
func (s *sessionImpl) SetDataEncoded(encodedData []byte) error {
	if len(encodedData) == 0 {
		return nil
	}
	data, err := s.pool.DecodeSessionData(encodedData)
	if err != nil {
		return err
	}
	return s.SetData(data)
}

// SetFrontendData sets frontend id and session id
func (s *sessionImpl) SetFrontendData(frontendID string, frontendSessionID int64) {
	s.Lock()
	defer s.Unlock()
	s.frontendID = frontendID
	s.frontendSessionID = frontendSessionID
}

func (s *sessionImpl) SetIP(ip string) {
	s.Lock()
	defer s.Unlock()
	s.ip = ip
}

// Bind bind UID to current session
func (s *sessionImpl) Bind(ctx context.Context, uid string, custSessData map[string]any, callback map[string]string) error {
	if uid == "" {
		return constants.ErrIllegalUID
	}
	newBinding, err := s.prepareBind(uid)
	if err != nil {
		return errors.WithStack(err)
	}
	bindSucceeded := false
	defer func() {
		s.finishBind(uid, newBinding, bindSucceeded)
	}()
	if len(custSessData) > 0 {
		err = s.SetData(custSessData)
		if err != nil {
			return errors.WithStack(fmt.Errorf("%w,uid=%s", err, uid))
		}
	}
	for _, cb := range s.pool.sessionBindCallbacks {
		err = cb(ctx, s, callback)
		if err != nil {
			return err
		}
	}
	if s.isClosed() {
		return errors.WithStack(constants.ErrCloseClosedSession)
	}

	// if code running on frontend server
	if s.GetIsFrontend() {
		// Publish the replacement first so the old session cannot remove it while closing.
		previous, loaded, err := s.registerSessionByUID(uid)
		if err != nil {
			return errors.WithStack(err)
		}
		bindSucceeded = true
		if loaded && previous != s {
			logger.Zap.Warn("session already bind,closing", zap.String("uid", uid))
			// 应标记为重新绑定使其他服务不执行关闭回调
			previous.Close(nil, CloseReasonKickRebind)
		}
		if s.isClosed() {
			s.pool.removeSessionByUID(uid, s)
			return errors.WithStack(constants.ErrCloseClosedSession)
		}
	} else {
		// If frontentID is set this means it is a remote call and the current server
		// is not the frontend server that received the user request
		err = s.bindInFront(ctx, callback)
		if err != nil {
			logger.Zap.Error("error while trying to push session to front", zap.Error(err))
			return err
		}
		bindSucceeded = true

		// 绑定成功 同步数据到当前session 否则后续从context中获取的session拿不到boundData数据
		err = s.ObtainFromCluster()
		if err != nil {
			return err
		}
	}

	// invoke after callbacks on session bound
	for _, cb := range s.pool.afterBindCallbacks {
		_ = cb(ctx, s, callback)
	}

	if s.isClosed() {
		return errors.WithStack(constants.ErrCloseClosedSession)
	}
	return nil
}

// BindBackend
//
//	@implement Session.BindBackend
//	@receiver s
//	@param ctx
//	@param targetServerType
//	@param targetServerID
//	@return error
func (s *sessionImpl) BindBackend(ctx context.Context, targetServerType string, targetServerID string, callback map[string]string) error {
	if s.UID() == "" {
		return errors.WithStack(constants.ErrIllegalUID)
	}
	backendID, token, bound := s.setBackendIDIfAbsent(targetServerType, targetServerID)
	if !bound {
		return errors.WithStack(fmt.Errorf("%w,uid=%s,bound=%s,target=%s", constants.ErrSessionAlreadyBound, s.UID(), backendID, targetServerID))
	}
	rollback := BackendRollback(func() bool {
		return s.removeBackendIDIfToken(token)
	})
	if coordinator := s.pool.backendCoordinator.Load(); coordinator != nil {
		if err := coordinator.Bind(ctx, s, targetServerType, targetServerID, callback, rollback); err != nil {
			logger.Zap.Error("error while trying to bind backend", zap.Error(err))
			rollback()
			return err
		}
	}
	var err error
	for _, cb := range s.pool.afterBindBackendCallbacks {
		err = cb(ctx, s, targetServerType, targetServerID, callback)
		if err != nil {
			break
		}
	}
	return nil
}

// Kick kicks the user
func (s *sessionImpl) Kick(ctx context.Context, callback map[string]string, reason ...CloseReason) error {
	err := s.entity.Kick(ctx, reason...)
	if err != nil {
		return err
	}
	return s.closeOnce(callback, reason...)
}

// KickBackend
//
//	@implement Session.KickBackend
//	@receiver s
//	@param ctx
//	@param targetServerType
//	@param reason
//	@return error
func (s *sessionImpl) KickBackend(ctx context.Context, targetServerType string, callback map[string]string, reason ...CloseReason) error {
	if s.UID() == "" {
		return errors.WithStack(constants.ErrIllegalUID)
	}
	token, bound := s.takeBackendID(targetServerType)
	if !bound {
		return errors.WithStack(constants.ErrSessionNotBoundBackend)
	}
	rea := CloseReasonNormal
	if len(reason) > 0 {
		rea = reason[0]
	}
	rollback := BackendRollback(func() bool {
		return s.restoreBackendIDIfToken(token)
	})
	if coordinator := s.pool.backendCoordinator.Load(); coordinator != nil {
		if err := coordinator.Kick(ctx, s, targetServerType, token.id, callback, rea, rollback); err != nil {
			logger.Zap.Error("error while trying to kick backend", zap.Error(err))
			rollback()
			return err
		}
	}
	for _, cb := range s.pool.afterKickBackendCallbacks {
		if err := cb(ctx, s, targetServerType, token.id, callback, rea); err != nil {
			return err
		}
	}
	return nil
}

// OnClose adds the function it receives to the callbacks that will be called
// when the session is closed
func (s *sessionImpl) OnClose(c func()) error {
	if !s.GetIsFrontend() {
		return constants.ErrOnCloseBackend
	}
	s.lifecycleMutex.Lock()
	defer s.lifecycleMutex.Unlock()
	s.OnCloseCallbacks = append(s.OnCloseCallbacks, c)
	return nil
}

// Close terminates current session, session related data will not be released,
// all related data should be cleared explicitly in Session closed callback
func (s *sessionImpl) Close(callback map[string]string, reason ...CloseReason) {
	_ = s.closeOnce(callback, reason...)
}

func (s *sessionImpl) closeOnce(callback map[string]string, reason ...CloseReason) error {
	uid, subscriptions, ok := s.beginClose()
	if !ok {
		return nil
	}
	logger.Zap.Debug("session close", zap.Int64("id", s.ID()), zap.String("uid", uid))
	s.pool.removeSessionLocal(uid, s.id, s)
	s.drainSubscriptions(uid, subscriptions)
	return s.entity.Close(callback, reason...)
}

func (s *sessionImpl) prepareBind(uid string) (bool, error) {
	s.lifecycleMutex.Lock()
	defer s.lifecycleMutex.Unlock()
	if s.closed {
		return false, constants.ErrCloseClosedSession
	}
	if s.binding {
		return false, fmt.Errorf("%w,uid=%s", constants.ErrSessionAlreadyBound, uid)
	}
	identity := s.identitySnapshot()
	if identity.uid != "" {
		if identity.uid != uid {
			return false, fmt.Errorf("%w,uid=%s", constants.ErrSessionAlreadyBound, uid)
		}
		s.binding = true
		return false, nil
	}
	identity.uid = uid
	identity.uidInt = util.ForceIdStrToInt(uid)
	s.publishIdentityLocked(identity)
	s.binding = true
	return true, nil
}

func (s *sessionImpl) finishBind(uid string, newBinding bool, succeeded bool) {
	s.lifecycleMutex.Lock()
	defer s.lifecycleMutex.Unlock()
	s.binding = false
	identity := s.identitySnapshot()
	if !succeeded && newBinding && !s.closed && identity.uid == uid {
		identity.uid = ""
		identity.uidInt = 0
		s.publishIdentityLocked(identity)
	}
}

func (s *sessionImpl) registerSessionByUID(uid string) (Session, bool, error) {
	s.lifecycleMutex.Lock()
	defer s.lifecycleMutex.Unlock()
	if s.closed {
		return nil, false, constants.ErrCloseClosedSession
	}
	identity := s.identitySnapshot()
	if identity.uid == "" {
		return nil, false, constants.ErrEmptyUID
	}
	if identity.uid != uid {
		return nil, false, fmt.Errorf("%w,uid=%s", constants.ErrSessionAlreadyBound, uid)
	}

	s.pool.sessionsMutex.Lock()
	defer s.pool.sessionsMutex.Unlock()
	if s.registeredUID != "" && s.registeredUID != uid {
		s.pool.removeSessionByUIDLocked(s.registeredUID, s)
	}
	previous, loaded := s.pool.storeSessionByUIDLocked(uid, s)
	s.registeredUID = uid
	return previous, loaded, nil
}

func (s *sessionImpl) isClosed() bool {
	s.lifecycleMutex.RLock()
	defer s.lifecycleMutex.RUnlock()
	return s.closed
}

func (s *sessionImpl) beginClose() (string, []*nats.Subscription, bool) {
	s.lifecycleMutex.Lock()
	defer s.lifecycleMutex.Unlock()
	if s.closed {
		return "", nil, false
	}
	s.closed = true
	identity := s.identitySnapshot()
	identity.online = false
	s.publishIdentityLocked(identity)
	subscriptions := append([]*nats.Subscription(nil), s.Subscriptions...)
	s.Subscriptions = nil
	registeredUID := s.registeredUID
	s.registeredUID = ""
	return registeredUID, subscriptions, true
}

func (s *sessionImpl) drainSubscriptions(uid string, subscriptions []*nats.Subscription) {
	if !s.GetIsFrontend() {
		return
	}
	// TODO: this logic should be moved to nats rpc server
	for _, sub := range subscriptions {
		if err := sub.Drain(); err != nil {
			logger.Zap.Error("error Drain to user's messages channel: , this can cause performance and leak issues", zap.Error(err))
		} else {
			logger.Zap.Debug("successfully Drain to user's messages channel", zap.String("uid", uid), zap.String("subject", sub.Subject))
		}
	}
}

func (s *sessionImpl) setOnline(online bool) {
	s.lifecycleMutex.Lock()
	if !s.closed || !online {
		identity := s.identitySnapshot()
		if identity.online != online {
			identity.online = online
			s.publishIdentityLocked(identity)
		}
	}
	s.lifecycleMutex.Unlock()
}

func (s *sessionImpl) identitySnapshot() sessionIdentity {
	if identity := s.identity.Load(); identity != nil {
		return *identity
	}
	return sessionIdentity{}
}

// publishIdentityLocked publishes all hot-read identity fields as one version.
// The caller must hold lifecycleMutex for writes after construction.
func (s *sessionImpl) publishIdentityLocked(identity sessionIdentity) {
	s.identity.Store(&identity)
}

func (s *sessionImpl) lifecycleState() (string, int64, bool) {
	identity := s.identitySnapshot()
	return identity.uid, identity.uidInt, identity.online
}

// RemoteAddr returns the remote network address.
func (s *sessionImpl) RemoteAddr() net.Addr {
	return s.entity.RemoteAddr()
}
func (s *sessionImpl) RemoteIPWithoutCache() netip.Addr {
	return s.entity.RemoteIP()
}

func (s *sessionImpl) RemoteIP() netip.Addr {
	ip := netip.Addr{}
	err := ip.UnmarshalText([]byte(s.RemoteIPText()))
	if err != nil {
		logger.Zap.Error("unmarsha ip error", zap.Error(err))
	}
	return ip
}
func (s *sessionImpl) RemoteIPText() string {
	s.RLock()
	defer s.RUnlock()
	return s.ip
}

// Remove delete data associated with the key from session storage
func (s *sessionImpl) Remove(key string) error {
	s.Lock()
	defer s.Unlock()

	delete(s.data, key)
	return s.updateEncodedData()
}

// Set associates value with the key in session storage
func (s *sessionImpl) Set(key string, value interface{}) error {
	s.Lock()
	defer s.Unlock()

	s.data[key] = value
	return s.updateEncodedData()
}

// HasKey decides whether a key has associated value
func (s *sessionImpl) HasKey(key string) bool {
	s.RLock()
	defer s.RUnlock()

	_, has := s.data[key]
	return has
}

// Get returns a key value
func (s *sessionImpl) Get(key string) interface{} {
	s.RLock()
	defer s.RUnlock()

	v, ok := s.data[key]
	if !ok {
		return nil
	}
	return v
}

// Int returns the value associated with the key as a int.
func (s *sessionImpl) Int(key string) int {
	s.RLock()
	defer s.RUnlock()

	v, ok := s.data[key]
	if !ok {
		return 0
	}

	value, ok := v.(int)
	if !ok {
		return 0
	}
	return value
}

// Int8 returns the value associated with the key as a int8.
func (s *sessionImpl) Int8(key string) int8 {
	s.RLock()
	defer s.RUnlock()

	v, ok := s.data[key]
	if !ok {
		return 0
	}

	value, ok := v.(int8)
	if !ok {
		return 0
	}
	return value
}

// Int16 returns the value associated with the key as a int16.
func (s *sessionImpl) Int16(key string) int16 {
	s.RLock()
	defer s.RUnlock()

	v, ok := s.data[key]
	if !ok {
		return 0
	}

	value, ok := v.(int16)
	if !ok {
		return 0
	}
	return value
}

// Int32 returns the value associated with the key as a int32.
func (s *sessionImpl) Int32(key string) int32 {
	s.RLock()
	defer s.RUnlock()

	v, ok := s.data[key]
	if !ok {
		return 0
	}

	value, ok := v.(int32)
	if !ok {
		return 0
	}
	return value
}

// Int64 returns the value associated with the key as a int64.
func (s *sessionImpl) Int64(key string) int64 {
	s.RLock()
	defer s.RUnlock()

	v, ok := s.data[key]
	if !ok {
		return 0
	}

	value, ok := v.(int64)
	if !ok {
		return 0
	}
	return value
}

// Uint returns the value associated with the key as a uint.
func (s *sessionImpl) Uint(key string) uint {
	s.RLock()
	defer s.RUnlock()

	v, ok := s.data[key]
	if !ok {
		return 0
	}

	value, ok := v.(uint)
	if !ok {
		return 0
	}
	return value
}

// Uint8 returns the value associated with the key as a uint8.
func (s *sessionImpl) Uint8(key string) uint8 {
	s.RLock()
	defer s.RUnlock()

	v, ok := s.data[key]
	if !ok {
		return 0
	}

	value, ok := v.(uint8)
	if !ok {
		return 0
	}
	return value
}

// Uint16 returns the value associated with the key as a uint16.
func (s *sessionImpl) Uint16(key string) uint16 {
	s.RLock()
	defer s.RUnlock()

	v, ok := s.data[key]
	if !ok {
		return 0
	}

	value, ok := v.(uint16)
	if !ok {
		return 0
	}
	return value
}

// Uint32 returns the value associated with the key as a uint32.
func (s *sessionImpl) Uint32(key string) uint32 {
	s.RLock()
	defer s.RUnlock()

	v, ok := s.data[key]
	if !ok {
		return 0
	}

	value, ok := v.(uint32)
	if !ok {
		return 0
	}
	return value
}

// Uint64 returns the value associated with the key as a uint64.
func (s *sessionImpl) Uint64(key string) uint64 {
	s.RLock()
	defer s.RUnlock()

	v, ok := s.data[key]
	if !ok {
		return 0
	}

	value, ok := v.(uint64)
	if !ok {
		return 0
	}
	return value
}

// Float32 returns the value associated with the key as a float32.
func (s *sessionImpl) Float32(key string) float32 {
	s.RLock()
	defer s.RUnlock()

	v, ok := s.data[key]
	if !ok {
		return 0
	}

	value, ok := v.(float32)
	if !ok {
		return 0
	}
	return value
}

// Float64 returns the value associated with the key as a float64.
func (s *sessionImpl) Float64(key string) float64 {
	s.RLock()
	defer s.RUnlock()

	v, ok := s.data[key]
	if !ok {
		return 0
	}

	value, ok := v.(float64)
	if !ok {
		return 0
	}
	return value
}

// String returns the value associated with the key as a string.
func (s *sessionImpl) String(key string) string {
	s.RLock()
	defer s.RUnlock()

	v, ok := s.data[key]
	if !ok {
		return ""
	}

	value, ok := v.(string)
	if !ok {
		return ""
	}
	return value
}

// stringUnsafe
//
//	@Description:非线程安全
//	@receiver s
//	@param key
//	@return string
func (s *sessionImpl) stringUnsafe(key string) string {
	v, ok := s.data[key]
	if !ok {
		return ""
	}

	value, ok := v.(string)
	if !ok {
		return ""
	}
	return value
}

// Value returns the value associated with the key as a interface{}.
func (s *sessionImpl) Value(key string) interface{} {
	s.RLock()
	defer s.RUnlock()

	return s.data[key]
}

func (s *sessionImpl) bindInFront(ctx context.Context, callback map[string]string) error {
	data := s.sessionDataOwner.snapshot()
	bindMsg := &protos.BindMsg{
		Uid:      s.UID(),
		Fid:      data.frontendID,
		Sid:      data.frontendSessionID,
		Metadata: callback,
		Session: &protos.Session{
			Data: data.encodedData,
		},
	}
	res, err := s.SendRequestToFrontend(ctx, constants.SessionBindRoute, bindMsg)
	if err != nil {
		return err
	}
	logger.Log.Debugf("%s Got response: %+v", constants.SessionBindRoute, res)
	return nil
}

// PushToFront updates the session in the frontend
func (s *sessionImpl) PushToFront(ctx context.Context) error {
	data := s.sessionDataOwner.snapshot()
	if data.isFrontend {
		return constants.ErrFrontSessionCantPushToFront
	}
	// rpcClient.Call虽然会填充session数据,但是frontend接收时不会也不应该把session proto数据覆盖local session,所以这里还是额外传送一份session
	_, err := s.SendRequestToFrontend(ctx, constants.SessionPushRoute, &protos.Session{
		Id:   data.frontendSessionID,
		Uid:  s.UID(),
		Data: data.encodedData,
	})
	return err
}

// Clear releases all data related to current session
func (s *sessionImpl) Clear() {
	s.lifecycleMutex.Lock()
	identity := s.identitySnapshot()
	identity.uid = ""
	identity.uidInt = 0
	s.publishIdentityLocked(identity)
	s.lifecycleMutex.Unlock()

	s.Lock()
	defer s.Unlock()
	s.data = map[string]interface{}{}
	s.updateEncodedData()
}

// SetHandshakeData sets the handshake data received by the client.
func (s *sessionImpl) SetHandshakeData(data *HandshakeData) {
	s.Lock()
	defer s.Unlock()

	s.handshakeData = data
}

// GetHandshakeData gets the handshake data received by the client.
func (s *sessionImpl) GetHandshakeData() *HandshakeData {
	s.RLock()
	defer s.RUnlock()
	return s.handshakeData
}

// GetHandshakeValidators return the handshake validators associated with the session.
func (s *sessionImpl) GetHandshakeValidators() map[string]func(data *HandshakeData) error {
	return s.pool.handshakeValidators.snapshot()
}

func (s *sessionImpl) ValidateHandshake(data *HandshakeData) error {
	for name, fun := range s.GetHandshakeValidators() {
		if err := fun(data); err != nil {
			return fmt.Errorf("failed to run '%s' validator: %w. SessionId=%d", name, err, s.ID())
		}
	}

	return nil
}

// SendRequest 发送数据到指定server,会携带网关数据
//
//	@receiver s
//	@param ctx
//	@param serverID
//	@param route
//	@param msg
//	@return *protos.Response
//	@return error
func (s *sessionImpl) SendRequest(ctx context.Context, serverID, route string, msg proto.Message) (*protos.Response, error) {
	b, err := proto.Marshal(msg)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return s.entity.SendRequest(ctx, serverID, route, b)
}
func (s *sessionImpl) SendRequestToFrontend(ctx context.Context, route string, msg proto.Message) (*protos.Response, error) {
	uid, _, online := s.lifecycleState()
	frontendID := s.GetFrontendID()
	if frontendID == "" {
		return nil, errors.New(fmt.Sprintf("frontendID is nil.sid=%d,uid=%s,online=%t,agent=%s,route=%s", s.ID(), uid, online, s.entity.NetworkEntityName(), route))
	}
	return s.SendRequest(ctx, frontendID, route, msg)
}

func (s *sessionImpl) GetBackends() map[string]string {
	return s.backendBindings.snapshot()
}
func (s *sessionImpl) GetBackendID(svrType string) string {
	return s.backendBindings.get(svrType)
}

func (b *backendBindings) snapshot() map[string]string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make(map[string]string, len(b.backends))
	for serverType, id := range b.backends {
		result[serverType] = id
	}
	return result
}

func (b *backendBindings) get(serverType string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.backends[serverType]
}

func (b *backendBindings) ensureLocked() {
	if b.backends == nil {
		b.backends = make(map[string]string)
	}
	if b.generations == nil {
		b.generations = make(map[string]uint64)
	}
}

func (b *backendBindings) advanceGenerationLocked(serverType string) uint64 {
	b.ensureLocked()
	b.generations[serverType]++
	return b.generations[serverType]
}

func (b *backendBindings) setIfAbsent(serverType string, id string) (string, backendMutationToken, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()
	if backendID := b.backends[serverType]; backendID != "" {
		return backendID, backendMutationToken{}, false
	}
	generation := b.advanceGenerationLocked(serverType)
	b.backends[serverType] = id
	return "", backendMutationToken{serverType: serverType, id: id, generation: generation}, true
}

func (s *sessionImpl) setBackendIDIfAbsent(svrType string, id string) (string, backendMutationToken, bool) {
	return s.backendBindings.setIfAbsent(svrType, id)
}

func (b *backendBindings) take(serverType string) (backendMutationToken, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()
	backendID := b.backends[serverType]
	if backendID == "" {
		return backendMutationToken{}, false
	}
	generation := b.advanceGenerationLocked(serverType)
	delete(b.backends, serverType)
	return backendMutationToken{serverType: serverType, id: backendID, generation: generation}, true
}

func (s *sessionImpl) takeBackendID(svrType string) (backendMutationToken, bool) {
	return s.backendBindings.take(svrType)
}

func (b *backendBindings) removeIfToken(token backendMutationToken) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()
	if b.generations[token.serverType] != token.generation || b.backends[token.serverType] != token.id {
		return false
	}
	b.advanceGenerationLocked(token.serverType)
	delete(b.backends, token.serverType)
	return true
}

func (s *sessionImpl) removeBackendIDIfToken(token backendMutationToken) bool {
	return s.backendBindings.removeIfToken(token)
}

func (b *backendBindings) restoreIfToken(token backendMutationToken) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()
	if b.generations[token.serverType] != token.generation || b.backends[token.serverType] != "" {
		return false
	}
	b.advanceGenerationLocked(token.serverType)
	b.backends[token.serverType] = token.id
	return true
}

func (s *sessionImpl) restoreBackendIDIfToken(token backendMutationToken) bool {
	return s.backendBindings.restoreIfToken(token)
}

func (b *backendBindings) removeIfEqual(serverType string, id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()
	if backendID, ok := b.backends[serverType]; !ok || backendID != id {
		return false
	}
	b.advanceGenerationLocked(serverType)
	delete(b.backends, serverType)
	return true
}

func (s *sessionImpl) RemoveBackendIDIfEqual(svrType string, id string) bool {
	return s.backendBindings.removeIfEqual(svrType, id)
}

// CompareAndRemoveBackendID performs an atomic identity-aware backend removal.
// Sessions without the narrow capability return an error; there is no racy
// GetBackendID/RemoveBackendID fallback.
func CompareAndRemoveBackendID(s Session, svrType string, id string) (bool, error) {
	remover, ok := s.(interface {
		RemoveBackendIDIfEqual(svrType string, id string) bool
	})
	if !ok {
		return false, errors.Errorf("session %T does not support atomic backend removal", s)
	}
	return remover.RemoveBackendIDIfEqual(svrType, id), nil
}

func (s *sessionImpl) SetBackends(bs map[string]string) {
	s.backendBindings.replace(bs)
}

func (b *backendBindings) replace(backends map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()

	next := make(map[string]string, len(backends))
	affected := make(map[string]struct{}, len(b.generations)+len(backends))
	// Replacing the whole map invalidates in-flight mutations for types whose
	// slots are currently empty as well.
	for svrType := range b.generations {
		affected[svrType] = struct{}{}
	}
	for svrType := range b.backends {
		affected[svrType] = struct{}{}
	}
	for svrType, id := range backends {
		next[svrType] = id
		affected[svrType] = struct{}{}
	}
	for svrType := range affected {
		b.advanceGenerationLocked(svrType)
	}
	b.backends = next
}

func (s *sessionImpl) SetBackendID(svrType string, id string) {
	s.backendBindings.set(svrType, id)
}

func (b *backendBindings) set(serverType string, id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()
	b.advanceGenerationLocked(serverType)
	b.backends[serverType] = id
}

func (s *sessionImpl) RemoveBackendID(svrType string) {
	s.backendBindings.remove(svrType)
}

func (b *backendBindings) remove(serverType string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensureLocked()
	b.advanceGenerationLocked(serverType)
	delete(b.backends, serverType)
}
func (s *sessionImpl) Online() bool {
	return s.identitySnapshot().online
}
func (s *sessionImpl) ClusterStorageKey() string {
	uid := s.UID()
	if uid == "" {
		return ""
	}
	return s.pool.getSessionStorageKey(uid)
}

func (s *sessionImpl) GetClusterStorage() CacheInterface {
	return s.pool.storage
}

func (s *sessionImpl) FlushFrontendData() error {
	uid, _, online := s.lifecycleState()
	if uid == "" {
		return errors.WithStack(constants.ErrIllegalUID)
	}
	data := s.sessionDataOwner.snapshot()
	err := s.pool.storage.Hmset(s.pool.getSessionStorageKey(uid), map[string]string{
		fieldKeyOnline:         lo.If(online, "1").Else("0"),
		fieldKeyFrontendID:     data.frontendID,
		fieldKeyFrontendSessID: strconv.FormatInt(data.frontendSessionID, 10),
		fieldKeyIP:             data.ip,
		fieldKeyData:           string(data.encodedData),
	})
	if err != nil {
		return err
	}
	return nil
}
func (s *sessionImpl) FlushOnline() error {
	uid, _, online := s.lifecycleState()
	if uid == "" {
		return errors.WithStack(constants.ErrIllegalUID)
	}
	err := s.pool.storage.Hset(s.pool.getSessionStorageKey(uid), fieldKeyOnline, lo.If(online, "1").Else("0"))
	if err != nil {
		return err
	}
	return nil
}

func (s *sessionImpl) FlushBackendData() error {
	uid := s.UID()
	if uid == "" {
		return errors.WithStack(constants.ErrIllegalUID)
	}
	backends, err := json.Marshal(s.backendBindings.snapshot())
	if err != nil {
		return errors.WithStack(err)
	}
	err = s.pool.storage.Hset(s.pool.getSessionStorageKey(uid), fieldKeyBackends, string(backends))
	if err != nil {
		return err
	}
	return nil
}

func (s *sessionImpl) FlushUserData() error {
	uid := s.UID()
	if uid == "" {
		return errors.WithStack(constants.ErrIllegalUID)
	}
	err := s.pool.storage.Hset(s.pool.getSessionStorageKey(uid), fieldKeyData, string(s.GetDataEncoded()))
	if err != nil {
		return err
	}
	return nil
}

// ObtainFromCluster
//
//	@Description:从存储服务获取并解包session数据
//	@receiver s
//	@return error 数据不存在时返回 [constants.ErrSessionNotFound]
func (s *sessionImpl) ObtainFromCluster() error {
	s.reset()
	cache, err := s.pool.storage.Hgetall(s.ClusterStorageKey())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return constants.ErrSessionNotFound
		}
		return err
	}
	if len(cache) == 0 {
		return constants.ErrSessionNotFound
	}
	for field, v := range cache {
		switch field {
		case fieldKeyFrontendID:
			s.Lock()
			s.frontendID = v
			s.Unlock()
		case fieldKeyFrontendSessID:
			frontendSessionID, parseErr := strconv.ParseInt(v, 10, 64)
			err = parseErr
			if err != nil {
				logger.Zap.Error("parse frontend id error", zap.Error(err))
			} else {
				s.Lock()
				s.frontendSessionID = frontendSessionID
				s.Unlock()
			}
		case fieldKeyBackends:
			bsData, err := s.pool.DecodeSessionData([]byte(v))
			if err != nil {
				return err
			}
			s.SetBackends(util.MapStrInter2MapStrStr(bsData))
		case fieldKeyData:
			data, err := s.pool.DecodeSessionData([]byte(v))
			if err != nil {
				return err
			}
			if err = s.SetData(data); err != nil {
				return err
			}
		case fieldKeyIP:
			s.SetIP(v)
		case fieldKeyOnline:
			s.setOnline(v == "1")
		}
	}
	return nil
}

func (s *sessionImpl) reset() {
	s.setOnline(false)
}

// InitialFromCluster 仅供bind时使用,仅恢复backend绑定关系和用户自定义数据
func (s *sessionImpl) InitialFromCluster() error {
	cache, err := s.pool.storage.Hgetall(s.ClusterStorageKey())
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for field, v := range cache {
		switch field {
		case fieldKeyBackends:
			bsData, err := s.pool.DecodeSessionData([]byte(v))
			if err != nil {
				return err
			}
			s.SetBackends(util.MapStrInter2MapStrStr(bsData))
		case fieldKeyData:
			data, err := s.pool.DecodeSessionData([]byte(v))
			if err != nil {
				return err
			}
			if err = s.SetData(data); err != nil {
				return err
			}
		}
	}
	return nil
}
func FrontendSessHash(frontendID string, sid int64) int64 {
	return int64(crc32.ChecksumIEEE([]byte(fmt.Sprintf("%s-%d", frontendID, sid))))
}

func (s *sessionImpl) Go(ctx context.Context, task func(ctx context.Context)) error {
	uidInt := s.UIDInt()
	if uidInt != 0 {
		_, err := co.GoWithUser(ctx, uidInt, task)
		return err
	}
	data := s.sessionDataOwner.snapshot()
	sid := lo.If(data.isFrontend, s.id).Else(FrontendSessHash(data.frontendID, data.frontendSessionID))
	_, err := co.GoWithID(ctx, sid, task, co.WithPoolName(co.SessionGoPoolName))
	return err
}

func (s *sessionImpl) HasRequestsInFlight() bool {
	s.requestsInFlight.mu.RLock()
	defer s.requestsInFlight.mu.RUnlock()
	return len(s.requestsInFlight.m) != 0
}

func (s *sessionImpl) GetRequestsInFlight() ReqInFlight {
	return s.requestsInFlight
}

func (s *sessionImpl) SetRequestInFlight(reqID string, reqData string, inFlight bool) {
	s.requestsInFlight.mu.Lock()
	if inFlight {
		s.requestsInFlight.m[reqID] = reqData
	} else {
		if _, ok := s.requestsInFlight.m[reqID]; ok {
			delete(s.requestsInFlight.m, reqID)
		}
	}
	s.requestsInFlight.mu.Unlock()
}
