package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/alkaid/pitaya/v2/helpers"
)

type benchmarkServers struct {
	frontend           *helpers.TestServerProcess
	backend            *helpers.TestServerProcess
	natsMessagesBuffer int
}

const (
	effectiveNATSBufferMarker = "pitaya test effective NATS messages buffer: "
	natsSlowConsumerMarker    = "nats slow consumer"
	natsDroppedMessageMarker  = "some messages were dropped"
)

// The helper retains a matched marker even after the bounded output tail rolls.
func benchmarkOutputMarkers(natsMessagesBuffer int) []string {
	markers := []string{
		natsSlowConsumerMarker,
		natsDroppedMessageMarker,
	}
	if natsMessagesBuffer > 0 {
		markers = append(markers, effectiveNATSBufferMarker+strconv.Itoa(natsMessagesBuffer))
	}
	return markers
}

func configuredNATSMessagesBuffer() (int, error) {
	value := os.Getenv("PITAYA_TEST_NATS_MESSAGES_BUFFER")
	if value == "" {
		return 0, nil
	}
	buffer, err := strconv.Atoi(value)
	if err != nil || buffer <= 0 {
		return 0, fmt.Errorf("invalid PITAYA_TEST_NATS_MESSAGES_BUFFER %q", value)
	}
	return buffer, nil
}

func main() {
	readyFile := flag.String("ready-file", "", "path used to publish the frontend address")
	diagnosticsDir := flag.String("diagnostics-dir", "", "directory for bounded server logs")
	sdPrefix := flag.String("sd-prefix", "", "service discovery prefix")
	flag.Parse()
	if *readyFile == "" || *diagnosticsDir == "" {
		fmt.Fprintln(os.Stderr, "ready-file and diagnostics-dir are required")
		os.Exit(2)
	}
	if *sdPrefix == "" {
		*sdPrefix = fmt.Sprintf("pitaya/benchmark/%d/", os.Getpid())
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if err := run(ctx, *readyFile, *diagnosticsDir, *sdPrefix); err != nil {
		fmt.Fprintf(os.Stderr, "benchmark server supervisor: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, readyFile, diagnosticsDir, sdPrefix string) (err error) {
	servers, startErr := startServers(ctx, sdPrefix)
	defer func() {
		stopErr := servers.stop()
		diagnosticsErr := servers.writeDiagnostics(diagnosticsDir)
		err = errors.Join(err, stopErr, diagnosticsErr)
	}()
	if startErr != nil {
		if errors.Is(startErr, context.Canceled) {
			return nil
		}
		return fmt.Errorf("start benchmark servers: %w", startErr)
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := writeAtomic(readyFile, "127.0.0.1:"+strconv.Itoa(servers.frontend.Port())); err != nil {
		return fmt.Errorf("publish benchmark readiness: %w", err)
	}
	return servers.wait(ctx)
}

func startServers(ctx context.Context, sdPrefix string) (*benchmarkServers, error) {
	natsMessagesBuffer, err := configuredNATSMessagesBuffer()
	servers := &benchmarkServers{natsMessagesBuffer: natsMessagesBuffer}
	if err != nil {
		return servers, err
	}
	outputMarkers := benchmarkOutputMarkers(natsMessagesBuffer)
	backend, err := helpers.StartTestServerContext(ctx, helpers.TestServerConfig{
		ServerType:         "game",
		SDPrefix:           sdPrefix,
		CleanPitayaEnv:     true,
		NATSMessagesBuffer: natsMessagesBuffer,
		OutputMarkers:      outputMarkers,
	})
	if err != nil {
		return servers, fmt.Errorf("backend: %w", err)
	}
	servers.backend = backend

	frontend, err := helpers.StartTestServerContext(ctx, helpers.TestServerConfig{
		Frontend:           true,
		ServerType:         "connector",
		SDPrefix:           sdPrefix,
		CleanPitayaEnv:     true,
		NATSMessagesBuffer: natsMessagesBuffer,
		OutputMarkers:      outputMarkers,
	})
	if err != nil {
		return servers, fmt.Errorf("frontend: %w\nbackend output:\n%s", err, backend.Tail())
	}
	servers.frontend = frontend
	return servers, nil
}

func (s *benchmarkServers) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case <-s.frontend.Done():
		return unexpectedServerExit("frontend", s.frontend)
	case <-s.backend.Done():
		return unexpectedServerExit("backend", s.backend)
	}
}

func unexpectedServerExit(name string, process *helpers.TestServerProcess) error {
	waitErr := process.Wait()
	if waitErr != nil {
		return fmt.Errorf("%s exited unexpectedly: %w\nlast output:\n%s", name, waitErr, process.Tail())
	}
	return fmt.Errorf("%s exited unexpectedly\nlast output:\n%s", name, process.Tail())
}

func (s *benchmarkServers) stop() error {
	if s == nil {
		return nil
	}
	var errs []error
	if err := s.frontend.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("frontend: %w", err))
	}
	if err := s.backend.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("backend: %w", err))
	}
	return errors.Join(errs...)
}

func (s *benchmarkServers) writeDiagnostics(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var errs []error
	for name, output := range map[string]string{
		"frontend.log": s.frontend.Tail(),
		"backend.log":  s.backend.Tail(),
	} {
		contents := []byte(output)
		if len(contents) > 0 && contents[len(contents)-1] != '\n' {
			contents = append(contents, '\n')
		}
		if err := os.WriteFile(filepath.Join(dir, name), contents, 0o600); err != nil {
			errs = append(errs, fmt.Errorf("write %s: %w", name, err))
		}
		if err := validateStressServerOutput(name, output, s.natsMessagesBuffer); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func validateStressServerOutput(name, output string, natsMessagesBuffer int) error {
	if natsMessagesBuffer == 0 {
		return nil
	}
	effectiveMarker := effectiveNATSBufferMarker + strconv.Itoa(natsMessagesBuffer)
	if !strings.Contains(output, effectiveMarker) {
		return fmt.Errorf("%s did not confirm %s", name, effectiveMarker)
	}
	for _, marker := range []string{natsSlowConsumerMarker, natsDroppedMessageMarker} {
		if strings.Contains(output, marker) {
			return fmt.Errorf("%s reported dropped NATS messages: %s", name, marker)
		}
	}
	return nil
}

func writeAtomic(path, value string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".benchmark-ready-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := fmt.Fprintln(tmp, value); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
