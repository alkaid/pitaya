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
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net"
	"net/netip"
	"reflect"
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
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
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
	// Deprecated: 用不到,除非定制frontend
	//  只有在frontend调用才有用
	//  @param callbacks
	SetOnCloseCallbacks(callbacks []func())
	SetIsFrontend(isFrontend bool)
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
