package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/alkaid/pitaya/v2/config"
	"github.com/alkaid/pitaya/v2/internal/statefulowner"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const reportSchemaVersion = 1

type commandConfig struct {
	endpoints   string
	user        string
	password    string
	prefix      string
	dialTimeout time.Duration
	timeout     time.Duration
	tlsEnabled  bool
	caFile      string
	certFile    string
	keyFile     string
	serverName  string
	format      string
	limit       int
	pageSize    int
	apply       bool
	input       string
}

type report struct {
	SchemaVersion int                           `json:"schema_version"`
	Operation     string                        `json:"operation"`
	DryRun        bool                          `json:"dry_run"`
	Error         string                        `json:"error,omitempty"`
	Assignments   []statefulowner.Snapshot      `json:"assignments"`
	Counts        map[statefulowner.Status]int  `json:"counts"`
	Cleanup       []statefulowner.CleanupResult `json:"cleanup,omitempty"`
	Metrics       ownerMetrics                  `json:"metrics"`
}

type labeledCount struct {
	ServerType string `json:"server_type,omitempty"`
	Result     string `json:"result,omitempty"`
	Status     string `json:"status,omitempty"`
	Count      int64  `json:"count"`
}

type validationMetric struct {
	ServerType       string `json:"server_type"`
	Count            int64  `json:"count"`
	TotalNanoseconds int64  `json:"total_nanoseconds"`
	MaxNanoseconds   int64  `json:"max_nanoseconds"`
}

type ownerMetrics struct {
	InventoryCounts   []labeledCount     `json:"assignment_inventory"`
	ValidationLatency []validationMetric `json:"validation_latency"`
	CleanupResults    []labeledCount     `json:"cleanup_results"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		printUsage(stdout)
		return 0
	}
	operation := args[0]
	if operation != "inventory" && operation != "prune-stale" && operation != "prune-list" {
		fmt.Fprintf(stderr, "unknown operation %q\n", operation)
		printUsage(stderr)
		return 2
	}

	cfg, err := parseFlags(operation, args[1:], stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := validateConfig(operation, cfg); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()
	cli, err := newClient(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "connect to etcd: %v\n", err)
		return 1
	}
	defer cli.Close()

	var inventory statefulowner.Inventory
	switch operation {
	case "inventory", "prune-stale":
		inventory, err = statefulowner.InventoryAll(ctx, cli, cfg.prefix, cfg.limit, cfg.pageSize)
	case "prune-list":
		var keys []string
		keys, err = readKeyList(cfg.input, cfg.limit)
		if err == nil {
			inventory, err = statefulowner.InventoryKeys(ctx, cli, cfg.prefix, keys, cfg.limit)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", operation, err)
		return 1
	}
	statefulowner.SortSnapshots(inventory.Assignments)

	result := report{
		SchemaVersion: reportSchemaVersion,
		Operation:     operation,
		DryRun:        !cfg.apply,
		Assignments:   inventory.Assignments,
		Counts:        inventory.Counts,
	}
	if operation != "inventory" {
		selected := inventory.Assignments
		if operation == "prune-stale" {
			selected = make([]statefulowner.Snapshot, 0, inventory.Counts[statefulowner.StatusStale])
			for _, snapshot := range inventory.Assignments {
				if cfg.apply && snapshot.Status == statefulowner.StatusCorrupt {
					fmt.Fprintf(stderr, "%s: corrupt assignment %q blocks apply\n", operation, snapshot.AssignmentKey)
					return 1
				}
				if snapshot.Status == statefulowner.StatusStale {
					selected = append(selected, snapshot)
				}
			}
		}
		result.Cleanup, err = statefulowner.Prune(ctx, cli, cfg.prefix, selected, cfg.apply)
	}
	return finishReport(stdout, stderr, cfg.format, operation, result, err)
}

func finishReport(stdout, stderr io.Writer, format, operation string, result report, operationErr error) int {
	result.Metrics = buildOwnerMetrics(result.Assignments, result.Cleanup)
	if operationErr != nil {
		result.Error = operationErr.Error()
	}
	if err := writeReport(stdout, format, result); err != nil {
		fmt.Fprintf(stderr, "write report: %v\n", err)
		return 1
	}
	if operationErr != nil {
		fmt.Fprintf(stderr, "%s: %v\n", operation, operationErr)
		return 1
	}
	return 0
}

func buildOwnerMetrics(assignments []statefulowner.Snapshot, cleanup []statefulowner.CleanupResult) ownerMetrics {
	type inventoryKey struct {
		serverType string
		status     string
	}
	inventoryCounts := make(map[inventoryKey]int64)
	validation := make(map[string]validationMetric)
	serverTypeByKey := make(map[string]string, len(assignments))
	for _, assignment := range assignments {
		serverType := assignment.ServerType
		if serverType == "" {
			serverType = "unknown"
		}
		serverTypeByKey[assignment.AssignmentKey] = serverType
		inventoryCounts[inventoryKey{serverType: serverType, status: string(assignment.Status)}]++
		metric := validation[serverType]
		metric.ServerType = serverType
		metric.Count++
		metric.TotalNanoseconds += assignment.ValidationDuration.Nanoseconds()
		metric.MaxNanoseconds = max(metric.MaxNanoseconds, assignment.ValidationDuration.Nanoseconds())
		validation[serverType] = metric
	}

	metrics := ownerMetrics{}
	for key, count := range inventoryCounts {
		metrics.InventoryCounts = append(metrics.InventoryCounts, labeledCount{
			ServerType: key.serverType,
			Status:     key.status,
			Count:      count,
		})
	}
	for _, metric := range validation {
		metrics.ValidationLatency = append(metrics.ValidationLatency, metric)
	}
	cleanupCounts := make(map[string]int64)
	for _, item := range cleanup {
		serverType := serverTypeByKey[item.AssignmentKey]
		cleanupCounts[serverType+"\x00"+string(item.Status)]++
	}
	for key, count := range cleanupCounts {
		parts := strings.SplitN(key, "\x00", 2)
		metrics.CleanupResults = append(metrics.CleanupResults, labeledCount{
			ServerType: parts[0],
			Result:     parts[1],
			Count:      count,
		})
	}
	sort.Slice(metrics.InventoryCounts, func(i, j int) bool {
		left, right := metrics.InventoryCounts[i], metrics.InventoryCounts[j]
		return left.ServerType+"\x00"+left.Status < right.ServerType+"\x00"+right.Status
	})
	sort.Slice(metrics.ValidationLatency, func(i, j int) bool {
		return metrics.ValidationLatency[i].ServerType < metrics.ValidationLatency[j].ServerType
	})
	sort.Slice(metrics.CleanupResults, func(i, j int) bool {
		left, right := metrics.CleanupResults[i], metrics.CleanupResults[j]
		return left.ServerType+"\x00"+left.Result < right.ServerType+"\x00"+right.Result
	})
	return metrics
}

func parseFlags(operation string, args []string, stderr io.Writer) (commandConfig, error) {
	dialTimeout, err := durationEnv("PITAYA_OWNERCTL_DIAL_TIMEOUT", 5*time.Second)
	if err != nil {
		return commandConfig{}, err
	}
	timeout, err := durationEnv("PITAYA_OWNERCTL_TIMEOUT", 30*time.Second)
	if err != nil {
		return commandConfig{}, err
	}
	tlsEnabled, err := boolEnv("PITAYA_OWNERCTL_TLS")
	if err != nil {
		return commandConfig{}, err
	}
	fs := flag.NewFlagSet("pitaya-ownerctl "+operation, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := commandConfig{}
	fs.StringVar(&cfg.endpoints, "endpoints", envOr("PITAYA_OWNERCTL_ENDPOINTS", "localhost:2379"), "comma-separated etcd endpoints")
	fs.StringVar(&cfg.user, "user", os.Getenv("PITAYA_OWNERCTL_USER"), "etcd username")
	fs.StringVar(&cfg.password, "password", os.Getenv("PITAYA_OWNERCTL_PASSWORD"), "etcd password")
	fs.StringVar(&cfg.prefix, "prefix", envOr("PITAYA_OWNERCTL_PREFIX", "pitaya/"), "etcd namespace prefix")
	fs.DurationVar(&cfg.dialTimeout, "dial-timeout", dialTimeout, "etcd dial timeout")
	fs.DurationVar(&cfg.timeout, "timeout", timeout, "operation timeout")
	fs.BoolVar(&cfg.tlsEnabled, "tls", tlsEnabled, "enable TLS")
	fs.StringVar(&cfg.caFile, "ca-file", os.Getenv("PITAYA_OWNERCTL_CA_FILE"), "trusted CA bundle")
	fs.StringVar(&cfg.certFile, "cert-file", os.Getenv("PITAYA_OWNERCTL_CERT_FILE"), "client certificate")
	fs.StringVar(&cfg.keyFile, "key-file", os.Getenv("PITAYA_OWNERCTL_KEY_FILE"), "client private key")
	fs.StringVar(&cfg.serverName, "server-name", os.Getenv("PITAYA_OWNERCTL_SERVER_NAME"), "TLS server name")
	fs.StringVar(&cfg.format, "format", "table", "output format: table or json")
	fs.IntVar(&cfg.limit, "limit", 10000, "maximum assignments inspected")
	fs.IntVar(&cfg.pageSize, "page-size", statefulowner.DefaultPageSize, "etcd page size")
	if operation != "inventory" {
		fs.BoolVar(&cfg.apply, "apply", false, "apply stale assignment deletions")
	}
	if operation == "prune-list" {
		fs.StringVar(&cfg.input, "input", "", "reviewed assignment-key list")
	}
	if err := fs.Parse(args); err != nil {
		return commandConfig{}, err
	}
	if fs.NArg() != 0 {
		return commandConfig{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return cfg, nil
}

func validateConfig(operation string, cfg commandConfig) error {
	if cfg.timeout <= 0 || cfg.dialTimeout <= 0 {
		return fmt.Errorf("timeout and dial-timeout must be positive")
	}
	if cfg.limit <= 0 || cfg.pageSize <= 0 {
		return fmt.Errorf("limit and page-size must be positive")
	}
	if cfg.format != "table" && cfg.format != "json" {
		return fmt.Errorf("format must be table or json")
	}
	if _, err := statefulowner.NamespacePrefix(cfg.prefix); err != nil {
		return err
	}
	if operation == "prune-list" && cfg.input == "" {
		return fmt.Errorf("prune-list requires --input")
	}
	endpoints := strings.Split(cfg.endpoints, ",")
	for _, endpoint := range endpoints {
		if strings.TrimSpace(endpoint) == "" {
			return fmt.Errorf("endpoints must not contain empty values")
		}
	}
	return nil
}

func newClient(cfg commandConfig) (*clientv3.Client, error) {
	tlsConfig, err := config.LoadClientTLS("pitaya-ownerctl.tls", config.TLSConfig{
		Enabled:    cfg.tlsEnabled,
		CAFile:     cfg.caFile,
		CertFile:   cfg.certFile,
		KeyFile:    cfg.keyFile,
		ServerName: cfg.serverName,
	})
	if err != nil {
		return nil, err
	}
	parts := strings.Split(cfg.endpoints, ",")
	endpoints := make([]string, 0, len(parts))
	for _, endpoint := range parts {
		endpoints = append(endpoints, strings.TrimSpace(endpoint))
	}
	return clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		Username:    cfg.user,
		Password:    cfg.password,
		DialTimeout: cfg.dialTimeout,
		TLS:         tlsConfig,
	})
}

func readKeyList(path string, limit int) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open input: %w", err)
	}
	defer file.Close()

	keys := make([]string, 0)
	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		line++
		value := strings.TrimSpace(scanner.Text())
		if value == "" || strings.HasPrefix(value, "#") {
			continue
		}
		if strings.ContainsAny(value, " \t") {
			return nil, fmt.Errorf("input line %d must contain one full assignment key", line)
		}
		keys = append(keys, value)
		if len(keys) > limit {
			return nil, fmt.Errorf("%w: input contains more than %d keys", statefulowner.ErrLimitExceeded, limit)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}
	return keys, nil
}

func writeReport(output io.Writer, format string, result report) error {
	if format == "json" {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}

	w := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "STATUS\tASSIGNMENT KEY\tSERVER TYPE\tOWNER\tEPOCH\tREGISTRATION REVISION\tDETAIL"); err != nil {
		return err
	}
	cleanup := make(map[string]statefulowner.CleanupStatus, len(result.Cleanup))
	for _, item := range result.Cleanup {
		cleanup[item.AssignmentKey] = item.Status
	}
	for _, item := range result.Assignments {
		detail := item.Diagnostic
		if detail == "" && item.StaleReason != "" {
			detail = string(item.StaleReason)
		}
		if cleanupStatus, ok := cleanup[item.AssignmentKey]; ok {
			if detail != "" {
				detail += ", "
			}
			detail += string(cleanupStatus)
		}
		if _, err := fmt.Fprintf(
			w,
			"%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			item.Status,
			item.AssignmentKey,
			item.ServerType,
			item.OwnerID,
			item.OwnerEpoch,
			item.RegistrationRevision,
			detail,
		); err != nil {
			return err
		}
	}
	return w.Flush()
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: pitaya-ownerctl <inventory|prune-stale|prune-list> [flags]")
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", name, err)
	}
	return parsed, nil
}

func boolEnv(name string) (bool, error) {
	value := os.Getenv(name)
	if value == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return parsed, nil
}
