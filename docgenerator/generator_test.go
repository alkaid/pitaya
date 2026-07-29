// Copyright (c) TFG Co. All Rights Reserved.
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

package docgenerator

import (
	"context"
	"testing"
	"time"

	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/protos/test"
	"github.com/stretchr/testify/assert"
)

type MyComp struct {
	component.Base
}

type MyStruct struct {
	Str             string
	privateInt      int
	Int             int       `json:"int"`
	Time            time.Time `json:"time"`
	NotOnJSONString string    `json:"-"`
	Bytes           []byte    `json:"bytes"`
	Struct          *struct {
		Int        int `json:"int"`
		NotPointer struct {
			Str string `json:"str"`
			Int int
		} `json:"notPointer"`
	} `json:"struct"`
	Slice []*struct {
		Int int `json:"int"`
		Str string
	} `json:"slice"`
	NotPointer struct {
		Str string `json:"str"`
		Int int
	} `json:"notPointer"`
}

func (m *MyComp) Init() {}

func (m *MyComp) Shutdown() {}

func (m *MyComp) HandlerEmpty(ctx context.Context) {}

func (m *MyComp) HandlerRaw(ctx context.Context, b []byte) ([]byte, error) {
	return nil, nil
}

func (m *MyComp) HandlerOrRemoteStruct(ctx context.Context, s *MyStruct) (*MyStruct, error) {
	s.privateInt = 0
	return nil, nil
}

func (m *MyComp) RemoteStruct(ctx context.Context, ss *test.SomeStruct) (*test.SomeStruct, error) {
	return nil, nil
}

func pointerDoc(typeName string, getPtrNames bool) map[string]interface{} {
	if !getPtrNames {
		return map[string]interface{}{}
	}
	return map[string]interface{}{typeName: map[string]interface{}{}}
}

func expectedHandlersDoc(getPtrNames bool) map[string]interface{} {
	return map[string]interface{}{
		"metagame.MyComp.HandlerEmpty": map[string]interface{}{
			"input":  interface{}(nil),
			"output": []interface{}{},
		},
		"metagame.MyComp.HandlerRaw": map[string]interface{}{
			"input":  "[]byte",
			"output": []interface{}{"[]byte"},
		},
		"metagame.MyComp.HandlerOrRemoteStruct": map[string]interface{}{
			"input":  pointerDoc("*docgenerator.MyStruct", getPtrNames),
			"output": []interface{}{pointerDoc("*docgenerator.MyStruct", getPtrNames)},
		},
		"metagame.MyComp.RemoteStruct": map[string]interface{}{
			"input":  pointerDoc("*test.SomeStruct", getPtrNames),
			"output": []interface{}{pointerDoc("*test.SomeStruct", getPtrNames)},
		},
	}
}

func expectedRemotesDoc(getPtrNames bool) map[string]interface{} {
	return map[string]interface{}{
		"metagame.MyComp.RemoteStruct": map[string]interface{}{
			"input":  pointerDoc("*test.SomeStruct", getPtrNames),
			"output": []interface{}{pointerDoc("*test.SomeStruct", getPtrNames)},
		},
	}
}

func TestHandlersDoc(t *testing.T) {
	t.Parallel()

	handlerServices := map[string]*component.Service{}
	s := component.NewService(&MyComp{}, []component.Option{})
	err := s.ExtractHandler()
	assert.NoError(t, err)
	handlerServices[s.Name] = s

	doc, err := HandlersDocs("metagame", handlerServices, false)
	assert.NoError(t, err)
	assert.Equal(t, expectedHandlersDoc(false), doc)
}

func TestHandlersDocWithPointerNames(t *testing.T) {
	t.Parallel()

	handlerServices := map[string]*component.Service{}
	s := component.NewService(&MyComp{}, []component.Option{})
	err := s.ExtractHandler()
	assert.NoError(t, err)
	handlerServices[s.Name] = s

	doc, err := HandlersDocs("metagame", handlerServices, true)
	assert.NoError(t, err)
	assert.Equal(t, expectedHandlersDoc(true), doc)
}

func TestRemotesDoc(t *testing.T) {
	t.Parallel()

	remoteServices := map[string]*component.Service{}
	s := component.NewService(&MyComp{}, []component.Option{})
	err := s.ExtractRemote()
	assert.NoError(t, err)
	remoteServices[s.Name] = s

	doc, err := RemotesDocs("metagame", remoteServices, false)
	assert.NoError(t, err)
	assert.Equal(t, expectedRemotesDoc(false), doc)
}

func TestRemotesDocWithPointerNames(t *testing.T) {
	t.Parallel()

	remoteServices := map[string]*component.Service{}
	s := component.NewService(&MyComp{}, []component.Option{})
	err := s.ExtractRemote()
	assert.NoError(t, err)
	remoteServices[s.Name] = s

	doc, err := RemotesDocs("metagame", remoteServices, true)
	assert.NoError(t, err)
	assert.Equal(t, expectedRemotesDoc(true), doc)
}
