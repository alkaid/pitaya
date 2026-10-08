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

package pitaya

import (
	"testing"

	"github.com/alkaid/pitaya/v2/component"
	"github.com/alkaid/pitaya/v2/config"
	"github.com/stretchr/testify/assert"
)

type MyComp struct {
	component.Base
	running bool
}

func (m *MyComp) Init() {
	m.running = true
}

func (m *MyComp) Shutdown() {
	m.running = false
}

func TestRegister(t *testing.T) {
	config := config.NewDefaultPitayaConfig()
	app := NewDefaultApp(true, "testtype", Cluster, map[string]string{}, *config).(*App)
	b := &component.Base{}
	before := len(app.handlerComp)
	app.Register(b)
	assert.Equal(t, before+1, len(app.handlerComp))
	assert.Equal(t, regComp{b, nil}, app.handlerComp[before])
}

func TestRegisterRemote(t *testing.T) {
	config := config.NewDefaultPitayaConfig()
	app := NewDefaultApp(true, "testtype", Cluster, map[string]string{}, *config).(*App)
	before := app.remoteComp
	b := &component.Base{}
	app.RegisterRemote(b)
	assert.Equal(t, len(before)+1, len(app.remoteComp))
	assert.Equal(t, regComp{b, nil}, app.remoteComp[len(before)])
}

func TestStartupComponents(t *testing.T) {
	app := NewDefaultApp(true, "testtype", Standalone, map[string]string{}, *config.NewDefaultPitayaConfig()).(*App)

	handler, remote := &MyComp{}, &MyComp{}
	app.Register(handler)
	app.RegisterRemote(remote)
	app.startupComponents()
	assert.True(t, handler.running)
	assert.True(t, remote.running)
}

func TestShutdownComponents(t *testing.T) {
	app := NewDefaultApp(true, "testtype", Standalone, map[string]string{}, *config.NewDefaultPitayaConfig()).(*App)

	handler, remote := &MyComp{}, &MyComp{}
	app.Register(handler)
	app.RegisterRemote(remote)
	app.startupComponents()

	app.shutdownComponents()
	assert.False(t, handler.running)
	assert.False(t, remote.running)
}
