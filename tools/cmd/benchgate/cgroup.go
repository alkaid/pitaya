package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	// Quota alone is insufficient: burst, weight, utilization clamps, and
	// cpuset partitioning can change service without changing cpu.max/cfs_quota.
	cgroupV2ConstraintFiles = []string{
		"cpu.idle",
		"cpu.max",
		"cpu.max.burst",
		"cpu.uclamp.max",
		"cpu.uclamp.min",
		"cpu.weight",
		"cpuset.cpus",
		"cpuset.cpus.effective",
		"cpuset.cpus.exclusive",
		"cpuset.cpus.exclusive.effective",
		"cpuset.cpus.partition",
		"cpuset.mems",
		"cpuset.mems.effective",
	}
	cgroupV1CPUConstraintFiles = []string{
		// v1 idle/uclamp alter scheduling and DVFS just like quota/weight, so
		// they must remain part of the stable performance regime fingerprint.
		"cpu.cfs_burst_us",
		"cpu.cfs_period_us",
		"cpu.cfs_quota_us",
		"cpu.idle",
		"cpu.shares",
		"cpu.uclamp.max",
		"cpu.uclamp.min",
	}
	cgroupCPUSetConstraintFiles = []string{
		"cpuset.cpu_exclusive",
		"cpuset.cpus",
		"cpuset.cpus.effective",
		"cpuset.effective_cpus",
		"cpuset.effective_mems",
		"cpuset.mems",
		"cpuset.mems.effective",
		"cpuset.sched_load_balance",
		"cpuset.sched_relax_domain_level",
	}
)

type cgroupMembership struct {
	hierarchyID string
	unified     bool
	controllers []string
	path        string
}

type cgroupMount struct {
	mountID     uint64
	parentID    uint64
	deviceMajor uint32
	deviceMinor uint32
	filesystem  string
	root        string
	mountPoint  string
	controllers []string
}

type resolvedCgroupMembership struct {
	path string
}

type cgroupControlCounts struct {
	cpu    int
	cpuset int
}

type mountInfoEntry struct {
	mountID     uint64
	parentID    uint64
	deviceMajor uint32
	deviceMinor uint32
	filesystem  string
	root        string
	mountPoint  string
}

type cgroupMountTopology struct {
	entries []mountInfoEntry
	mounts  []cgroupMount
}

type cgroupPathValidator func(cgroupMount, string) error

func cpuConstraintsFingerprintFiles(statusFilename, membershipFilename, mountInfoFilename string) (string, error) {
	if err := validateProductionCgroupNamespace(); err != nil {
		return "", err
	}
	return cpuConstraintsFingerprintFilesWithValidator(statusFilename, membershipFilename, mountInfoFilename, validateProductionCgroupPath, true)
}

func cpuConstraintsFingerprintFilesWithValidator(statusFilename, membershipFilename, mountInfoFilename string, validator cgroupPathValidator, fullAncestryProven bool) (string, error) {
	records, err := collectCPUConstraintRecordsWithValidator(statusFilename, membershipFilename, mountInfoFilename, validator, fullAncestryProven)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(strings.Join(records, "\n")))
	return fmt.Sprintf("%x", digest[:]), nil
}

func collectCPUConstraintRecords(statusFilename, membershipFilename, mountInfoFilename string) ([]string, error) {
	if err := validateProductionCgroupNamespace(); err != nil {
		return nil, err
	}
	return collectCPUConstraintRecordsWithValidator(statusFilename, membershipFilename, mountInfoFilename, validateProductionCgroupPath, true)
}

func collectCPUConstraintRecordsWithValidator(statusFilename, membershipFilename, mountInfoFilename string, validator cgroupPathValidator, fullAncestryProven bool) ([]string, error) {
	// Kernel-authored status, cgroup, and mountinfo are the trust anchor. A
	// discovered path is read only after topology and mount-identity checks.
	if validator == nil {
		return nil, errors.New("cgroup path validator is required")
	}
	if !fullAncestryProven {
		return nil, errors.New("complete cgroup ancestry has not been proven")
	}
	status, err := readRegularFile(statusFilename, maximumMetadataBytes)
	if err != nil {
		return nil, fmt.Errorf("read CPU affinity: %w", err)
	}
	records, err := parseCPUStatusRecords(status)
	if err != nil {
		return nil, fmt.Errorf("parse CPU affinity: %w", err)
	}

	membershipData, err := readRegularFile(membershipFilename, maximumMetadataBytes)
	if err != nil {
		return nil, fmt.Errorf("read cgroup membership: %w", err)
	}
	memberships, err := parseCgroupMemberships(membershipData)
	if err != nil {
		return nil, fmt.Errorf("parse cgroup membership: %w", err)
	}

	mountInfoData, err := readRegularFile(mountInfoFilename, maximumMetadataBytes)
	if err != nil {
		return nil, fmt.Errorf("read cgroup mounts: %w", err)
	}
	topology, err := parseCgroupMounts(mountInfoData, hasUnifiedMembership(memberships))
	if err != nil {
		return nil, fmt.Errorf("parse cgroup mounts: %w", err)
	}

	totalControls := 0
	for _, membership := range memberships {
		matchedControllers := make(map[string]bool)
		matchedMounts := 0
		for _, mount := range topology.mounts {
			if !membershipMatchesMount(membership, mount) {
				continue
			}
			if topologyErr := topology.validatePath(mount, mount.mountPoint); topologyErr != nil {
				return nil, topologyErr
			}
			if validateErr := validator(mount, mount.mountPoint); validateErr != nil {
				return nil, fmt.Errorf("validate cgroup mount point %q: %w", mount.mountPoint, validateErr)
			}
			resolved, resolveErr := resolveCgroupMembership(mount, membership.path)
			if resolveErr != nil {
				return nil, fmt.Errorf("resolve %s cgroup membership %q at %q: %w", mount.filesystem, membership.path, mount.mountPoint, resolveErr)
			}
			matchedMounts++
			for _, controller := range membership.controllers {
				if containsString(mount.controllers, controller) {
					matchedControllers[controller] = true
				}
			}
			mountRecords, controlCounts, collectErr := collectCgroupMountRecords(topology, mount, membership, resolved, validator)
			if collectErr != nil {
				return nil, collectErr
			}
			if controlErr := validateCgroupControlSurface(mount, controlCounts); controlErr != nil {
				return nil, controlErr
			}
			totalControls += controlCounts.cpu + controlCounts.cpuset
			records = append(records, mountRecords...)
		}
		if matchedMounts == 0 {
			return nil, fmt.Errorf("no cgroup mount exposes hierarchy %s membership %q", membership.hierarchyID, membership.path)
		}
		for _, controller := range membership.controllers {
			if !matchedControllers[controller] {
				return nil, fmt.Errorf("no cgroup mount exposes %s controller membership %q", controller, membership.path)
			}
		}
	}
	if totalControls == 0 {
		// An empty hierarchy is indistinguishable from a wrong or synthetic
		// mount, so the formal gate does not accept "apparently unconstrained".
		return nil, errors.New("no CPU control files found in cgroup hierarchy")
	}

	sort.Strings(records)
	for index := 1; index < len(records); index++ {
		if records[index] == records[index-1] {
			return nil, fmt.Errorf("duplicate CPU constraint record %q", records[index])
		}
	}
	return records, nil
}

func parseCPUStatusRecords(contents []byte) ([]string, error) {
	if !utf8.Valid(contents) || strings.ContainsRune(string(contents), '\x00') {
		return nil, errors.New("status is not valid text")
	}
	wanted := map[string]bool{
		"Cpus_allowed":      true,
		"Cpus_allowed_list": true,
		"Mems_allowed_list": true,
	}
	seen := make(map[string]bool)
	var records []string
	for _, line := range strings.Split(string(contents), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || !wanted[key] {
			continue
		}
		if seen[key] {
			return nil, fmt.Errorf("duplicate %s field", key)
		}
		seen[key] = true
		value = strings.Join(strings.Fields(value), " ")
		if value == "" {
			return nil, fmt.Errorf("%s is empty", key)
		}
		records = append(records, fmt.Sprintf("status field=%q value=%q", key, value))
	}
	if len(records) == 0 {
		return nil, errors.New("no CPU affinity fields")
	}
	return records, nil
}

func parseCgroupMemberships(contents []byte) ([]cgroupMembership, error) {
	lines, err := nonEmptyProcLines(contents)
	if err != nil {
		return nil, err
	}
	controllerOwner := make(map[string]string)
	seenUnified := false
	var memberships []cgroupMembership
	for lineNumber, line := range lines {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("line %d has %d fields, want 3", lineNumber+1, len(fields))
		}
		hierarchyID, parseErr := strconv.ParseUint(fields[0], 10, 64)
		if parseErr != nil || strconv.FormatUint(hierarchyID, 10) != fields[0] {
			return nil, fmt.Errorf("line %d has invalid hierarchy ID %q", lineNumber+1, fields[0])
		}
		membershipPath, pathErr := canonicalCgroupPath(fields[2])
		if pathErr != nil {
			return nil, fmt.Errorf("line %d path: %w", lineNumber+1, pathErr)
		}
		if fields[1] == "" {
			if hierarchyID != 0 {
				return nil, fmt.Errorf("line %d has empty controllers for non-unified hierarchy", lineNumber+1)
			}
			if seenUnified {
				return nil, errors.New("duplicate unified cgroup membership")
			}
			seenUnified = true
			memberships = append(memberships, cgroupMembership{
				hierarchyID: "0",
				unified:     true,
				path:        membershipPath,
			})
			continue
		}
		if hierarchyID == 0 {
			return nil, fmt.Errorf("line %d has controllers for unified hierarchy", lineNumber+1)
		}
		controllers, controllerErr := parseControllerList(fields[1])
		if controllerErr != nil {
			return nil, fmt.Errorf("line %d controllers: %w", lineNumber+1, controllerErr)
		}
		controllers = relevantCgroupControllers(controllers)
		if len(controllers) == 0 {
			continue
		}
		for _, controller := range controllers {
			if owner, exists := controllerOwner[controller]; exists {
				return nil, fmt.Errorf("controller %s belongs to both hierarchy %s and %d", controller, owner, hierarchyID)
			}
			controllerOwner[controller] = fields[0]
		}
		memberships = append(memberships, cgroupMembership{
			hierarchyID: fields[0],
			controllers: controllers,
			path:        membershipPath,
		})
	}
	legacyCPU, legacyCPUSet := false, false
	for _, membership := range memberships {
		legacyCPU = legacyCPU || containsString(membership.controllers, "cpu")
		legacyCPUSet = legacyCPUSet || containsString(membership.controllers, "cpuset")
	}
	if legacyCPU && legacyCPUSet {
		filtered := memberships[:0]
		for _, membership := range memberships {
			if !membership.unified {
				filtered = append(filtered, membership)
			}
		}
		memberships = filtered
	}
	if len(memberships) == 0 {
		return nil, errors.New("no CPU-related cgroup membership")
	}
	sort.Slice(memberships, func(left, right int) bool {
		leftKey := memberships[left].hierarchyID + "\x00" + strings.Join(memberships[left].controllers, ",") + "\x00" + memberships[left].path
		rightKey := memberships[right].hierarchyID + "\x00" + strings.Join(memberships[right].controllers, ",") + "\x00" + memberships[right].path
		return leftKey < rightKey
	})
	return memberships, nil
}

func parseCgroupMounts(contents []byte, unifiedRelevant bool) (cgroupMountTopology, error) {
	lines, err := nonEmptyProcLines(contents)
	if err != nil {
		return cgroupMountTopology{}, err
	}
	var topology cgroupMountTopology
	seenMountIDs := make(map[uint64]bool)
	for lineNumber, line := range lines {
		fields := strings.Fields(line)
		separator := -1
		for index, field := range fields {
			if field == "-" {
				separator = index
				break
			}
		}
		if separator < 6 || len(fields) != separator+4 {
			return cgroupMountTopology{}, fmt.Errorf("line %d has invalid mountinfo framing", lineNumber+1)
		}
		mountID, parseErr := strconv.ParseUint(fields[0], 10, 64)
		if parseErr != nil || strconv.FormatUint(mountID, 10) != fields[0] {
			return cgroupMountTopology{}, fmt.Errorf("line %d has invalid mount ID %q", lineNumber+1, fields[0])
		}
		parentID, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil || strconv.FormatUint(parentID, 10) != fields[1] || parentID == mountID {
			return cgroupMountTopology{}, fmt.Errorf("line %d has invalid parent mount ID %q", lineNumber+1, fields[1])
		}
		if seenMountIDs[mountID] {
			return cgroupMountTopology{}, fmt.Errorf("line %d duplicates mount ID %d", lineNumber+1, mountID)
		}
		seenMountIDs[mountID] = true
		deviceMajor, deviceMinor, deviceErr := parseMountInfoDevice(fields[2])
		if deviceErr != nil {
			return cgroupMountTopology{}, fmt.Errorf("line %d device: %w", lineNumber+1, deviceErr)
		}
		rootField, decodeErr := unescapeMountInfoPath(fields[3])
		if decodeErr != nil {
			return cgroupMountTopology{}, fmt.Errorf("line %d root: %w", lineNumber+1, decodeErr)
		}
		if rootField == "" || strings.ContainsRune(rootField, '\x00') {
			return cgroupMountTopology{}, fmt.Errorf("line %d root is empty or invalid", lineNumber+1)
		}
		mountPoint, decodeErr := unescapeMountInfoPath(fields[4])
		if decodeErr != nil {
			return cgroupMountTopology{}, fmt.Errorf("line %d mount point: %w", lineNumber+1, decodeErr)
		}
		if !filepath.IsAbs(mountPoint) || filepath.Clean(mountPoint) != mountPoint || strings.ContainsRune(mountPoint, '\x00') {
			return cgroupMountTopology{}, fmt.Errorf("line %d mount point is not a canonical absolute path", lineNumber+1)
		}
		filesystem := fields[separator+1]
		topology.entries = append(topology.entries, mountInfoEntry{
			mountID:     mountID,
			parentID:    parentID,
			deviceMajor: deviceMajor,
			deviceMinor: deviceMinor,
			filesystem:  filesystem,
			root:        rootField,
			mountPoint:  mountPoint,
		})
		if filesystem != "cgroup" && filesystem != "cgroup2" {
			continue
		}

		var controllers []string
		if filesystem == "cgroup" {
			controllerSet := make(map[string]bool)
			// Mount source is caller-controlled text; only cgroup super options
			// identify the v1 hierarchy's bound controllers.
			superOptions := strings.Split(fields[separator+3], ",")
			for _, controller := range superOptions {
				if isRelevantCgroupController(controller) {
					controllerSet[controller] = true
				}
			}
			for controller := range controllerSet {
				controllers = append(controllers, controller)
			}
			sort.Strings(controllers)
			if len(controllers) == 0 {
				continue
			}
			// noprefix renames every v1 control file; accepting it while looking
			// up canonical names would silently omit quotas and cpuset limits.
			if containsStringValue(superOptions, "noprefix") {
				return cgroupMountTopology{}, fmt.Errorf("line %d uses unsupported cgroup v1 noprefix controls", lineNumber+1)
			}
		} else if !unifiedRelevant {
			continue
		}
		// A non-root cgroup mount or a remounted namespace root can hide host
		// ancestors whose quotas still apply; mount identity cannot recover them.
		if rootField != "/" {
			return cgroupMountTopology{}, fmt.Errorf("line %d %s mount root %q is not the full cgroup hierarchy", lineNumber+1, filesystem, rootField)
		}
		topology.mounts = append(topology.mounts, cgroupMount{
			mountID:     mountID,
			parentID:    parentID,
			deviceMajor: deviceMajor,
			deviceMinor: deviceMinor,
			filesystem:  filesystem,
			root:        rootField,
			mountPoint:  mountPoint,
			controllers: controllers,
		})
	}
	if len(topology.mounts) == 0 {
		return cgroupMountTopology{}, errors.New("no cgroup mount exposes CPU-related controllers")
	}
	sort.Slice(topology.entries, func(left, right int) bool {
		return topology.entries[left].mountID < topology.entries[right].mountID
	})
	sort.Slice(topology.mounts, func(left, right int) bool {
		leftKey := topology.mounts[left].filesystem + "\x00" + topology.mounts[left].mountPoint + "\x00" + strings.Join(topology.mounts[left].controllers, ",")
		rightKey := topology.mounts[right].filesystem + "\x00" + topology.mounts[right].mountPoint + "\x00" + strings.Join(topology.mounts[right].controllers, ",")
		return leftKey < rightKey
	})
	return topology, nil
}

func (topology cgroupMountTopology) validatePath(mount cgroupMount, filename string) error {
	if !pathWithin(mount.mountPoint, filename) {
		return fmt.Errorf("path %q escapes cgroup mount %q", filename, mount.mountPoint)
	}
	for _, entry := range topology.entries {
		if entry.mountID == mount.mountID {
			continue
		}
		if entry.mountPoint == mount.mountPoint {
			return fmt.Errorf("cgroup mount %q is overmounted by mount %d (%s)", mount.mountPoint, entry.mountID, entry.filesystem)
		}
		if pathWithin(mount.mountPoint, entry.mountPoint) && pathWithin(entry.mountPoint, filename) {
			return fmt.Errorf("path %q crosses nested mount %d (%s) at %q", filename, entry.mountID, entry.filesystem, entry.mountPoint)
		}
	}
	return nil
}

func parseMountInfoDevice(value string) (uint32, uint32, error) {
	fields := strings.Split(value, ":")
	if len(fields) != 2 || fields[0] == "" || fields[1] == "" {
		return 0, 0, fmt.Errorf("invalid major:minor device %q", value)
	}
	major, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil || strconv.FormatUint(major, 10) != fields[0] {
		return 0, 0, fmt.Errorf("invalid device major %q", fields[0])
	}
	minor, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil || strconv.FormatUint(minor, 10) != fields[1] {
		return 0, 0, fmt.Errorf("invalid device minor %q", fields[1])
	}
	return uint32(major), uint32(minor), nil
}

func hasUnifiedMembership(memberships []cgroupMembership) bool {
	for _, membership := range memberships {
		if membership.unified {
			return true
		}
	}
	return false
}

func membershipMatchesMount(membership cgroupMembership, mount cgroupMount) bool {
	if membership.unified {
		return mount.filesystem == "cgroup2"
	}
	if mount.filesystem != "cgroup" {
		return false
	}
	for _, controller := range membership.controllers {
		if containsString(mount.controllers, controller) {
			return true
		}
	}
	return false
}

func resolveCgroupMembership(mount cgroupMount, membershipPath string) (resolvedCgroupMembership, error) {
	if mount.root != "/" {
		return resolvedCgroupMembership{}, errors.New("cgroup hierarchy does not expose provably complete ancestry")
	}
	relative := strings.TrimPrefix(membershipPath, "/")
	target := filepath.Join(mount.mountPoint, filepath.FromSlash(relative))
	if !pathWithin(mount.mountPoint, target) {
		return resolvedCgroupMembership{}, errors.New("membership path escapes the cgroup mount")
	}

	realMount, err := filepath.EvalSymlinks(mount.mountPoint)
	if err != nil {
		return resolvedCgroupMembership{}, err
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return resolvedCgroupMembership{}, err
	}
	if !pathWithin(realMount, realTarget) {
		return resolvedCgroupMembership{}, errors.New("resolved membership path escapes the cgroup mount")
	}
	logicalRelative, err := filepath.Rel(mount.mountPoint, target)
	if err != nil {
		return resolvedCgroupMembership{}, err
	}
	resolvedRelative, err := filepath.Rel(realMount, realTarget)
	if err != nil {
		return resolvedCgroupMembership{}, err
	}
	if logicalRelative != resolvedRelative {
		return resolvedCgroupMembership{}, errors.New("membership path traverses a symbolic link")
	}
	info, err := os.Stat(target)
	if err != nil {
		return resolvedCgroupMembership{}, err
	}
	if !info.IsDir() {
		return resolvedCgroupMembership{}, errors.New("membership path is not a directory")
	}
	return resolvedCgroupMembership{path: target}, nil
}

func collectCgroupMountRecords(topology cgroupMountTopology, mount cgroupMount, membership cgroupMembership, resolved resolvedCgroupMembership, validator cgroupPathValidator) ([]string, cgroupControlCounts, error) {
	controllers := strings.Join(mount.controllers, ",")
	recordPrefix := fmt.Sprintf("cgroup filesystem=%q mount_id=%d parent_id=%d device=%q mount=%q root=%q membership=%q controllers=%q",
		mount.filesystem, mount.mountID, mount.parentID, fmt.Sprintf("%d:%d", mount.deviceMajor, mount.deviceMinor), mount.mountPoint, mount.root, membership.path, controllers)
	records := []string{recordPrefix}
	files := constraintFilesForMount(mount)
	var controlCounts cgroupControlCounts
	for current := resolved.path; ; current = filepath.Dir(current) {
		relative, err := filepath.Rel(mount.mountPoint, current)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, cgroupControlCounts{}, fmt.Errorf("walk cgroup hierarchy at %q: path escapes mount", current)
		}
		// Lexical containment cannot see an overmount. Bind every consumed
		// directory and file to the parsed cgroup mount before trusting values.
		if topologyErr := topology.validatePath(mount, current); topologyErr != nil {
			return nil, cgroupControlCounts{}, topologyErr
		}
		if validateErr := validator(mount, current); validateErr != nil {
			return nil, cgroupControlCounts{}, fmt.Errorf("validate cgroup level %q: %w", current, validateErr)
		}
		level := "/"
		if relative != "." {
			level += filepath.ToSlash(relative)
		}
		entries, err := os.ReadDir(current)
		if err != nil {
			return nil, cgroupControlCounts{}, fmt.Errorf("read cgroup level %q: %w", current, err)
		}
		present := make(map[string]bool, len(entries))
		for _, entry := range entries {
			present[entry.Name()] = true
		}
		for _, filename := range files {
			if !present[filename] {
				continue
			}
			controlPath := filepath.Join(current, filename)
			if topologyErr := topology.validatePath(mount, controlPath); topologyErr != nil {
				return nil, cgroupControlCounts{}, topologyErr
			}
			if validateErr := validator(mount, controlPath); validateErr != nil {
				return nil, cgroupControlCounts{}, fmt.Errorf("validate cgroup control %q: %w", controlPath, validateErr)
			}
			contents, readErr := readRegularFile(controlPath, maximumMetadataBytes)
			if readErr != nil {
				return nil, cgroupControlCounts{}, fmt.Errorf("read cgroup control %q: %w", controlPath, readErr)
			}
			value, valueErr := normalizeCgroupControlValue(contents)
			if valueErr != nil {
				return nil, cgroupControlCounts{}, fmt.Errorf("read cgroup control %q: %w", controlPath, valueErr)
			}
			records = append(records, fmt.Sprintf("%s level=%q file=%q value=%q", recordPrefix, level, filename, value))
			if strings.HasPrefix(filename, "cpuset.") {
				controlCounts.cpuset++
			} else {
				controlCounts.cpu++
			}
		}
		if current == mount.mountPoint {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil, cgroupControlCounts{}, errors.New("cgroup hierarchy reached filesystem root before mount point")
		}
	}
	return records, controlCounts, nil
}

func validateCgroupControlSurface(mount cgroupMount, counts cgroupControlCounts) error {
	if mount.filesystem == "cgroup2" {
		if counts.cpu+counts.cpuset == 0 {
			return fmt.Errorf("no CPU control files found for cgroup2 mount %q", mount.mountPoint)
		}
		return nil
	}
	// Each v1 controller owns a separate control surface; records from a
	// sibling hierarchy must not mask a matched but empty CPU hierarchy.
	if containsString(mount.controllers, "cpu") && counts.cpu == 0 {
		return fmt.Errorf("no cpu controls found for cgroup v1 mount %q", mount.mountPoint)
	}
	if containsString(mount.controllers, "cpuset") && counts.cpuset == 0 {
		return fmt.Errorf("no cpuset controls found for cgroup v1 mount %q", mount.mountPoint)
	}
	return nil
}

func constraintFilesForMount(mount cgroupMount) []string {
	if mount.filesystem == "cgroup2" {
		return append([]string(nil), cgroupV2ConstraintFiles...)
	}
	fileSet := make(map[string]bool)
	if containsString(mount.controllers, "cpu") {
		for _, filename := range cgroupV1CPUConstraintFiles {
			fileSet[filename] = true
		}
	}
	if containsString(mount.controllers, "cpuset") {
		for _, filename := range cgroupCPUSetConstraintFiles {
			fileSet[filename] = true
		}
	}
	files := make([]string, 0, len(fileSet))
	for filename := range fileSet {
		files = append(files, filename)
	}
	sort.Strings(files)
	return files
}

func normalizeCgroupControlValue(contents []byte) (string, error) {
	if !utf8.Valid(contents) || strings.ContainsRune(string(contents), '\x00') {
		return "", errors.New("value is not valid text")
	}
	value := strings.TrimSpace(string(contents))
	if strings.ContainsAny(value, "\r\n") {
		return "", errors.New("value is multiline")
	}
	if value == "" {
		return "<empty>", nil
	}
	return strings.Join(strings.Fields(value), " "), nil
}

func nonEmptyProcLines(contents []byte) ([]string, error) {
	if !utf8.Valid(contents) || strings.ContainsRune(string(contents), '\x00') {
		return nil, errors.New("file is not valid text")
	}
	text := strings.TrimSuffix(string(contents), "\n")
	if text == "" {
		return nil, errors.New("file is empty")
	}
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if line == "" || strings.ContainsRune(line, '\r') {
			return nil, fmt.Errorf("line %d is empty or contains a carriage return", index+1)
		}
	}
	return lines, nil
}

func parseControllerList(value string) ([]string, error) {
	seen := make(map[string]bool)
	controllers := strings.Split(value, ",")
	for _, controller := range controllers {
		if controller == "" || strings.ContainsAny(controller, ":/\\ \t\r\n\x00") {
			return nil, fmt.Errorf("invalid controller name %q", controller)
		}
		if seen[controller] {
			return nil, fmt.Errorf("duplicate controller %q", controller)
		}
		seen[controller] = true
	}
	sort.Strings(controllers)
	return controllers, nil
}

func relevantCgroupControllers(controllers []string) []string {
	var relevant []string
	for _, controller := range controllers {
		if isRelevantCgroupController(controller) {
			relevant = append(relevant, controller)
		}
	}
	return relevant
}

func isRelevantCgroupController(controller string) bool {
	// cpuacct is accounting-only; combined cpu,cpuacct hierarchies are covered
	// by cpu, while cpuacct-only membership cannot change benchmark scheduling.
	return controller == "cpu" || controller == "cpuset"
}

func canonicalCgroupPath(value string) (string, error) {
	if value == "" || !strings.HasPrefix(value, "/") || strings.ContainsRune(value, '\x00') {
		return "", errors.New("must be an absolute cgroup path")
	}
	if cleaned := pathpkg.Clean(value); cleaned != value {
		return "", fmt.Errorf("must be a canonical cgroup path, got %q", value)
	}
	return value, nil
}

func unescapeMountInfoPath(value string) (string, error) {
	var decoded strings.Builder
	decoded.Grow(len(value))
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			decoded.WriteByte(value[index])
			continue
		}
		if index+3 >= len(value) {
			return "", errors.New("truncated mountinfo escape")
		}
		digits := value[index+1 : index+4]
		parsed, err := strconv.ParseUint(digits, 8, 8)
		if err != nil {
			return "", fmt.Errorf("invalid mountinfo escape \\%s", digits)
		}
		character := byte(parsed)
		if character != ' ' && character != '\t' && character != '\n' && character != '\\' {
			return "", fmt.Errorf("unsupported mountinfo escape \\%s", digits)
		}
		decoded.WriteByte(character)
		index += 3
	}
	return decoded.String(), nil
}

func pathWithin(base, target string) bool {
	relative, err := filepath.Rel(base, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func containsString(values []string, wanted string) bool {
	index := sort.SearchStrings(values, wanted)
	return index < len(values) && values[index] == wanted
}

func containsStringValue(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
