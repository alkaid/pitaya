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
	"net"
	"net/netip"
	"sync"

	"github.com/alkaid/pitaya/v2/logger"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

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

func (s *sessionImpl) updateEncodedData() error {
	var b []byte
	b, err := s.pool.EncodeSessionData(s.data)
	if err != nil {
		return errors.WithStack(err)
	}
	s.encodedData = b
	return nil
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

// SetIsFrontend ...
func (s *sessionImpl) SetIsFrontend(isFrontend bool) {
	s.Lock()
	defer s.Unlock()
	s.IsFrontend = isFrontend
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
