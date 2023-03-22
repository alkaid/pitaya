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
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/topfreegames/pitaya/v2/co"

	"github.com/topfreegames/pitaya/v2/util"

	nats "github.com/nats-io/nats.go"
	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/networkentity"
	"github.com/topfreegames/pitaya/v2/protos"
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

type BoundData struct {
	FrontendID string            `json:"_fid"` // 绑定的网关ID
	UID        string            `json:"_uid"`
	Backends   map[string]string `json:"_backends"` // 绑定的后端ID表 key:serverType value:serverID
}

type sessionPoolImpl struct {
	sessionBindCallbacks []OnSessionBindFunc
	afterBindCallbacks   []OnSessionBindFunc
	// SessionCloseCallbacks contains global session close callbacks
	SessionCloseCallbacks []OnSessionCloseFunc
	sessionsByUID         sync.Map
	sessionsByID          sync.Map
	sessionIDSvc          *sessionIDService
	// SessionCount keeps the current number of sessions
	SessionCount         int64
	UserCount            int64
	storage              CacheInterface
	bindBackendCallbacks []OnSessionBindBackendFunc
	kickBackendCallbacks []OnSessionKickBackendFunc
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
	OnBindBackend(f OnSessionBindBackendFunc)
	OnKickBackend(f OnSessionKickBackendFunc)
	CloseAll()
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
	sync.RWMutex                                  // protect data
	id                int64                       // session global unique id
	uid               string                      // binding user id
	lastTime          int64                       // last heartbeat time
	entity            networkentity.NetworkEntity // low-level network entity
	data              map[string]any              // session data store 用户自定义数据
	handshakeData     *HandshakeData              // handshake data received by the client
	encodedData       []byte                      // session data encoded as a byte array
	OnCloseCallbacks  []func()                    // onClose callbacks
	IsFrontend        bool                        // if session is a frontend session
	frontendID        string                      // the id of the frontend that owns the session
	frontendSessionID int64                       // the id of the session on the frontend server
	online            bool                        // 是否在线,仅cluster session有效
	backends          map[string]string           // 绑定的backends
	bsMutex           sync.RWMutex                // backends 的mutex
	ip                string                      // 远程客户端ip地址
	Subscriptions     []*nats.Subscription        // subscription created on bind when using nats rpc server  // subscription created on bind when using nats rpc server
	requestsInFlight  ReqInFlight
	pool              *sessionPoolImpl
}

type ReqInFlight struct {
	m  map[string]string
	mu sync.RWMutex
}

// SessPublic 供业务层使用的 Session
type SessPublic interface {
	GetIsFrontend() bool
	GetFrontendID() string
	GetFrontendSessionID() int64
	Push(route string, v interface{}) error
	ID() int64
	UID() string
	// Online 是否在线,用于cluster类型session的在线判断
	//  @return bool
	Online() bool
	// Bind 绑定session到他当前所在的frontend
	//  @param ctx
	//  @param uid
	//  @param callback 回调数据,通知其他服务时透传
	//  @return error
	Bind(ctx context.Context, uid string, callback map[string]string) error
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
	HasKey(key string) bool
	// Get 获取用户自定义数据
	//  @param key
	//  @return interface{}
	Get(key string) interface{}
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
	// GoBySession 根据session派发线程
	//  @see co.GoByUID or co.GoByID
	//  @param task
	GoBySession(task func())
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
	s := &sessionImpl{
		id:               pool.sessionIDSvc.sessionID(),
		entity:           entity,
		data:             make(map[string]any),
		handshakeData:    nil,
		lastTime:         time.Now().Unix(),
		OnCloseCallbacks: []func(){},
		IsFrontend:       frontend,
		pool:             pool,
		requestsInFlight: ReqInFlight{m: make(map[string]string)},
		backends:         map[string]string{},
		online:           true,
	}
	// sessionstick 的 backend，在 BindBackend()时保存,这里只处理 frontend
	if frontend {
		pool.sessionsByID.Store(s.id, s)
		atomic.AddInt64(&pool.SessionCount, 1)
	}
	if len(UID) > 0 {
		s.uid = UID[0]
	}
	return s, true
}

// NewSessionPool returns a new session pool instance
func NewSessionPool() SessionPool {
	return &sessionPoolImpl{
		sessionBindCallbacks:  make([]OnSessionBindFunc, 0),
		afterBindCallbacks:    make([]OnSessionBindFunc, 0),
		SessionCloseCallbacks: make([]OnSessionCloseFunc, 0),
		sessionIDSvc:          newSessionIDService(),
		bindBackendCallbacks:  make([]OnSessionBindBackendFunc, 0),
		kickBackendCallbacks:  make([]OnSessionKickBackendFunc, 0),
	}
}

func (pool *sessionPoolImpl) GetSessionCount() int64 {
	return pool.SessionCount
}
func (pool *sessionPoolImpl) GetUserCount() int64 {
	return pool.UserCount
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
func (pool *sessionPoolImpl) OnBindBackend(f OnSessionBindBackendFunc) {
	// Prevents the same function to be added twice in onSessionBind
	sf1 := reflect.ValueOf(f)
	for _, fun := range pool.bindBackendCallbacks {
		sf2 := reflect.ValueOf(fun)
		if sf1.Pointer() == sf2.Pointer() {
			return
		}
	}
	pool.bindBackendCallbacks = append(pool.bindBackendCallbacks, f)
}
func (pool *sessionPoolImpl) OnKickBackend(f OnSessionKickBackendFunc) {
	// Prevents the same function to be added twice in onSessionBind
	sf1 := reflect.ValueOf(f)
	for _, fun := range pool.kickBackendCallbacks {
		sf2 := reflect.ValueOf(fun)
		if sf1.Pointer() == sf2.Pointer() {
			return
		}
	}
	pool.kickBackendCallbacks = append(pool.kickBackendCallbacks, f)
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
	logger.Log.Infof("closing all sessions, %d sessions", pool.SessionCount)
	for pool.SessionCount > 0 {
		pool.sessionsByID.Range(func(_, value interface{}) bool {
			s := value.(Session)
			if s.HasRequestsInFlight() {
				reqsInFlight := s.GetRequestsInFlight()
				reqsInFlight.mu.RLock()
				for _, route := range reqsInFlight.m {
					logger.Log.Debugf("Session for user %s is waiting on a response for route %s from a remote server. Delaying session close.", s.UID(), route)
				}
				reqsInFlight.mu.RUnlock()
				return false
			} else {
				s.Close(nil)
				return true
			}
		})
		logger.Log.Debugf("%d sessions remaining", pool.SessionCount)
		if pool.SessionCount > 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	logger.Log.Info("finished closing sessions")
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
	if len(session.UID()) <= 0 {
		return errors.WithStack(constants.ErrEmptyUID)
	}
	pool.sessionsByUID.Store(session.UID(), session)
	atomic.AddInt64(&pool.UserCount, 1)
	if _, ok := pool.sessionsByID.Load(session.ID()); ok {
		return nil
	}
	pool.sessionsByID.Store(session.ID(), session)
	atomic.AddInt64(&pool.SessionCount, 1)
	return nil
}

// RemoveSessionLocal
// @implement SessionPool.RemoveSessionLocal
//
//	@receiver pool
//	@param session
func (pool *sessionPoolImpl) RemoveSessionLocal(session Session) {
	if len(session.UID()) > 0 {
		pool.sessionsByUID.Delete(session.UID())
		atomic.AddInt64(&pool.UserCount, -1)
	}
	if _, ok := pool.sessionsByID.Load(session.ID()); ok {
		return
	}
	atomic.AddInt64(&pool.SessionCount, -1)
	pool.sessionsByID.Delete(session.ID())
}

func (pool *sessionPoolImpl) SetClusterCache(storage CacheInterface) {
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
	return s.OnCloseCallbacks
}

// GetIsFrontend ...
func (s *sessionImpl) GetIsFrontend() bool {
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
	return s.Subscriptions
}

// SetOnCloseCallbacks ...
func (s *sessionImpl) SetOnCloseCallbacks(callbacks []func()) {
	s.OnCloseCallbacks = callbacks
}

// SetIsFrontend ...
func (s *sessionImpl) SetIsFrontend(isFrontend bool) {
	s.IsFrontend = isFrontend
}

// SetSubscriptions ...
func (s *sessionImpl) SetSubscriptions(subscriptions []*nats.Subscription) {
	s.Subscriptions = subscriptions
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
	return s.uid
}

// GetData gets the data
func (s *sessionImpl) GetData() map[string]interface{} {
	s.RLock()
	defer s.RUnlock()

	return s.data
}

// SetData 设置用户自定义数据
//
//	@receiver s
//	@param data
//	@return error
func (s *sessionImpl) SetData(data map[string]interface{}) error {
	s.Lock()
	defer s.Unlock()

	s.data = data
	return s.updateEncodedData()
}

// GetDataEncoded returns the session data as an encoded value
func (s *sessionImpl) GetDataEncoded() []byte {
	return s.encodedData
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
	s.frontendID = frontendID
	s.frontendSessionID = frontendSessionID
}

func (s *sessionImpl) SetIP(ip string) {
	s.ip = ip
}

// Bind bind UID to current session
func (s *sessionImpl) Bind(ctx context.Context, uid string, callback map[string]string) error {
	if uid == "" {
		return constants.ErrIllegalUID
	}
	var err error
	if s.UID() != "" && s.UID() != uid {
		return errors.WithStack(fmt.Errorf("%w,uid=%s", constants.ErrSessionAlreadyBound, uid))
	}

	s.uid = uid
	for _, cb := range s.pool.sessionBindCallbacks {
		err = cb(ctx, s, callback)
		if err != nil {
			s.uid = ""
			return err
		}
	}
	for _, cb := range s.pool.afterBindCallbacks {
		err = cb(ctx, s, callback)
		if err != nil {
			s.uid = ""
			return err
		}
	}

	// if code running on frontend server
	if s.IsFrontend {
		s.pool.sessionsByUID.Store(uid, s)
		atomic.AddInt64(&s.pool.UserCount, 1)
	} else {
		// If frontentID is set this means it is a remote call and the current server
		// is not the frontend server that received the user request
		err = s.bindInFront(ctx, callback)
		if err != nil {
			logger.Zap.Error("error while trying to push session to front", zap.Error(err))
			s.uid = ""
			return err
		}

		// 绑定成功 同步数据到当前session 否则后续从context中获取的session拿不到boundData数据
		err = s.ObtainFromCluster()
		if err != nil {
			return err
		}
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
	backendID := s.GetBackendID(targetServerType)
	if backendID != "" {
		return errors.WithStack(fmt.Errorf("%w,uid=%s,bound=%s,target=%s", constants.ErrSessionAlreadyBound, s.UID(), backendID, targetServerID))
	}
	s.SetBackendID(targetServerType, targetServerID)
	var err error
	for _, cb := range s.pool.bindBackendCallbacks {
		err = cb(ctx, s, targetServerType, targetServerID, callback)
		if err != nil {
			break
		}
	}
	// 回滚
	if err != nil {
		logger.Zap.Error("error while trying to bind backend", zap.Error(err))
		s.RemoveBackendID(targetServerType)
		return err
	}
	return nil
}

// Kick kicks the user
func (s *sessionImpl) Kick(ctx context.Context, callback map[string]string, reason ...CloseReason) error {
	err := s.entity.Kick(ctx, reason...)
	if err != nil {
		return err
	}
	// TODO 这里理应调用session.close(),不知道为什么原来是这样，测试时注意下是否有问题
	// return s.entity.Close(callback, reason...)
	s.Close(callback, reason...)
	return nil
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
	var err error = nil
	if s.UID() == "" {
		return errors.WithStack(constants.ErrIllegalUID)
	}
	backendID := s.GetBackendID(targetServerType)
	if backendID == "" {
		return errors.WithStack(constants.ErrSessionNotBoundBackend)
	}
	rea := CloseReasonNormal
	if len(reason) > 0 {
		rea = reason[0]
	}
	s.RemoveBackendID(targetServerType)
	for _, cb := range s.pool.kickBackendCallbacks {
		err := cb(ctx, s, targetServerType, backendID, callback, rea)
		if err != nil {
			s.uid = ""
			return err
		}
	}
	if err != nil {
		logger.Zap.Error("error while trying to bind backend", zap.Error(err))
		// 回滚
		s.SetBackendID(targetServerType, backendID)
		return err
	}
	return nil
}

// OnClose adds the function it receives to the callbacks that will be called
// when the session is closed
func (s *sessionImpl) OnClose(c func()) error {
	if !s.IsFrontend {
		return constants.ErrOnCloseBackend
	}
	s.OnCloseCallbacks = append(s.OnCloseCallbacks, c)
	return nil
}

// Close terminates current session, session related data will not be released,
// all related data should be cleared explicitly in Session closed callback
func (s *sessionImpl) Close(callback map[string]string, reason ...CloseReason) {
	logger.Zap.Debug("session close", zap.Int64("id", s.ID()), zap.String("uid", s.UID()))
	atomic.AddInt64(&s.pool.SessionCount, -1)
	s.online = false
	s.pool.sessionsByID.Delete(s.ID())
	// 须校验存的session和要关闭的是否同一个session，相同才清uid-session map中的值。否则互相频繁顶号时会有误删的异步问题
	oldSession := s.pool.GetSessionByUID(s.UID())
	if oldSession != nil && oldSession.ID() == s.ID() {
		s.pool.sessionsByUID.Delete(s.UID())
		atomic.AddInt64(&s.pool.UserCount, -1)
	} else {
		var oldID int64
		if oldSession != nil {
			oldID = oldSession.ID()
		}
		logger.Zap.Debug("stored session not equal current session,ignore delete stored session", zap.Int64("oldid", oldID), zap.Int64("id", s.ID()), zap.String("uid", s.UID()))
	}
	// TODO: this logic should be moved to nats rpc server
	if s.IsFrontend && s.Subscriptions != nil && len(s.Subscriptions) > 0 {
		// if the user is bound to an userid and nats rpc server is being used we need to unsubscribe
		for _, sub := range s.Subscriptions {
			err := sub.Drain()
			if err != nil {
				logger.Zap.Error("error Drain to user's messages channel: , this can cause performance and leak issues", zap.Error(err))
			} else {
				logger.Zap.Debug("successfully Drain to user's messages channel", zap.String("uid", s.UID()), zap.String("subject", sub.Subject))
			}
		}
	}
	s.entity.Close(callback, reason...)
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
	err := ip.UnmarshalText([]byte(s.ip))
	if err != nil {
		logger.Zap.Error("unmarsha ip error", zap.Error(err))
	}
	return ip
}
func (s *sessionImpl) RemoteIPText() string {
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
	bindMsg := &protos.BindMsg{
		Uid:      s.uid,
		Fid:      s.frontendID,
		Sid:      s.frontendSessionID,
		Metadata: callback,
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
	if s.IsFrontend {
		return constants.ErrFrontSessionCantPushToFront
	}
	// rpcClient.Call虽然会填充session数据,但是frontend接收时不会也不应该把session proto数据覆盖local session,所以这里还是额外传送一份session
	_, err := s.SendRequestToFrontend(ctx, constants.SessionPushRoute, &protos.Session{
		Id:   s.frontendSessionID,
		Uid:  s.uid,
		Data: s.encodedData,
	})
	return err
}

// Clear releases all data related to current session
func (s *sessionImpl) Clear() {
	s.Lock()
	defer s.Unlock()

	s.uid = ""
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
	return s.handshakeData
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
	return s.SendRequest(ctx, s.frontendID, route, msg)
}

func (s *sessionImpl) GetBackends() map[string]string {
	bs := map[string]string{}
	s.bsMutex.RLock()
	defer s.bsMutex.RUnlock()
	for k, v := range s.backends {
		bs[k] = v
	}
	return bs
}
func (s *sessionImpl) GetBackendID(svrType string) string {
	s.bsMutex.RLock()
	defer s.bsMutex.RUnlock()
	return s.backends[svrType]
}
func (s *sessionImpl) SetBackends(bs map[string]string) {
	s.bsMutex.Lock()
	defer s.bsMutex.Unlock()
	s.backends = bs
}
func (s *sessionImpl) SetBackendID(svrType string, id string) {
	s.bsMutex.Lock()
	defer s.bsMutex.Unlock()
	s.backends[svrType] = id
}
func (s *sessionImpl) RemoveBackendID(svrType string) {
	s.bsMutex.Lock()
	defer s.bsMutex.Unlock()
	delete(s.backends, svrType)
}
func (s *sessionImpl) Online() bool {
	return s.online
}
func (s *sessionImpl) ClusterStorageKey() string {
	if s.uid == "" {
		return ""
	}
	return s.pool.getSessionStorageKey(s.uid)
}

func (s *sessionImpl) GetClusterStorage() CacheInterface {
	return s.pool.storage
}

func (s *sessionImpl) FlushFrontendData() error {
	if "" == s.uid {
		return errors.WithStack(constants.ErrIllegalUID)
	}
	err := s.pool.storage.Hmset(s.ClusterStorageKey(), map[string]string{
		fieldKeyOnline:         lo.If(s.online, "1").Else("0"),
		fieldKeyFrontendID:     s.frontendID,
		fieldKeyFrontendSessID: strconv.FormatInt(s.frontendSessionID, 10),
		fieldKeyIP:             s.ip,
	})
	if err != nil {
		return err
	}
	return nil
}
func (s *sessionImpl) FlushOnline() error {
	if "" == s.uid {
		return errors.WithStack(constants.ErrIllegalUID)
	}
	err := s.pool.storage.Hset(s.ClusterStorageKey(), fieldKeyOnline, lo.If(s.online, "1").Else("0"))
	if err != nil {
		return err
	}
	return nil
}

func (s *sessionImpl) FlushBackendData() error {
	if "" == s.uid {
		return errors.WithStack(constants.ErrIllegalUID)
	}
	s.bsMutex.RLock()
	backends, err := json.Marshal(s.backends)
	s.bsMutex.RUnlock()
	if err != nil {
		return errors.WithStack(err)
	}
	err = s.pool.storage.Hset(s.ClusterStorageKey(), fieldKeyBackends, string(backends))
	if err != nil {
		return err
	}
	return nil
}

func (s *sessionImpl) FlushUserData() error {
	if "" == s.uid {
		return errors.WithStack(constants.ErrIllegalUID)
	}
	err := s.pool.storage.Hset(s.ClusterStorageKey(), fieldKeyData, string(s.encodedData))
	if err != nil {
		return err
	}
	return nil
}

// ObtainFromCluster
//
//	@Description:从存储服务获取并解包session数据
//	@receiver s
//	@return error
func (s *sessionImpl) ObtainFromCluster() error {
	cache, err := s.pool.storage.Hgetall(s.ClusterStorageKey())
	if err != nil {
		return err
	}
	for field, v := range cache {
		switch field {
		case fieldKeyFrontendID:
			s.frontendID = v
		case fieldKeyFrontendSessID:
			s.frontendSessionID, err = strconv.ParseInt(v, 10, 64)
			if err != nil {
				logger.Zap.Error("parse frontend id error", zap.Error(err))
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
			s.data = data
			err = s.updateEncodedData()
			if err != nil {
				return err
			}
		case fieldKeyIP:
			s.ip = v
		case fieldKeyOnline:
			s.online = v == "1"
		}
	}
	return nil
}

func (s *sessionImpl) InitialFromCluster() error {
	// 仅供bind时使用,仅恢复backend绑定关系和用户自定义数据
	cache, err := s.pool.storage.Hgetall(s.ClusterStorageKey())
	if err != nil {
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
			s.data = data
			err = s.updateEncodedData()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// GoBySession 根据session数据决策派发任务线程
//
//	@param task
func (s *sessionImpl) GoBySession(task func()) {
	if s.UID() != "" {
		co.GoByUID(s.UID(), task)
		return
	}
	goID := 0
	if s.ID() > 0 {
		goID = int(s.ID())
	}
	co.GoByID(goID, task)
}

func (s *sessionImpl) HasRequestsInFlight() bool {
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
