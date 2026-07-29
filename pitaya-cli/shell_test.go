package main

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/alkaid/pitaya/v2/client"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/session"
)

func TestParseCommandLinePreservesPayloadWhitespace(t *testing.T) {
	tests := []struct {
		name string
		line string
		want parsedCommand
	}{
		{
			name: "request JSON",
			line: `request connector.room.send {"message": "hello world", "nested": {"value": 1}}`,
			want: parsedCommand{
				name: "request",
				args: []string{"connector.room.send", `{"message": "hello world", "nested": {"value": 1}}`},
			},
		},
		{
			name: "notify text",
			line: "notify connector.room.send plain text with spaces",
			want: parsedCommand{
				name: "notify",
				args: []string{"connector.room.send", "plain text with spaces"},
			},
		},
		{
			name: "handshake JSON",
			line: `sethandshake {"sys": {"platform": "ios device"}}`,
			want: parsedCommand{
				name: "sethandshake",
				args: []string{`{"sys": {"platform": "ios device"}}`},
			},
		},
		{
			name: "handshake field",
			line: `  sethandshake platform "ios device"  `,
			want: parsedCommand{name: "sethandshake", args: []string{"platform", "ios device"}},
		},
		{
			name: "quoted address",
			line: `connect "localhost:3250"`,
			want: parsedCommand{name: "connect", args: []string{"localhost:3250"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCommandLine(tt.line)
			if err != nil {
				t.Fatalf("parseCommandLine() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseCommandLine() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestCommandHelpPreservesCommandSet(t *testing.T) {
	help := commandHelpText()
	want := []string{
		"clear",
		"connect",
		"disconnect",
		"exit",
		"help",
		"notify",
		"push",
		"request",
		"sethandshake",
	}

	last := -1
	for _, name := range want {
		index := strings.Index(help, name)
		if index == -1 {
			t.Fatalf("help does not contain %q:\n%s", name, help)
		}
		if index < last {
			t.Fatalf("help command %q is out of order:\n%s", name, help)
		}
		last = index
	}
	if strings.Contains(help, "routes") {
		t.Fatalf("help unexpectedly exposes the file-only routes command:\n%s", help)
	}
}

func TestCommandHistoryPersistsAndNavigates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	history := loadCommandHistory(path)
	if err := history.Add("help"); err != nil {
		t.Fatalf("Add(help) error = %v", err)
	}
	if err := history.Add(`request route {"message": "hello world"}`); err != nil {
		t.Fatalf("Add(request) error = %v", err)
	}

	if got, ok := history.Previous("draft"); !ok || got != `request route {"message": "hello world"}` {
		t.Fatalf("Previous() = %q, %v", got, ok)
	}
	if got, ok := history.Previous(""); !ok || got != "help" {
		t.Fatalf("second Previous() = %q, %v", got, ok)
	}
	if got, ok := history.Next(); !ok || got != `request route {"message": "hello world"}` {
		t.Fatalf("Next() = %q, %v", got, ok)
	}
	if got, ok := history.Next(); !ok || got != "draft" {
		t.Fatalf("second Next() = %q, %v", got, ok)
	}

	reloaded := loadCommandHistory(path)
	if !reflect.DeepEqual(reloaded.entries, history.entries) {
		t.Fatalf("reloaded entries = %#v, want %#v", reloaded.entries, history.entries)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("Stat(history) error = %v", err)
	} else if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("history permissions = %o, want no group/world access", info.Mode().Perm())
	}
}

func TestCommandHistoryAppendsAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	first := loadCommandHistory(path)
	second := loadCommandHistory(path)

	if err := first.Add("first"); err != nil {
		t.Fatalf("first Add() error = %v", err)
	}
	if err := second.Add("second"); err != nil {
		t.Fatalf("second Add() error = %v", err)
	}

	got := loadCommandHistory(path).entries
	want := []string{"first", "second"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("history entries = %#v, want %#v", got, want)
	}
}

func TestCommandHistoryRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("existing\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}

	path := filepath.Join(directory, "history")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	history := loadCommandHistory(path)
	if len(history.entries) != 0 {
		t.Fatalf("loaded entries through symlink = %#v, want none", history.entries)
	}
	if err := history.Add("help"); err == nil {
		t.Fatal("Add() error = nil, want symlink rejection")
	}

	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if got, want := string(contents), "existing\n"; got != want {
		t.Fatalf("target contents = %q, want %q", got, want)
	}
}

func TestReplModelInterruptAndEOFExitCodes(t *testing.T) {
	newModel := func() replModel {
		return newReplModel(&commandHistory{}, func(parsedCommand) commandResultMsg {
			return commandResultMsg{}
		})
	}

	model := newModel()
	updated, _ := model.Update(ctrlKey('c'))
	model = updated.(replModel)
	if model.interruptCount != 1 || model.exitCode != 0 {
		t.Fatalf("first Ctrl-C produced count=%d code=%d", model.interruptCount, model.exitCode)
	}

	updated, cmd := model.Update(ctrlKey('c'))
	model = updated.(replModel)
	if cmd == nil || model.exitCode != 1 {
		t.Fatalf("second Ctrl-C produced cmd=%v code=%d", cmd, model.exitCode)
	}

	model = newModel()
	updated, cmd = model.Update(ctrlKey('d'))
	model = updated.(replModel)
	if cmd == nil || model.exitCode != 0 {
		t.Fatalf("Ctrl-D produced cmd=%v code=%d", cmd, model.exitCode)
	}
}

func TestReplModelPromptsForMissingConnectAddress(t *testing.T) {
	model := newReplModel(&commandHistory{}, func(parsedCommand) commandResultMsg {
		return commandResultMsg{}
	})
	model.input.SetValue("connect")

	updated, cmd := model.submitInput()
	model = updated.(replModel)
	if cmd == nil {
		t.Fatal("submitInput() returned no command to retain the submitted line")
	}
	if !model.awaitingAddress || model.input.Prompt != addressPrompt {
		t.Fatalf("connect prompt state = awaiting %v, prompt %q", model.awaitingAddress, model.input.Prompt)
	}
}

func TestRunStreamREPL(t *testing.T) {
	var output strings.Builder
	code := runStreamREPL(strings.NewReader("help\nexit\n"), &output, &commandHistory{})
	if code != 0 {
		t.Fatalf("runStreamREPL() code = %d, want 0", code)
	}

	got := output.String()
	for _, want := range []string{replWelcome, ">>> help", "Commands:", ">>> exit"} {
		if !strings.Contains(got, want) {
			t.Fatalf("stream output does not contain %q:\n%s", want, got)
		}
	}
	if strings.ContainsRune(got, '\x1b') {
		t.Fatalf("stream output contains terminal escape sequences: %q", got)
	}
}

func TestRunStreamREPLReturnsFailureAfterCommandError(t *testing.T) {
	var output strings.Builder
	code := runStreamREPL(strings.NewReader("bogus-command\nexit\n"), &output, &commandHistory{})
	if code != 1 {
		t.Fatalf("runStreamREPL() code = %d, want 1", code)
	}
	if !strings.Contains(output.String(), "Error:") {
		t.Fatalf("stream output does not contain an error: %s", output.String())
	}
}

func TestRunStreamREPLReturnsFailureForMissingConnectAddress(t *testing.T) {
	var output strings.Builder
	code := runStreamREPL(strings.NewReader("connect\n"), &output, &commandHistory{})
	if code != 1 {
		t.Fatalf("runStreamREPL() code = %d, want 1", code)
	}
}

func TestWaitForResponseMatchesRequestID(t *testing.T) {
	responses := make(chan uint, 2)
	responses <- 41
	responses <- 42
	close(responses)

	disconnected := make(chan struct{})
	if err := waitForResponse(42, responses, disconnected); err != nil {
		t.Fatalf("waitForResponse() error = %v", err)
	}
}

func TestWaitForResponseConsumesQueuedMatchAfterReaderExit(t *testing.T) {
	responses := make(chan uint, 2)
	responses <- 41
	responses <- 42
	readerDone := make(chan struct{})
	close(readerDone)

	if err := waitForResponse(42, responses, readerDone); err != nil {
		t.Fatalf("waitForResponse() error = %v", err)
	}
}

func TestWaitForResponseFailsWhenConnectionCloses(t *testing.T) {
	responses := make(chan uint)
	disconnected := make(chan struct{}, 1)
	disconnected <- struct{}{}

	if err := waitForResponse(42, responses, disconnected); err == nil {
		t.Fatal("waitForResponse() error = nil, want closed connection error")
	}
}

func TestReadServerMessagesDrainsBeforeSingleDisconnectCallback(t *testing.T) {
	fake := &fakePitayaClient{messages: make(chan *message.Message, 1)}
	fake.messages <- &message.Message{ID: 42, Type: message.Response}
	connection := newServerConnection(fake)
	registerDisconnectHandler(connection)

	responses := make(chan uint, 1)
	callbackStarted := make(chan struct{})
	callbackRelease := make(chan struct{})
	var callbackCalls atomic.Int32
	callbacks := serverCallbacks{
		onMessage: func(response *message.Message) {
			responses <- response.ID
		},
		onDisconnect: func() {
			callbackCalls.Add(1)
			close(callbackStarted)
			<-callbackRelease
		},
	}

	fake.onDisconnected(client.CloseReasonError)
	fake.onDisconnected(client.CloseReasonError)
	go readServerMessages(connection, callbacks)

	<-callbackStarted
	select {
	case responseID := <-responses:
		if responseID != 42 {
			t.Fatalf("response ID = %d, want 42", responseID)
		}
	default:
		t.Fatal("queued response was not delivered before disconnect callback")
	}
	select {
	case <-connection.readerDone:
		t.Fatal("readerDone closed before disconnect callback completed")
	default:
	}

	close(callbackRelease)
	<-connection.readerDone
	if got := callbackCalls.Load(); got != 1 {
		t.Fatalf("disconnect callback calls = %d, want 1", got)
	}
}

type fakePitayaClient struct {
	onDisconnected func(client.CloseReason)
	messages       chan *message.Message
}

func (*fakePitayaClient) ConnectTo(string, ...*tls.Config) error { return nil }
func (*fakePitayaClient) ConnectedStatus() bool                  { return true }
func (*fakePitayaClient) Disconnect(client.CloseReason)          {}
func (f *fakePitayaClient) MsgChannel() chan *message.Message    { return f.messages }
func (*fakePitayaClient) SendNotify(string, []byte) error        { return nil }
func (*fakePitayaClient) SendRequest(string, []byte) (uint, error) {
	return 0, nil
}
func (*fakePitayaClient) SetClientHandshakeData(*session.HandshakeData) {}
func (f *fakePitayaClient) OnDisconnected(callback func(client.CloseReason)) {
	f.onDisconnected = callback
}

func ctrlKey(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: code, Mod: tea.ModCtrl})
}
