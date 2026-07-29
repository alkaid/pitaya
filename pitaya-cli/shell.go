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

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"text/tabwriter"
	"unicode"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/charmbracelet/x/term"
	"github.com/mattn/go-shellwords"
)

const (
	replPrompt       = ">>> "
	addressPrompt    = "address: "
	historyLimit     = 500
	maxCommandBytes  = 1024 * 1024
	replWelcome      = "Pitaya REPL Client"
	interruptWarning = "Input Ctrl-c once more to exit"
)

type commandDefinition struct {
	name string
	help string
}

var replCommands = []commandDefinition{
	{name: "clear", help: "clear the screen"},
	{name: "connect", help: "connects to pitaya"},
	{name: "disconnect", help: "disconnects from pitaya server"},
	{name: "exit", help: "exit the program"},
	{name: "help", help: "display help"},
	{name: "notify", help: "makes a notify to pitaya server"},
	{name: "push", help: "insert information of push return"},
	{name: "request", help: "makes a request to pitaya server"},
	{name: "sethandshake", help: "sets a handshake parameter"},
}

type parsedCommand struct {
	name string
	args []string
}

type commandAction uint8

const (
	commandContinue commandAction = iota
	commandExit
	commandClear
)

type commandResultMsg struct {
	action commandAction
	err    error
}

type replOutputMsg string

type serverCallbacks struct {
	onMessage    func(*message.Message)
	onDisconnect func()
}

type replModel struct {
	input           textinput.Model
	history         *commandHistory
	execute         func(parsedCommand) commandResultMsg
	busy            bool
	awaitingAddress bool
	interruptCount  int
	exitCode        int
	terminalWidth   int
}

func newReplModel(history *commandHistory, execute func(parsedCommand) commandResultMsg) replModel {
	input := textinput.New()
	input.Prompt = replPrompt
	input.ShowSuggestions = true
	input.SetSuggestions(replCommandNames())
	input.SetVirtualCursor(false)
	input.Focus()

	return replModel{
		input:   input,
		history: history,
		execute: execute,
	}
}

func (m replModel) Init() tea.Cmd {
	return m.input.Focus()
}

func (m replModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:gocyclo
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.terminalWidth = msg.Width
		m.resizeInput()
		return m, nil

	case replOutputMsg:
		return m, tea.Printf("%s", string(msg))

	case commandResultMsg:
		m.busy = false
		m.input.Focus()
		m.resizeInput()

		if msg.err != nil {
			return m, tea.Printf("Error: %s", msg.err)
		}

		switch msg.action {
		case commandExit:
			m.exitCode = 0
			return m, tea.Quit
		case commandClear:
			return m, tea.ClearScreen
		default:
			return m, nil
		}

	case tea.KeyPressMsg:
		key := msg.Keystroke()
		if key == "ctrl+c" {
			return m.handleInterrupt()
		}
		m.interruptCount = 0

		if m.busy {
			return m, nil
		}

		switch key {
		case "ctrl+d":
			if m.input.Value() == "" {
				m.exitCode = 0
				return m, tea.Quit
			}
		case "enter":
			return m.submitInput()
		case "up":
			if value, ok := m.history.Previous(m.input.Value()); ok {
				m.input.SetValue(value)
				m.input.CursorEnd()
			}
			return m, nil
		case "down":
			if value, ok := m.history.Next(); ok {
				m.input.SetValue(value)
				m.input.CursorEnd()
			}
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m replModel) View() tea.View {
	if m.busy {
		return tea.NewView("")
	}

	view := tea.NewView(m.input.View())
	view.Cursor = m.input.Cursor()
	return view
}

func (m replModel) handleInterrupt() (tea.Model, tea.Cmd) {
	m.interruptCount++
	if m.interruptCount >= 2 {
		m.exitCode = 1
		return m, tea.Sequence(tea.Println("Interrupted"), tea.Quit)
	}

	if !m.busy {
		m.awaitingAddress = false
		m.input.Reset()
		m.input.Prompt = replPrompt
		m.input.ShowSuggestions = true
		m.history.ResetNavigation()
		m.resizeInput()
	}

	return m, tea.Println(interruptWarning)
}

func (m replModel) submitInput() (tea.Model, tea.Cmd) {
	line := strings.TrimSpace(m.input.Value())
	if line == "" {
		return m, nil
	}

	prompt := m.input.Prompt
	_ = m.history.Add(line)

	if m.awaitingAddress {
		m.awaitingAddress = false
		m.input.Prompt = replPrompt
		m.input.ShowSuggestions = true
		return m.startCommand(parsedCommand{name: "connect", args: []string{line}}, prompt+line)
	}

	command, err := parseCommandLine(line)
	if err != nil {
		m.input.Reset()
		return m, tea.Sequence(
			tea.Printf("%s%s", prompt, line),
			tea.Printf("Error: %s", err),
		)
	}

	if command.name == "connect" && len(command.args) == 0 {
		m.awaitingAddress = true
		m.input.Reset()
		m.input.Prompt = addressPrompt
		m.input.ShowSuggestions = false
		m.resizeInput()
		return m, tea.Printf("%s%s", prompt, line)
	}

	return m.startCommand(command, prompt+line)
}

func (m replModel) startCommand(command parsedCommand, displayedLine string) (tea.Model, tea.Cmd) {
	m.busy = true
	m.input.Reset()
	m.history.ResetNavigation()

	return m, tea.Sequence(
		tea.Printf("%s", displayedLine),
		func() tea.Msg { return m.execute(command) },
	)
}

func (m *replModel) resizeInput() {
	if m.terminalWidth <= 0 {
		return
	}

	width := m.terminalWidth - len(m.input.Prompt)
	if width < 1 {
		width = 1
	}
	m.input.SetWidth(width)
}

func repl() int {
	history := loadCommandHistory(historyFilePath())
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return runStreamREPL(os.Stdin, os.Stdout, history)
	}

	return runInteractiveREPL(os.Stdin, os.Stdout, history)
}

func runInteractiveREPL(input io.Reader, output io.Writer, history *commandHistory) int {
	fmt.Fprintln(output, replWelcome)

	var program *tea.Program
	logger := messageLog{send: func(message string) {
		program.Send(replOutputMsg(message))
	}}
	execute := func(command parsedCommand) commandResultMsg {
		action, err := dispatchCommand(logger, command, serverCallbacks{
			onMessage: func(response *message.Message) {
				logger.Printf("sv->%s\n", string(parseData(response.Data)))
			},
		}, true)
		return commandResultMsg{action: action, err: err}
	}

	model := newReplModel(history, execute)
	program = tea.NewProgram(model, tea.WithInput(input), tea.WithOutput(output))
	finalModel, err := program.Run()
	if err != nil {
		if errors.Is(err, tea.ErrInterrupted) {
			fmt.Fprintln(output, "Interrupted")
		} else {
			fmt.Fprintf(output, "Error: %s\n", err)
		}
		return 1
	}

	if final, ok := finalModel.(replModel); ok {
		return final.exitCode
	}
	return 0
}

func runStreamREPL(input io.Reader, output io.Writer, history *commandHistory) int {
	fmt.Fprintln(output, replWelcome)
	logger := log.New(output, "", 0)
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), maxCommandBytes)
	awaitingAddress := false
	hadError := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		prompt := replPrompt
		var command parsedCommand
		var err error

		if awaitingAddress {
			prompt = addressPrompt
			awaitingAddress = false
			command = parsedCommand{name: "connect", args: []string{line}}
		} else {
			command, err = parseCommandLine(line)
		}

		fmt.Fprintf(output, "%s%s\n", prompt, line)
		_ = history.Add(line)

		if err != nil {
			fmt.Fprintf(output, "Error: %s\n", err)
			hadError = true
			continue
		}
		if command.name == "connect" && len(command.args) == 0 {
			awaitingAddress = true
			continue
		}

		action, err := dispatchCommand(logger, command, serverCallbacks{
			onMessage: func(response *message.Message) {
				logger.Printf("sv->%s\n", string(parseData(response.Data)))
			},
		}, true)
		if err != nil {
			fmt.Fprintf(output, "Error: %s\n", err)
			hadError = true
			continue
		}
		if action == commandExit {
			if hadError {
				return 1
			}
			return 0
		}
		if action == commandClear {
			continue
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(output, "Error: %s\n", err)
		return 1
	}
	if awaitingAddress {
		fmt.Fprintln(output, "Error: connect requires one address")
		hadError = true
	}
	if hadError {
		return 1
	}
	return 0
}

func dispatchCommand(logger Log, command parsedCommand, callbacks serverCallbacks, builtins bool) (commandAction, error) { //nolint:gocyclo
	if builtins {
		switch command.name {
		case "clear":
			return commandClear, nil
		case "exit":
			return commandExit, nil
		case "help":
			logger.Print(commandHelpText())
			return commandContinue, nil
		}
	}

	switch command.name {
	case "connect":
		if len(command.args) != 1 || command.args[0] == "" {
			return commandContinue, errors.New("connect requires one address")
		}
		return commandContinue, connect(logger, command.args[0], callbacks)
	case "sethandshake":
		return commandContinue, setHandshake(logger, command.args)
	case "request":
		return commandContinue, request(logger, command.args)
	case "notify":
		return commandContinue, notify(logger, command.args)
	case "push":
		return commandContinue, push(logger, command.args)
	case "routes":
		if builtins {
			return commandContinue, errors.New("incorrect input, try 'help'")
		}
		return commandContinue, routes(logger)
	case "disconnect":
		disconnect()
		return commandContinue, nil
	default:
		return commandContinue, errors.New("incorrect input, try 'help'")
	}
}

func parseCommandLine(line string) (parsedCommand, error) {
	name, remainder, ok := splitFirstField(line)
	if !ok {
		return parsedCommand{}, errors.New("command is empty")
	}

	command := parsedCommand{name: name}
	var err error
	switch name {
	case "request", "notify":
		route, payload, hasRoute := splitFirstField(remainder)
		if hasRoute {
			command.args = append(command.args, route)
		}
		if payload = strings.TrimSpace(payload); payload != "" {
			command.args = append(command.args, payload)
		}
	case "sethandshake":
		remainder = strings.TrimSpace(remainder)
		if strings.HasPrefix(remainder, "{") {
			command.args = []string{remainder}
		} else {
			command.args, err = shellwords.Parse(remainder)
		}
	default:
		command.args, err = shellwords.Parse(remainder)
	}

	return command, err
}

func splitFirstField(value string) (field string, remainder string, ok bool) {
	value = strings.TrimLeftFunc(value, unicode.IsSpace)
	if value == "" {
		return "", "", false
	}

	index := strings.IndexFunc(value, unicode.IsSpace)
	if index == -1 {
		return value, "", true
	}
	return value[:index], value[index:], true
}

func replCommandNames() []string {
	names := make([]string, len(replCommands))
	for i, command := range replCommands {
		names[i] = command.name
	}
	return names
}

func commandHelpText() string {
	var builder strings.Builder
	builder.WriteString("\nCommands:\n")
	writer := tabwriter.NewWriter(&builder, 0, 4, 2, ' ', 0)
	for _, command := range replCommands {
		fmt.Fprintf(writer, "\t%s\t\t\t%s\n", command.name, command.help)
	}
	_ = writer.Flush()
	builder.WriteByte('\n')
	return builder.String()
}

type messageLog struct {
	send func(string)
}

func (l messageLog) Print(values ...interface{}) {
	l.emit(fmt.Sprint(values...))
}

func (l messageLog) Println(values ...interface{}) {
	l.emit(strings.TrimSuffix(fmt.Sprintln(values...), "\n"))
}

func (l messageLog) Printf(format string, values ...interface{}) {
	l.emit(fmt.Sprintf(format, values...))
}

func (l messageLog) emit(message string) {
	if l.send == nil {
		return
	}
	l.send(strings.TrimSuffix(message, "\n"))
}

type commandHistory struct {
	path     string
	entries  []string
	position int
	draft    string
}

func loadCommandHistory(path string) *commandHistory {
	history := &commandHistory{path: path}
	if path == "" {
		return history
	}

	info, err := os.Lstat(path)
	if err != nil {
		return history
	}
	if err := validateHistoryFileInfo(path, info); err != nil {
		return history
	}

	file, err := os.Open(path)
	if err != nil {
		return history
	}
	defer file.Close()
	if err := verifyOpenHistoryFile(path, file); err != nil {
		return history
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxCommandBytes)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			history.entries = append(history.entries, line)
		}
	}
	if len(history.entries) > historyLimit {
		history.entries = history.entries[len(history.entries)-historyLimit:]
	}
	if scanner.Err() != nil {
		history.path = ""
	}
	history.position = len(history.entries)
	return history
}

func (h *commandHistory) Add(line string) error {
	if h == nil || line == "" {
		return nil
	}

	h.entries = append(h.entries, line)
	if len(h.entries) > historyLimit {
		h.entries = h.entries[len(h.entries)-historyLimit:]
	}
	h.ResetNavigation()

	if h.path == "" {
		return nil
	}
	file, err := openHistoryFileForAppend(h.path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintln(file, line)
	return err
}

func openHistoryFileForAppend(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if err := validateHistoryFileInfo(path, info); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := verifyOpenHistoryFile(path, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func verifyOpenHistoryFile(path string, file *os.File) error {
	openedInfo, err := file.Stat()
	if err != nil {
		return err
	}

	pathInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := validateHistoryFileInfo(path, pathInfo); err != nil {
		return err
	}
	if !os.SameFile(openedInfo, pathInfo) {
		return fmt.Errorf("history file %q changed while opening", path)
	}
	return nil
}

func validateHistoryFileInfo(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("history file %q must not be a symbolic link", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("history file %q is not a regular file", path)
	}
	return nil
}

func (h *commandHistory) Previous(current string) (string, bool) {
	if h == nil || len(h.entries) == 0 || h.position == 0 {
		return "", false
	}
	if h.position == len(h.entries) {
		h.draft = current
	}
	h.position--
	return h.entries[h.position], true
}

func (h *commandHistory) Next() (string, bool) {
	if h == nil || h.position >= len(h.entries) {
		return "", false
	}
	h.position++
	if h.position == len(h.entries) {
		return h.draft, true
	}
	return h.entries[h.position], true
}

func (h *commandHistory) ResetNavigation() {
	if h == nil {
		return
	}
	h.position = len(h.entries)
	h.draft = ""
}

func isTerminal(file *os.File) bool {
	return term.IsTerminal(file.Fd())
}
