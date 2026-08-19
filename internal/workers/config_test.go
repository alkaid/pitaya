package workers

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetConnectionPoolUsesRedisACLAuthentication(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	commands := make(chan []string, 2)
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for index := 0; index < 2; index++ {
			command, readErr := readRESPCommand(reader)
			if readErr != nil {
				serverErr <- readErr
				return
			}
			commands <- command
			if _, writeErr := io.WriteString(conn, "+OK\r\n"); writeErr != nil {
				serverErr <- writeErr
				return
			}
		}
		serverErr <- nil
	}()

	pool := GetConnectionPool(Options{
		Address:  listener.Addr().String(),
		Username: "pitaya-worker",
		Password: "transport-secret",
		PoolSize: 1,
	})
	t.Cleanup(func() { _ = pool.Close() })
	conn := pool.Get()
	_, err = conn.Do("PING")
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	assert.Equal(t, []string{"AUTH", "pitaya-worker", "transport-secret"}, receiveCommand(t, commands))
	assert.Equal(t, []string{"PING"}, receiveCommand(t, commands))
	select {
	case err := <-serverErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("fake Redis server did not finish")
	}
}

func receiveCommand(t *testing.T, commands <-chan []string) []string {
	t.Helper()
	select {
	case command := <-commands:
		return command
	case <-time.After(time.Second):
		t.Fatal("fake Redis server did not receive command")
		return nil
	}
}

func readRESPCommand(reader *bufio.Reader) ([]string, error) {
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(header, "*") {
		return nil, fmt.Errorf("expected RESP array, got %q", strings.TrimSpace(header))
	}
	count, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(header, "*")))
	if err != nil {
		return nil, err
	}
	command := make([]string, 0, count)
	for index := 0; index < count; index++ {
		lengthLine, readErr := reader.ReadString('\n')
		if readErr != nil {
			return nil, readErr
		}
		if !strings.HasPrefix(lengthLine, "$") {
			return nil, fmt.Errorf("expected RESP bulk string, got %q", strings.TrimSpace(lengthLine))
		}
		length, parseErr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(lengthLine, "$")))
		if parseErr != nil {
			return nil, parseErr
		}
		value := make([]byte, length+2)
		if _, readErr = io.ReadFull(reader, value); readErr != nil {
			return nil, readErr
		}
		if string(value[length:]) != "\r\n" {
			return nil, fmt.Errorf("bulk string missing CRLF")
		}
		command = append(command, string(value[:length]))
	}
	return command, nil
}
