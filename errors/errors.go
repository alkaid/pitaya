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

package errors

import "errors"

// ErrUnknownCode is a string code representing an unknown error
// This will be used when no error code is sent by the handler
const ErrUnknownCode = "PIT-000"

// ErrInternalCode is a string code representing an internal Pitaya error
const ErrInternalCode = "PIT-500"

// ErrNotFoundCode is a string code representing a not found related error
const ErrNotFoundCode = "PIT-404"

// ErrBadRequestCode is a string code representing a bad request related error
const ErrBadRequestCode = "PIT-400"

// ErrClientClosedRequest is a string code representing the client closed request error
const ErrClientClosedRequest = "PIT-499"

//扩展的错误码 从1000开始

// ErrSessionNotFoundInServer 目标服务器上找不到该用户
const ErrSessionNotFoundInServer = "PIT-1000"

// Error is an error with a code, message and metadata
type Error struct {
	err      error
	Code     string
	Message  string
	Metadata map[string]string
}

var Is = errors.Is
var As = errors.As

func New(code string, msg string, metadata ...map[string]string) *Error {
	e := &Error{
		Code:    code,
		Message: msg,
	}
	if len(metadata) > 0 {
		e.Metadata = metadata[0]
	}
	return e
}

//Wrap 将 err 作为底层 error,若底层 error 已存在则覆盖
//  @receiver e
//  @param err
func (e *Error) Wrap(err error) {
	e.err = err
}

//Wrap 包装error
//  @param err 原error
//  @param code 业务code
//  @param msg
//  @param metadata
//  @return *Error
func Wrap(err error, code string, msg string, metadata ...map[string]string) *Error {
	e := &Error{
		Code:    code,
		Message: msg,
		err:     err,
	}
	if len(metadata) > 0 {
		e.Metadata = metadata[0]
	}
	if pitayaErr, ok := err.(*Error); ok {
		if len(pitayaErr.Metadata) > 0 {
			mergeMetadatas(e, pitayaErr.Metadata)
		}
	}
	return e
}

//WithCode Wrap with code 使用err的Message作为wrapped的Message
//  @param err
//  @param code
//  @return *Error
func WithCode(err error, code string) *Error {
	return Wrap(err, code, err.Error())
}

//WithMessage wrap with message 使用err的Code作为wrapped的Code
//  @param err
//  @param msg
//  @return *Error
func WithMessage(err error, msg string) *Error {
	code := ErrUnknownCode
	if pitayaErr, ok := err.(*Error); ok {
		code = pitayaErr.Code
	}
	return Wrap(err, code, msg)
}

// Deprecated: use Wrap instead
// 用err新建 Error. 和 Wrap 不同, NewError 不会包装原 err ,只会使用 err.Error()来作为自己的Error()输出
func NewError(err error, code string, metadata ...map[string]string) *Error {
	if pitayaErr, ok := err.(*Error); ok {
		if len(metadata) > 0 {
			mergeMetadatas(pitayaErr, metadata[0])
		}
		return pitayaErr
	}

	e := &Error{
		Code:    code,
		Message: err.Error(),
	}
	if len(metadata) > 0 {
		e.Metadata = metadata[0]
	}
	return e

}

//Error
//  @implement go builtin error.Error
//  @receiver e
//  @return string
func (e *Error) Error() string {
	return e.Message
}

//Unwrap implement Unwrap to use errors.Is & errors.As
//  @implement go builtin errors/wrap.go Unwrap
//  @receiver e
//  @return error
func (e *Error) Unwrap() error {
	return e.err
}

func mergeMetadatas(pitayaErr *Error, metadata map[string]string) {
	if pitayaErr.Metadata == nil {
		pitayaErr.Metadata = metadata
		return
	}

	for key, value := range metadata {
		pitayaErr.Metadata[key] = value
	}
}

// CodeFromError returns the code of error.
// If error is nil, return empty string.
// If error is not a pitaya error, returns unkown code
func CodeFromError(err error) string {
	if err == nil {
		return ""
	}

	pitayaErr, ok := err.(*Error)
	if !ok {
		return ErrUnknownCode
	}

	if pitayaErr == nil {
		return ""
	}

	return pitayaErr.Code
}
