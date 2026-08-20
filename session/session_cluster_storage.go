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
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/alkaid/pitaya/v2/constants"
	"github.com/alkaid/pitaya/v2/logger"
	"github.com/alkaid/pitaya/v2/util"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.uber.org/zap"
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

func (pool *sessionPoolImpl) SetClusterCache(storage CacheInterface) {
	if storage == nil {
		panic("pitaya/session: cluster cache must not be nil")
	}
	if storage.CacheTTL() <= 0 {
		panic("pitaya/session: cache TTL must be greater than zero")
	}
	pool.storage = storage
}

func (pool *sessionPoolImpl) getSessionStorageKey(uid string) string {
	return fmt.Sprintf(cacheKeyPitayaSession, uid)
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
