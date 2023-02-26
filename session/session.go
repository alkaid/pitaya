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
	cacheKeyPitayaSession  = "pit:s:%s:" // pitaya session key = pitaya:session:<uid>
	fieldKeyFrontendID     = "f"         // frontend id
	fieldKeyFrontendSessID = "s"         // frontend session id
	fieldKeyBackends       = "bs"        // backends id list
	fieldKeyUID            = "u"         // uid
	fieldKeyIP             = "ip"        // uid
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
	NewSession(entity networkentity.NetworkEntity, frontend bool, UID ...string) Session
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
	// ImperfectSessionFromCluster  框架内部使用,请勿调用
	//  @private pitaya
	//  @Description:从后端存储的session数据构造出一个不健全的session.
	//  "不健全"指session的agent仅有少数 networkentity.NetworkEntity 方法
	//  @receiver pool
	//  @param uid
	//  @return Session
	//  @return error
	ImperfectSessionFromCluster(uid string, entity networkentity.NetworkEntity) (Session, error)
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
	data              map[string]interface{}      // session data store
	boundData         *BoundData                  // session的绑定数据
	handshakeData     *HandshakeData              // handshake data received by the client
	encodedData       []byte                      // session data encoded as a byte array
	OnCloseCallbacks  []func()                    // onClose callbacks
	IsFrontend        bool                        // if session is a frontend session
	frontendID        string                      // the id of the frontend that owns the session
	frontendSessionID int64                       // the id of the session on the frontend server
	remoteIPText      string                      // 远程客户端ip地址

	Subscriptions []*nats.Subscription // subscription created on bind when using nats rpc server
	pool          *sessionPoolImpl
}

// SessPublic 供业务层使用的 Session
type SessPublic interface {
	GetIsFrontend() bool
	GetFrontendID() string
	GetFrontendSessionID() int64
	Push(route string, v interface{}) error
	ID() int64
	UID() string
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
	Set(key string, value interface{}) error
	HasKey(key string) bool
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
	// GetBoundData 获取绑定数据
	//  @return BoundData
	GetBoundData() *BoundData
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
	// Close  框架内部使用,请勿调用.Use Kick instead
	//  @private pitaya
	//  @param callback 回调数据,通知其他服务时透传
	//  @param reason
	Close(callback map[string]string, reason ...CloseReason)
	Clear()
	SetHandshakeData(data *HandshakeData)
	GetHandshakeData() *HandshakeData
	// Flush2Cluster
	//  @Description: 打包session数据到存储服务
	//  @receiver s
	//  @return error
	Flush2Cluster() error
	// ObtainFromCluster
	//  @Description:从存储服务获取并解包session数据
	//  @receiver s
	//  @return error
	ObtainFromCluster() error
	// InitialFromCluster
	//  @Description:从存储服务获取并解包session数据,排除不允许初始化的数据,仅用于bind时调用
	//  @receiver s
	//  @return error
	InitialFromCluster() error
	SetBackendID(svrType string, id string) error
	RemoveBackendID(svrType string) error
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
func (pool *sessionPoolImpl) NewSession(entity networkentity.NetworkEntity, frontend bool, UID ...string) Session {
	// stateful类型的backend服务会绑定并缓存session 所以这里有缓存直接取缓存
	if len(UID) > 0 {
		sess := pool.GetSessionByUID(UID[0])
		if sess != nil {
			return sess
		}
	}
	s := &sessionImpl{
		id:               pool.sessionIDSvc.sessionID(),
		entity:           entity,
		data:             make(map[string]interface{}),
		handshakeData:    nil,
		lastTime:         time.Now().Unix(),
		OnCloseCallbacks: []func(){},
		IsFrontend:       frontend,
		pool:             pool,
		boundData: &BoundData{
			Backends: make(map[string]string),
		},
	}
	// sessionstick 的 backend，在 BindBackend()时保存,这里只处理 frontend
	if frontend {
		pool.sessionsByID.Store(s.id, s)
		atomic.AddInt64(&pool.SessionCount, 1)
	}
	if len(UID) > 0 {
		s.uid = UID[0]
	}
	return s
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
	logger.Log.Debugf("closing all sessions, %d sessions", pool.SessionCount)
	pool.sessionsByID.Range(func(_, value interface{}) bool {
		s := value.(Session)
		s.Close(nil)
		return true
	})
	logger.Log.Debug("finished closing sessions")
}

func (pool *sessionPoolImpl) EncodeSessionData(data map[string]interface{}) ([]byte, error) {
	return json.Marshal(data)
}

func (pool *sessionPoolImpl) DecodeSessionData(encodedData []byte) (map[string]interface{}, error) {
	if len(encodedData) == 0 {
		return nil, nil
	}
	var data map[string]interface{}
	err := json.Unmarshal(encodedData, &data)
	if err != nil {
		return nil, err
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

// ImperfectSessionFromCluster
//
//	@implement SessionPool.ImperfectSessionFromCluster
//	TODO 逻辑移到 agent.Cluster
func (pool *sessionPoolImpl) ImperfectSessionFromCluster(uid string, entity networkentity.NetworkEntity) (Session, error) {
	v, err := pool.storage.Get(pool.getSessionStorageKey(uid))
	if err != nil {
		return nil, err
	}
	s := pool.NewSession(entity, false, uid)
	err = s.SetDataEncoded([]byte(v))
	if err != nil {
		return nil, err
	}
	return s, nil
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
	s.boundData.UID = s.uid
	s.boundData.FrontendID = s.frontendID
	v, ok := s.data[fieldKeyBackends]
	if !ok {
		s.boundData.Backends = make(map[string]string)
	} else {
		tmp := v.(map[string]interface{})
		s.boundData.Backends = util.MapStrInter2MapStrStr(tmp)
	}
	var b []byte
	b, err := s.pool.EncodeSessionData(s.data)
	if err != nil {
		return err
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

// Deprecated: 内部方法请勿调用 由于session数据的redis存储依赖于session.data,故这里不能让上层改变data实例
func (s *sessionImpl) SetData(data map[string]interface{}) error {
	s.Lock()
	defer s.Unlock()

	s.data = data
	s.frontendID = s.stringUnsafe(fieldKeyFrontendID)
	s.uid = s.stringUnsafe(fieldKeyUID)
	tmpId, err := strconv.Atoi(s.stringUnsafe(fieldKeyFrontendSessID))
	if err != nil {
		logger.Zap.Warn("", zap.Error(err))
	}
	s.frontendSessionID = int64(tmpId)
	s.remoteIPText = s.stringUnsafe(fieldKeyIP)
	return s.updateEncodedData()
}

// GetDataEncoded returns the session data as an encoded value
func (s *sessionImpl) GetDataEncoded() []byte {
	s.Lock()
	defer s.Unlock()
	// 打包
	s.data[fieldKeyUID] = s.uid
	s.data[fieldKeyFrontendID] = s.frontendID
	s.data[fieldKeyFrontendSessID] = strconv.Itoa(int(s.frontendSessionID))
	s.updateEncodedData()
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

func (s *sessionImpl) ipFromEntity() string {
	if s.remoteIPText == "" {
		ip, err := s.entity.RemoteIP().MarshalText()
		if err != nil {
			logger.Zap.Error("marshal ip error", zap.Error(err))
		}
		s.remoteIPText = string(ip)
	}
	return s.remoteIPText
}

// SetFrontendData sets frontend id and session id
func (s *sessionImpl) SetFrontendData(frontendID string, frontendSessionID int64) {
	s.Lock()
	defer s.Unlock()
	s.frontendID = frontendID
	s.frontendSessionID = frontendSessionID

	s.data[fieldKeyFrontendID] = frontendID
	s.data[fieldKeyFrontendSessID] = strconv.Itoa(int(s.frontendSessionID))
	s.data[fieldKeyIP] = s.ipFromEntity()
	err := s.updateEncodedData()
	if err != nil {
		logger.Zap.Error("set frontend data error", zap.Error(err))
	}
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
			return errors.WithStack(err)
		}
	}
	for _, cb := range s.pool.afterBindCallbacks {
		err = cb(ctx, s, callback)
		if err != nil {
			s.uid = ""
			return errors.WithStack(err)
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
			return errors.WithStack(err)
		}

		// 绑定成功 同步数据到当前session 否则后续从context中获取的session拿不到boundData数据
		err = s.ObtainFromCluster()
		if err != nil {
			return errors.WithStack(err)
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
			err := sub.Unsubscribe()
			if err != nil {
				logger.Zap.Error("error unsubscribing to user's messages channel: , this can cause performance and leak issues", zap.Error(err))
			} else {
				logger.Zap.Debug("successfully unsubscribed to user's messages channel", zap.String("uid", s.UID()), zap.String("subject", sub.Subject))
			}
		}
	}
	s.Subscriptions = nil
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
	err := ip.UnmarshalText([]byte(s.remoteIPText))
	if err != nil {
		logger.Zap.Error("unmarsha ip error", zap.Error(err))
	}
	return ip
}
func (s *sessionImpl) RemoteIPText() string {
	return s.remoteIPText
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
	// return s.sendRequestToFront(ctx, constants.SessionBindRoute, false)
	bindMsg := &protos.BindMsg{
		Uid:      s.uid,
		Fid:      s.frontendID,
		Sid:      s.frontendSessionID,
		Metadata: callback,
	}
	b, err := proto.Marshal(bindMsg)
	if err != nil {
		return err
	}
	res, err := s.entity.SendRequest(ctx, s.frontendID, constants.SessionBindRoute, b)
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
	return s.sendRequestToFront(ctx, constants.SessionPushRoute, true)
}

func (s *sessionImpl) PushToBackend(ctx context.Context) error {
	return nil
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

func (s *sessionImpl) sendRequestToFront(ctx context.Context, route string, includeData bool) error {
	sessionData := &protos.Session{
		Id:  s.frontendSessionID,
		Uid: s.uid,
	}
	if includeData {
		sessionData.Data = s.encodedData
	}
	b, err := proto.Marshal(sessionData)
	if err != nil {
		return errors.WithStack(err)
	}
	res, err := s.entity.SendRequest(ctx, s.frontendID, route, b)
	if err != nil {
		return err
	}
	logger.Log.Debugf("%s Got response: %+v", route, res)
	return nil
}
func (s *sessionImpl) sendRequestToBackend(ctx context.Context, route string, includeData bool, backendID string) error {
	sessionData := &protos.Session{
		Id:  s.frontendSessionID,
		Uid: s.uid,
	}
	if includeData {
		sessionData.Data = s.encodedData
	}
	b, err := proto.Marshal(sessionData)
	if err != nil {
		return err
	}
	res, err := s.entity.SendRequest(ctx, backendID, route, b)
	if err != nil {
		return err
	}
	logger.Log.Debugf("%s Got response: %+v", route, res)
	return nil
}

func (s *sessionImpl) GetBoundData() *BoundData {
	return s.boundData
}

// GetBackendID
//
//	@implement Session.GetBackendID
func (s *sessionImpl) GetBackendID(svrType string) string {
	s.RLock()
	defer s.RUnlock()
	backends, ok := s.data[fieldKeyBackends]
	if !ok {
		return ""
	}
	bid, ok := backends.(map[string]interface{})[svrType]
	if !ok {
		return ""
	}
	return bid.(string)
}
func (s *sessionImpl) SetBackendID(svrType string, id string) error {
	s.RLock()
	defer s.RUnlock()
	var backends map[string]interface{}
	v, ok := s.data[fieldKeyBackends]
	if !ok {
		backends = make(map[string]interface{})
	} else {
		backends = v.(map[string]interface{})
	}
	backends[svrType] = id
	s.data[fieldKeyBackends] = backends
	return s.updateEncodedData()
}
func (s *sessionImpl) RemoveBackendID(svrType string) error {
	s.Lock()
	defer s.Unlock()
	v, ok := s.data[fieldKeyBackends]
	var backends map[string]interface{}
	if !ok {
		return nil
	}
	backends = v.(map[string]interface{})
	delete(backends, svrType)
	return s.updateEncodedData()
}
func (s *sessionImpl) getSessionStorageKey() string {
	return s.pool.getSessionStorageKey(s.uid)
}

// Flush2Cluster
//
//	@Description: 打包session数据到存储服务
//	@receiver s
//	@return error
func (s *sessionImpl) Flush2Cluster() error {
	if "" == s.uid {
		return constants.ErrIllegalUID
	}
	// TODO GetDataEncoded 内部没有处理错误,本应修正,但是其他地方有引用 暂不改动这里后续考虑优化
	data := s.GetDataEncoded()
	// TODO 考虑是否需要redsync锁
	return s.pool.storage.Set(s.getSessionStorageKey(), string(data))
}

// ObtainFromCluster
//
//	@Description:从存储服务获取并解包session数据
//	@receiver s
//	@return error
func (s *sessionImpl) ObtainFromCluster() error {
	v, err := s.pool.storage.Get(s.getSessionStorageKey())
	if err != nil {
		return err
	}
	err = s.SetDataEncoded([]byte(v))
	if err != nil {
		return err
	}
	return nil
}
func (s *sessionImpl) InitialFromCluster() error {
	v, err := s.pool.storage.Get(s.getSessionStorageKey())
	if err != nil {
		return err
	}
	encodedData := []byte(v)
	if len(encodedData) == 0 {
		return nil
	}
	data, err := s.pool.DecodeSessionData(encodedData)
	if err != nil {
		return err
	}
	s.Lock()
	s.data = data
	s.remoteIPText = ""
	delete(s.data, fieldKeyIP)
	delete(s.data, fieldKeyFrontendID)
	delete(s.data, fieldKeyFrontendSessID)
	s.Unlock()
	return s.updateEncodedData()
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
