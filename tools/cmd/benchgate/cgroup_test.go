package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCPUConstraintsFingerprintV2CollectsMembershipAncestors(t *testing.T) {
	directory := t.TempDir()
	mountPoint := filepath.Join(directory, "cgroup v2")
	mustMkdirAll(t, filepath.Join(mountPoint, "workload.slice", "job.scope"))
	mustWriteFile(t, filepath.Join(mountPoint, "cpu.max"), []byte("max 100000\n"))
	mustWriteFile(t, filepath.Join(mountPoint, "cpu.uclamp.min"), []byte("0.00\n"))
	mustWriteFile(t, filepath.Join(mountPoint, "cpu.weight"), []byte("100\n"))
	mustWriteFile(t, filepath.Join(mountPoint, "cpuset.cpus.effective"), []byte("0-7\n"))
	mustWriteFile(t, filepath.Join(mountPoint, "workload.slice", "cpu.max"), []byte("200000 100000\n"))
	mustWriteFile(t, filepath.Join(mountPoint, "workload.slice", "cpu.max.burst"), []byte("20000\n"))
	mustWriteFile(t, filepath.Join(mountPoint, "workload.slice", "cpu.uclamp.max"), []byte("75.00\n"))
	mustWriteFile(t, filepath.Join(mountPoint, "workload.slice", "cpuset.mems"), []byte("0\n"))
	mustWriteFile(t, filepath.Join(mountPoint, "workload.slice", "job.scope", "cpu.max"), []byte("100000 100000\n"))
	mustWriteFile(t, filepath.Join(mountPoint, "workload.slice", "job.scope", "cpuset.cpus.effective"), []byte("2-3\n"))
	mustWriteFile(t, filepath.Join(mountPoint, "workload.slice", "job.scope", "cpuset.cpus.partition"), []byte("member\n"))

	status, membership, mountInfo := writeCgroupProcFixture(t, directory,
		"Mems_allowed_list:\t0\nCpus_allowed_list:\t0-7\nCpus_allowed:\tff\n",
		"0::/workload.slice/job.scope\n",
		"36 25 0:32 / "+mountInfoEscape(mountPoint)+" rw,nosuid,nodev,noexec,relatime - cgroup2 cgroup2 rw,nsdelegate\n",
	)

	records, err := collectFixtureCPUConstraintRecords(status, membership, mountInfo)
	if err != nil {
		t.Fatalf("collectCPUConstraintRecords() error = %v", err)
	}
	joined := strings.Join(records, "\n")
	for _, want := range []string{
		`mount_id=36 parent_id=25 device="0:32"`,
		`level="/" file="cpu.max" value="max 100000"`,
		`level="/workload.slice" file="cpu.max" value="200000 100000"`,
		`level="/workload.slice" file="cpu.max.burst" value="20000"`,
		`level="/workload.slice" file="cpu.uclamp.max" value="75.00"`,
		`level="/workload.slice/job.scope" file="cpu.max" value="100000 100000"`,
		`level="/workload.slice/job.scope" file="cpuset.cpus.effective" value="2-3"`,
		`level="/workload.slice/job.scope" file="cpuset.cpus.partition" value="member"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("records do not contain %q:\n%s", want, joined)
		}
	}

	before, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
	if err != nil {
		t.Fatalf("cpuConstraintsFingerprintFiles() error = %v", err)
	}
	changes := []struct {
		name     string
		filename string
		original string
		changed  string
	}{
		{name: "ancestor quota", filename: filepath.Join(mountPoint, "workload.slice", "cpu.max"), original: "200000 100000\n", changed: "150000 100000\n"},
		{name: "burst", filename: filepath.Join(mountPoint, "workload.slice", "cpu.max.burst"), original: "20000\n", changed: "10000\n"},
		{name: "uclamp max", filename: filepath.Join(mountPoint, "workload.slice", "cpu.uclamp.max"), original: "75.00\n", changed: "50.00\n"},
		{name: "uclamp min", filename: filepath.Join(mountPoint, "cpu.uclamp.min"), original: "0.00\n", changed: "25.00\n"},
		{name: "weight", filename: filepath.Join(mountPoint, "cpu.weight"), original: "100\n", changed: "200\n"},
		{name: "partition", filename: filepath.Join(mountPoint, "workload.slice", "job.scope", "cpuset.cpus.partition"), original: "member\n", changed: "root\n"},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			mustWriteFile(t, change.filename, []byte(change.changed))
			after, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
			if err != nil {
				t.Fatalf("fingerprint after control update: %v", err)
			}
			if before == after {
				t.Fatalf("fingerprint did not change after updating %s", change.filename)
			}
			mustWriteFile(t, change.filename, []byte(change.original))
		})
	}
}

func TestCPUConstraintsFingerprintV1CollectsControllersAndIsOrderStable(t *testing.T) {
	directory := t.TempDir()
	cpuMount := filepath.Join(directory, "cpu")
	cpusetMount := filepath.Join(directory, "cpuset")
	for _, mountPoint := range []string{cpuMount, cpusetMount} {
		mustMkdirAll(t, filepath.Join(mountPoint, "workload", "job"))
	}
	mustWriteFile(t, filepath.Join(cpuMount, "cpu.cfs_period_us"), []byte("100000\n"))
	mustWriteFile(t, filepath.Join(cpuMount, "cpu.idle"), []byte("0\n"))
	mustWriteFile(t, filepath.Join(cpuMount, "cpu.shares"), []byte("1024\n"))
	mustWriteFile(t, filepath.Join(cpuMount, "workload", "cpu.cfs_burst_us"), []byte("25000\n"))
	mustWriteFile(t, filepath.Join(cpuMount, "workload", "cpu.cfs_quota_us"), []byte("300000\n"))
	mustWriteFile(t, filepath.Join(cpuMount, "workload", "cpu.uclamp.max"), []byte("75.00\n"))
	mustWriteFile(t, filepath.Join(cpuMount, "workload", "cpu.uclamp.min"), []byte("0.00\n"))
	mustWriteFile(t, filepath.Join(cpuMount, "workload", "job", "cpu.cfs_quota_us"), []byte("100000\n"))
	mustWriteFile(t, filepath.Join(cpusetMount, "cpuset.cpus"), []byte("0-7\n"))
	mustWriteFile(t, filepath.Join(cpusetMount, "workload", "cpuset.effective_mems"), []byte("0\n"))
	mustWriteFile(t, filepath.Join(cpusetMount, "workload", "job", "cpuset.effective_cpus"), []byte("4-5\n"))

	statusText := "Cpus_allowed:\tff\nCpus_allowed_list:\t0-7\nMems_allowed_list:\t0\n"
	membershipText := "0::/\n8:cpuacct:/\n7:cpuset:/workload/job\n5:cpuacct,cpu:/workload/job\n"
	mountInfoText := "41 25 0:41 / " + mountInfoEscape(cpusetMount) + " rw - cgroup cgroup rw,cpuset\n" +
		"40 25 0:40 / " + mountInfoEscape(cpuMount) + " rw - cgroup cgroup rw,cpuacct,cpu\n"
	status, membership, mountInfo := writeCgroupProcFixture(t, directory, statusText, membershipText, mountInfoText)

	records, err := collectFixtureCPUConstraintRecords(status, membership, mountInfo)
	if err != nil {
		t.Fatalf("collectCPUConstraintRecords() error = %v", err)
	}
	joined := strings.Join(records, "\n")
	for _, want := range []string{
		`level="/" file="cpu.cfs_period_us" value="100000"`,
		`level="/" file="cpu.idle" value="0"`,
		`level="/" file="cpu.shares" value="1024"`,
		`level="/workload" file="cpu.cfs_burst_us" value="25000"`,
		`level="/workload" file="cpu.uclamp.max" value="75.00"`,
		`level="/workload" file="cpu.uclamp.min" value="0.00"`,
		`level="/workload" file="cpu.cfs_quota_us" value="300000"`,
		`level="/workload/job" file="cpu.cfs_quota_us" value="100000"`,
		`level="/workload/job" file="cpuset.effective_cpus" value="4-5"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("records do not contain %q:\n%s", want, joined)
		}
	}

	before, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
	if err != nil {
		t.Fatalf("cpuConstraintsFingerprintFiles() error = %v", err)
	}
	v1Changes := []struct {
		name     string
		filename string
		original string
		changed  string
	}{
		{name: "burst", filename: filepath.Join(cpuMount, "workload", "cpu.cfs_burst_us"), original: "25000\n", changed: "12500\n"},
		{name: "idle", filename: filepath.Join(cpuMount, "cpu.idle"), original: "0\n", changed: "1\n"},
		{name: "uclamp max", filename: filepath.Join(cpuMount, "workload", "cpu.uclamp.max"), original: "75.00\n", changed: "50.00\n"},
		{name: "uclamp min", filename: filepath.Join(cpuMount, "workload", "cpu.uclamp.min"), original: "0.00\n", changed: "25.00\n"},
	}
	for _, change := range v1Changes {
		mustWriteFile(t, change.filename, []byte(change.changed))
		changed, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
		if err != nil {
			t.Fatalf("fingerprint after v1 %s update: %v", change.name, err)
		}
		if changed == before {
			t.Fatalf("fingerprint did not change when v1 %s changed", change.name)
		}
		mustWriteFile(t, change.filename, []byte(change.original))
	}
	mustWriteFile(t, status, []byte("Mems_allowed_list:\t0\nCpus_allowed_list:\t0-7\nCpus_allowed:\tff\n"))
	mustWriteFile(t, membership, []byte("5:cpu,cpuacct:/workload/job\n7:cpuset:/workload/job\n8:cpuacct:/\n0::/\n"))
	mustWriteFile(t, mountInfo, []byte(
		"40 25 0:40 / "+mountInfoEscape(cpuMount)+" rw - cgroup cgroup rw,cpu,cpuacct\n"+
			"41 25 0:41 / "+mountInfoEscape(cpusetMount)+" rw - cgroup cgroup rw,cpuset\n",
	))
	after, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
	if err != nil {
		t.Fatalf("cpuConstraintsFingerprintFiles() after reorder error = %v", err)
	}
	if before != after {
		t.Fatalf("fingerprint changed after semantically equivalent input reorder:\nbefore %s\nafter  %s", before, after)
	}
}

func TestCPUConstraintsFingerprintRejectsHiddenCgroupNamespaceAncestry(t *testing.T) {
	directory := t.TempDir()
	mountPoint := filepath.Join(directory, "cgroup")
	mustMkdirAll(t, filepath.Join(mountPoint, "job.scope"))
	mustWriteFile(t, filepath.Join(mountPoint, "cpu.max"), []byte("max 100000\n"))
	mustWriteFile(t, filepath.Join(mountPoint, "job.scope", "cpu.max"), []byte("50000 100000\n"))
	status, membership, mountInfo := writeCgroupProcFixture(t, directory,
		"Cpus_allowed_list:\t0-3\n",
		"0::/job.scope\n",
		"36 25 0:32 /.. "+mountInfoEscape(mountPoint)+" rw - cgroup2 cgroup2 rw\n",
	)

	_, err := collectFixtureCPUConstraintRecords(status, membership, mountInfo)
	if err == nil || !strings.Contains(err.Error(), "full cgroup hierarchy") {
		t.Fatalf("error = %v, want hidden-ancestry error", err)
	}
}

func TestCPUConstraintsFingerprintFailsClosed(t *testing.T) {
	t.Run("root membership requires ancestry proof", func(t *testing.T) {
		directory := t.TempDir()
		mountPoint := filepath.Join(directory, "cgroup")
		mustMkdirAll(t, mountPoint)
		mustWriteFile(t, filepath.Join(mountPoint, "cpu.max"), []byte("max 100000\n"))
		status, membership, mountInfo := writeCgroupProcFixture(t, directory,
			"Cpus_allowed_list:\t0-3\n",
			"0::/\n",
			"36 25 0:32 / "+mountInfoEscape(mountPoint)+" rw - cgroup2 cgroup2 rw\n",
		)
		if _, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo); err != nil {
			t.Fatalf("proven root membership was rejected: %v", err)
		}
		_, err := cpuConstraintsFingerprintFilesWithValidator(status, membership, mountInfo, validateFixtureCgroupPath, false)
		if err == nil || !strings.Contains(err.Error(), "has not been proven") {
			t.Fatalf("error = %v, want missing ancestry proof", err)
		}
	})

	t.Run("non-canonical membership", func(t *testing.T) {
		directory := t.TempDir()
		mountPoint := filepath.Join(directory, "cgroup")
		mustMkdirAll(t, mountPoint)
		status, membership, mountInfo := writeCgroupProcFixture(t, directory,
			"Cpus_allowed_list:\t0-3\n",
			"0::/team/../escape\n",
			"36 25 0:32 / "+mountInfoEscape(mountPoint)+" rw - cgroup2 cgroup2 rw\n",
		)
		_, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
		if err == nil || !strings.Contains(err.Error(), "canonical") {
			t.Fatalf("error = %v, want canonical-path error", err)
		}
	})

	t.Run("missing controller mount", func(t *testing.T) {
		directory := t.TempDir()
		status, membership, mountInfo := writeCgroupProcFixture(t, directory,
			"Cpus_allowed_list:\t0-3\n",
			"4:cpu,cpuacct:/job\n",
			"36 25 0:32 / "+mountInfoEscape(directory)+" rw - tmpfs tmpfs rw\n",
		)
		_, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
		if err == nil || !strings.Contains(err.Error(), "no cgroup mount") {
			t.Fatalf("error = %v, want missing-mount error", err)
		}
	})

	t.Run("v1 mount source cannot spoof controller", func(t *testing.T) {
		directory := t.TempDir()
		spoofedCPUMount := filepath.Join(directory, "memory")
		cpusetMount := filepath.Join(directory, "cpuset")
		mustMkdirAll(t, filepath.Join(spoofedCPUMount, "job"))
		mustMkdirAll(t, filepath.Join(cpusetMount, "job"))
		mustWriteFile(t, filepath.Join(cpusetMount, "job", "cpuset.cpus"), []byte("0-3\n"))
		status, membership, mountInfo := writeCgroupProcFixture(t, directory,
			"Cpus_allowed_list:\t0-3\n",
			"5:cpu:/job\n7:cpuset:/job\n",
			"40 25 0:40 / "+mountInfoEscape(spoofedCPUMount)+" rw - cgroup cpu rw,memory\n"+
				"41 25 0:41 / "+mountInfoEscape(cpusetMount)+" rw - cgroup cgroup rw,cpuset\n",
		)
		_, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
		if err == nil || !strings.Contains(err.Error(), "no cgroup mount exposes") {
			t.Fatalf("error = %v, want missing CPU-controller mount", err)
		}
	})

	t.Run("v1 noprefix controls", func(t *testing.T) {
		directory := t.TempDir()
		mountPoint := filepath.Join(directory, "cpu")
		mustMkdirAll(t, filepath.Join(mountPoint, "job"))
		mustWriteFile(t, filepath.Join(mountPoint, "job", "cfs_quota_us"), []byte("100000\n"))
		mustWriteFile(t, filepath.Join(mountPoint, "job", "cfs_period_us"), []byte("100000\n"))
		status, membership, mountInfo := writeCgroupProcFixture(t, directory,
			"Cpus_allowed_list:\t0-3\n",
			"5:cpu:/job\n",
			"40 25 0:40 / "+mountInfoEscape(mountPoint)+" rw - cgroup cgroup rw,noprefix,cpu\n",
		)
		_, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
		if err == nil || !strings.Contains(err.Error(), "noprefix") {
			t.Fatalf("error = %v, want unsupported-noprefix error", err)
		}
	})

	t.Run("cpuset cannot mask empty CPU hierarchy", func(t *testing.T) {
		directory := t.TempDir()
		cpuMount := filepath.Join(directory, "cpu")
		cpusetMount := filepath.Join(directory, "cpuset")
		mustMkdirAll(t, filepath.Join(cpuMount, "job"))
		mustMkdirAll(t, filepath.Join(cpusetMount, "job"))
		mustWriteFile(t, filepath.Join(cpusetMount, "job", "cpuset.cpus"), []byte("0-3\n"))
		status, membership, mountInfo := writeCgroupProcFixture(t, directory,
			"Cpus_allowed_list:\t0-3\n",
			"5:cpu:/job\n7:cpuset:/job\n",
			"40 25 0:40 / "+mountInfoEscape(cpuMount)+" rw - cgroup cgroup rw,cpu\n"+
				"41 25 0:41 / "+mountInfoEscape(cpusetMount)+" rw - cgroup cgroup rw,cpuset\n",
		)
		_, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
		if err == nil || !strings.Contains(err.Error(), "no cpu controls") {
			t.Fatalf("error = %v, want empty-CPU-hierarchy error", err)
		}
	})

	t.Run("CPU cannot mask empty cpuset hierarchy", func(t *testing.T) {
		directory := t.TempDir()
		cpuMount := filepath.Join(directory, "cpu")
		cpusetMount := filepath.Join(directory, "cpuset")
		mustMkdirAll(t, filepath.Join(cpuMount, "job"))
		mustMkdirAll(t, filepath.Join(cpusetMount, "job"))
		mustWriteFile(t, filepath.Join(cpuMount, "job", "cpu.shares"), []byte("1024\n"))
		status, membership, mountInfo := writeCgroupProcFixture(t, directory,
			"Cpus_allowed_list:\t0-3\n",
			"5:cpu:/job\n7:cpuset:/job\n",
			"40 25 0:40 / "+mountInfoEscape(cpuMount)+" rw - cgroup cgroup rw,cpu\n"+
				"41 25 0:41 / "+mountInfoEscape(cpusetMount)+" rw - cgroup cgroup rw,cpuset\n",
		)
		_, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
		if err == nil || !strings.Contains(err.Error(), "no cpuset controls") {
			t.Fatalf("error = %v, want empty-cpuset-hierarchy error", err)
		}
	})

	t.Run("symlink escape", func(t *testing.T) {
		directory := t.TempDir()
		mountPoint := filepath.Join(directory, "cgroup")
		outside := filepath.Join(directory, "outside")
		mustMkdirAll(t, filepath.Join(mountPoint))
		mustMkdirAll(t, filepath.Join(outside, "job"))
		if err := os.Symlink(outside, filepath.Join(mountPoint, "escape")); err != nil {
			t.Fatalf("create escape symlink: %v", err)
		}
		status, membership, mountInfo := writeCgroupProcFixture(t, directory,
			"Cpus_allowed_list:\t0-3\n",
			"0::/escape/job\n",
			"36 25 0:32 / "+mountInfoEscape(mountPoint)+" rw - cgroup2 cgroup2 rw\n",
		)
		_, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
		if err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("error = %v, want path-escape error", err)
		}
	})

	t.Run("non-regular control", func(t *testing.T) {
		directory := t.TempDir()
		mountPoint := filepath.Join(directory, "cgroup")
		mustMkdirAll(t, filepath.Join(mountPoint, "job", "cpu.max"))
		status, membership, mountInfo := writeCgroupProcFixture(t, directory,
			"Cpus_allowed_list:\t0-3\n",
			"0::/job\n",
			"36 25 0:32 / "+mountInfoEscape(mountPoint)+" rw - cgroup2 cgroup2 rw\n",
		)
		_, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("error = %v, want non-regular-file error", err)
		}
	})

	t.Run("same-path tmpfs overmount", func(t *testing.T) {
		directory := t.TempDir()
		mountPoint := filepath.Join(directory, "cgroup")
		mustMkdirAll(t, filepath.Join(mountPoint, "job"))
		mustWriteFile(t, filepath.Join(mountPoint, "job", "cpu.max"), []byte("max 100000\n"))
		status, membership, mountInfo := writeCgroupProcFixture(t, directory,
			"Cpus_allowed_list:\t0-3\n",
			"0::/job\n",
			"36 25 0:32 / "+mountInfoEscape(mountPoint)+" rw - cgroup2 cgroup2 rw\n"+
				"37 25 0:99 / "+mountInfoEscape(mountPoint)+" rw - tmpfs tmpfs rw\n",
		)
		_, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
		if err == nil || !strings.Contains(err.Error(), "overmounted") {
			t.Fatalf("error = %v, want overmount error", err)
		}
	})

	t.Run("nested mount on membership ancestry", func(t *testing.T) {
		directory := t.TempDir()
		mountPoint := filepath.Join(directory, "cgroup")
		nested := filepath.Join(mountPoint, "team")
		mustMkdirAll(t, filepath.Join(nested, "job"))
		mustWriteFile(t, filepath.Join(nested, "job", "cpu.max"), []byte("max 100000\n"))
		status, membership, mountInfo := writeCgroupProcFixture(t, directory,
			"Cpus_allowed_list:\t0-3\n",
			"0::/team/job\n",
			"36 25 0:32 / "+mountInfoEscape(mountPoint)+" rw - cgroup2 cgroup2 rw\n"+
				"38 36 0:100 / "+mountInfoEscape(nested)+" rw - tmpfs tmpfs rw\n",
		)
		_, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
		if err == nil || !strings.Contains(err.Error(), "nested mount") {
			t.Fatalf("error = %v, want nested-mount error", err)
		}
	})

	t.Run("no CPU controls", func(t *testing.T) {
		directory := t.TempDir()
		mountPoint := filepath.Join(directory, "cgroup")
		mustMkdirAll(t, filepath.Join(mountPoint, "job"))
		status, membership, mountInfo := writeCgroupProcFixture(t, directory,
			"Cpus_allowed_list:\t0-3\n",
			"0::/job\n",
			"36 25 0:32 / "+mountInfoEscape(mountPoint)+" rw - cgroup2 cgroup2 rw\n",
		)
		_, err := fixtureCPUConstraintsFingerprint(status, membership, mountInfo)
		if err == nil || !strings.Contains(err.Error(), "no CPU control files") {
			t.Fatalf("error = %v, want missing-control error", err)
		}
	})
}

func writeCgroupProcFixture(t *testing.T, directory, status, membership, mountInfo string) (string, string, string) {
	t.Helper()
	statusPath := filepath.Join(directory, "status")
	membershipPath := filepath.Join(directory, "cgroup.membership")
	mountInfoPath := filepath.Join(directory, "mountinfo")
	mustWriteFile(t, statusPath, []byte(status))
	mustWriteFile(t, membershipPath, []byte(membership))
	mustWriteFile(t, mountInfoPath, []byte(mountInfo))
	return statusPath, membershipPath, mountInfoPath
}

func collectFixtureCPUConstraintRecords(status, membership, mountInfo string) ([]string, error) {
	return collectCPUConstraintRecordsWithValidator(status, membership, mountInfo, validateFixtureCgroupPath, true)
}

func fixtureCPUConstraintsFingerprint(status, membership, mountInfo string) (string, error) {
	return cpuConstraintsFingerprintFilesWithValidator(status, membership, mountInfo, validateFixtureCgroupPath, true)
}

func validateFixtureCgroupPath(cgroupMount, string) error {
	return nil
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("create directory %s: %v", path, err)
	}
}

func mountInfoEscape(value string) string {
	return strings.NewReplacer("\\", `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`).Replace(value)
}
