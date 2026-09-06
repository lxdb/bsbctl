// Package soak provides fail-closed process accounting for the release soak.
package soak

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	DaemonName                    = "bsbctl"
	CodexQuotaName                = "bsbctl-plugin-codex-quota"
	GitHubNotificationsName       = "bsbctl-plugin-github-notifications"
	MacResourcesName              = "bsbctl-plugin-mac-resources"
	DefaultCPUPercent             = 1.0
	DefaultRSSBytes         int64 = 100 << 20
)

var daemonProcessNames = map[string]struct{}{
	DaemonName:              {},
	CodexQuotaName:          {},
	GitHubNotificationsName: {},
	MacResourcesName:        {},
}

type ProcessSnapshot struct {
	PID        int     `json:"pid"`
	PPID       int     `json:"ppid"`
	Name       string  `json:"name"`
	CPUSeconds float64 `json:"cpu_seconds"`
	RSSBytes   int64   `json:"rss_bytes"`
}

type ProcessIdentity struct {
	PID  int    `json:"pid"`
	PPID int    `json:"ppid"`
	Name string `json:"name"`
}

type ProcessMeasurement struct {
	ProcessIdentity
	CPUSecondsDelta float64 `json:"cpu_seconds_delta"`
	CPUPercent      float64 `json:"cpu_percent"`
	RSSBytes        int64   `json:"rss_bytes"`
}

type Sample struct {
	Index                int                  `json:"index"`
	StartedAt            time.Time            `json:"started_at"`
	CapturedAt           time.Time            `json:"captured_at"`
	IntervalMilliseconds int64                `json:"interval_milliseconds"`
	Processes            []ProcessMeasurement `json:"processes"`
	AggregateCPUPercent  float64              `json:"aggregate_cpu_percent"`
	AggregateRSSBytes    int64                `json:"aggregate_rss_bytes"`
}

type Limits struct {
	CPUPercent float64 `json:"aggregate_cpu_percent"`
	RSSBytes   int64   `json:"aggregate_rss_bytes"`
}

type Summary struct {
	SampleCount             int     `json:"sample_count"`
	MeanAggregateCPUPercent float64 `json:"mean_aggregate_cpu_percent"`
	MaxAggregateCPUPercent  float64 `json:"max_aggregate_cpu_percent"`
	MeanAggregateRSSBytes   int64   `json:"mean_aggregate_rss_bytes"`
	MaxAggregateRSSBytes    int64   `json:"max_aggregate_rss_bytes"`
	Limits                  Limits  `json:"limits"`
}

// ParseProcessSnapshot parses the header-free output of
// ps -axo pid=,ppid=,time=,rss=,comm=. RSS is converted from KiB to bytes.
func ParseProcessSnapshot(reader io.Reader) ([]ProcessSnapshot, error) {
	scanner := bufio.NewScanner(reader)
	rows := make([]ProcessSnapshot, 0)
	seen := make(map[int]struct{})
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 5 {
			return nil, fmt.Errorf("process telemetry row has %d fields, want 5", len(fields))
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			return nil, errors.New("process telemetry has invalid pid")
		}
		if _, exists := seen[pid]; exists {
			return nil, fmt.Errorf("process telemetry repeats pid %d", pid)
		}
		seen[pid] = struct{}{}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil || ppid < 0 {
			return nil, fmt.Errorf("process telemetry for pid %d has invalid ppid", pid)
		}
		cpuSeconds, err := parseCPUTime(fields[2])
		if err != nil {
			return nil, fmt.Errorf("process telemetry for pid %d has invalid cpu time: %w", pid, err)
		}
		rssKiB, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || rssKiB < 0 || rssKiB > math.MaxInt64/1024 {
			return nil, fmt.Errorf("process telemetry for pid %d has invalid rss", pid)
		}
		name := filepath.Base(fields[4])
		if name == "." || name == string(filepath.Separator) || name == "" {
			return nil, fmt.Errorf("process telemetry for pid %d has invalid command", pid)
		}
		rows = append(rows, ProcessSnapshot{
			PID: pid, PPID: ppid, Name: name, CPUSeconds: cpuSeconds, RSSBytes: rssKiB * 1024,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read process telemetry: %w", err)
	}
	if len(rows) == 0 {
		return nil, errors.New("process telemetry is empty")
	}
	return rows, nil
}

func parseCPUTime(value string) (float64, error) {
	days := int64(0)
	clock := value
	if before, after, found := strings.Cut(value, "-"); found {
		parsed, err := strconv.ParseInt(before, 10, 64)
		if err != nil || parsed < 0 || strings.Contains(after, "-") {
			return 0, errors.New("invalid day component")
		}
		days = parsed
		clock = after
	}
	parts := strings.Split(clock, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, errors.New("invalid clock component")
	}
	hours := int64(0)
	minutesIndex := 0
	if len(parts) == 3 {
		var err error
		hours, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || hours < 0 {
			return 0, errors.New("invalid hour component")
		}
		minutesIndex = 1
	}
	minutes, err := strconv.ParseInt(parts[minutesIndex], 10, 64)
	if err != nil || minutes < 0 || (len(parts) == 3 && minutes >= 60) {
		return 0, errors.New("invalid minute component")
	}
	seconds, err := strconv.ParseFloat(parts[minutesIndex+1], 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds >= 60 {
		return 0, errors.New("invalid second component")
	}
	return float64(days*24*60*60+hours*60*60+minutes*60) + seconds, nil
}

// SelectDaemonTree identifies exactly one bsbctl process and its exact set of
// resident plugin children. Unrelated helper processes are ignored unless they
// are direct children of the daemon, in which case accounting fails closed.
func SelectDaemonTree(rows []ProcessSnapshot, parentPID int) ([]ProcessIdentity, error) {
	var parent *ProcessSnapshot
	children := make([]ProcessSnapshot, 0, len(daemonProcessNames)-1)
	for index := range rows {
		row := &rows[index]
		if row.PID == parentPID {
			parent = row
		}
		if row.PPID == parentPID {
			children = append(children, *row)
		}
	}
	if parent == nil || parent.Name != DaemonName {
		return nil, errors.New("bsbctl parent process telemetry is unavailable")
	}
	if len(children) != len(daemonProcessNames)-1 {
		return nil, fmt.Errorf("bsbctl has %d direct children, want exactly %d resident plugins", len(children), len(daemonProcessNames)-1)
	}
	identities := []ProcessIdentity{{PID: parent.PID, PPID: parent.PPID, Name: parent.Name}}
	seen := map[string]struct{}{DaemonName: {}}
	for _, child := range children {
		if _, allowed := daemonProcessNames[child.Name]; !allowed || child.Name == DaemonName {
			return nil, fmt.Errorf("bsbctl has unexpected direct child %q", child.Name)
		}
		if _, duplicate := seen[child.Name]; duplicate {
			return nil, fmt.Errorf("bsbctl has duplicate resident child %q", child.Name)
		}
		seen[child.Name] = struct{}{}
		identities = append(identities, ProcessIdentity{PID: child.PID, PPID: child.PPID, Name: child.Name})
	}
	if _, ok := seen[CodexQuotaName]; !ok {
		return nil, errors.New("Codex quota resident process telemetry is unavailable")
	}
	if _, ok := seen[GitHubNotificationsName]; !ok {
		return nil, errors.New("GitHub Notifications resident process telemetry is unavailable")
	}
	if _, ok := seen[MacResourcesName]; !ok {
		return nil, errors.New("Mac resources resident process telemetry is unavailable")
	}
	slices.SortFunc(identities, func(left, right ProcessIdentity) int { return cmp.Compare(left.Name, right.Name) })
	return identities, nil
}

func MeasureInterval(index int, startedAt, capturedAt time.Time, identities []ProcessIdentity, before, after []ProcessSnapshot) (Sample, error) {
	interval := capturedAt.Sub(startedAt)
	if index < 1 || interval <= 0 {
		return Sample{}, errors.New("process measurement interval is invalid")
	}
	if len(identities) != len(daemonProcessNames) {
		return Sample{}, errors.New("process identity set is incomplete")
	}
	beforeByPID := snapshotByPID(before)
	afterByPID := snapshotByPID(after)
	result := Sample{
		Index: index, StartedAt: startedAt.UTC(), CapturedAt: capturedAt.UTC(),
		IntervalMilliseconds: interval.Milliseconds(), Processes: make([]ProcessMeasurement, 0, len(identities)),
	}
	for _, identity := range identities {
		start, startOK := beforeByPID[identity.PID]
		end, endOK := afterByPID[identity.PID]
		if !startOK || !endOK || !sameProcess(identity, start) || !sameProcess(identity, end) {
			return Sample{}, fmt.Errorf("process telemetry is unavailable for %s pid %d", identity.Name, identity.PID)
		}
		if end.RSSBytes <= 0 {
			return Sample{}, fmt.Errorf("rss telemetry is unavailable for %s pid %d", identity.Name, identity.PID)
		}
		delta := end.CPUSeconds - start.CPUSeconds
		if delta < -1e-9 {
			return Sample{}, fmt.Errorf("cpu telemetry regressed for %s pid %d", identity.Name, identity.PID)
		}
		if delta < 0 {
			delta = 0
		}
		cpuPercent := delta / interval.Seconds() * 100
		measurement := ProcessMeasurement{
			ProcessIdentity: identity, CPUSecondsDelta: delta, CPUPercent: cpuPercent, RSSBytes: end.RSSBytes,
		}
		result.Processes = append(result.Processes, measurement)
		result.AggregateCPUPercent += cpuPercent
		result.AggregateRSSBytes += end.RSSBytes
	}
	return result, nil
}

func snapshotByPID(rows []ProcessSnapshot) map[int]ProcessSnapshot {
	result := make(map[int]ProcessSnapshot, len(rows))
	for _, row := range rows {
		result[row.PID] = row
	}
	return result
}

func sameProcess(identity ProcessIdentity, snapshot ProcessSnapshot) bool {
	return identity.PID == snapshot.PID && identity.PPID == snapshot.PPID && identity.Name == snapshot.Name
}

// Evaluate applies the aggregate guardrails to every warm steady-state sample.
func Evaluate(samples []Sample, limits Limits) (Summary, error) {
	if len(samples) == 0 {
		return Summary{}, errors.New("soak process telemetry is unavailable")
	}
	if limits.CPUPercent <= 0 || limits.RSSBytes <= 0 {
		return Summary{}, errors.New("soak guardrails are invalid")
	}
	result := Summary{SampleCount: len(samples), Limits: limits}
	var cpuTotal float64
	var rssTotal int64
	for _, sample := range samples {
		if len(sample.Processes) != 0 && len(sample.Processes) != len(daemonProcessNames) {
			return Summary{}, fmt.Errorf("sample %d has incomplete process telemetry", sample.Index)
		}
		if sample.AggregateCPUPercent < 0 || sample.AggregateRSSBytes < 0 {
			return Summary{}, fmt.Errorf("sample %d has invalid aggregate telemetry", sample.Index)
		}
		if sample.AggregateCPUPercent > limits.CPUPercent+1e-9 {
			return Summary{}, fmt.Errorf("sample %d aggregate cpu %.3f%% exceeds %.3f%%", sample.Index, sample.AggregateCPUPercent, limits.CPUPercent)
		}
		if sample.AggregateRSSBytes > limits.RSSBytes {
			return Summary{}, fmt.Errorf("sample %d aggregate rss %d exceeds %d bytes", sample.Index, sample.AggregateRSSBytes, limits.RSSBytes)
		}
		cpuTotal += sample.AggregateCPUPercent
		rssTotal += sample.AggregateRSSBytes
		result.MaxAggregateCPUPercent = max(result.MaxAggregateCPUPercent, sample.AggregateCPUPercent)
		result.MaxAggregateRSSBytes = max(result.MaxAggregateRSSBytes, sample.AggregateRSSBytes)
	}
	result.MeanAggregateCPUPercent = cpuTotal / float64(len(samples))
	result.MeanAggregateRSSBytes = rssTotal / int64(len(samples))
	return result, nil
}
