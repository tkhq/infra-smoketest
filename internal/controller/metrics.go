package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	"github.com/tkhq/infra-smoketest/internal/definition"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var runMetricPhases = []string{"Pending", "Running", "CleaningUp", "Succeeded", "Failed", "Cancelled", "Unknown"}

type runMetrics struct {
	reader        client.Reader
	runs          *prometheus.Desc
	cleanup       *prometheus.Desc
	oldestPending *prometheus.Desc
	lastCompleted *prometheus.Desc
	lastDuration  *prometheus.Desc
}

// NewRunMetrics collects gauges from the manager's cache. Rebuilding each scrape
// reflects existing Runs after restart and removes deleted Runs without tracking
// individual Run identities in metric labels or process-local counters.
func NewRunMetrics(reader client.Reader) prometheus.Collector {
	// Namespace is supplied by the scrape target; emitting it here would clash.
	labels := []string{"smoketest"}
	phaseLabels := []string{"smoketest", "phase"}
	return &runMetrics{
		reader:        reader,
		runs:          prometheus.NewDesc("infra_smoketest_runs", "Number of retained SmokeTestRuns by current phase.", phaseLabels, nil),
		cleanup:       prometheus.NewDesc("infra_smoketest_cleanup_pending_runs", "Number of SmokeTestRuns whose cleanup has started but is not complete, including failed Runs.", labels, nil),
		oldestPending: prometheus.NewDesc("infra_smoketest_oldest_pending_run_timestamp_seconds", "Creation time of the oldest pending SmokeTestRun as a Unix timestamp; absent when none are pending.", labels, nil),
		lastCompleted: prometheus.NewDesc("infra_smoketest_last_run_completed_timestamp_seconds", "Completion time of the latest retained SmokeTestRun with completed cleanup, by terminal phase, as a Unix timestamp.", phaseLabels, nil),
		lastDuration:  prometheus.NewDesc("infra_smoketest_last_run_duration_seconds", "Execution and cleanup duration of the latest retained completed SmokeTestRun by terminal phase; absent if that Run never started.", phaseLabels, nil),
	}
}

func (m *runMetrics) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{m.runs, m.cleanup, m.oldestPending, m.lastCompleted, m.lastDuration} {
		ch <- desc
	}
}

type runMetricSummary struct {
	counts        map[string]float64
	cleanup       float64
	oldestPending time.Time
	latest        map[string]*api.SmokeTestRun
}

func (m *runMetrics) Collect(ch chan<- prometheus.Metric) {
	// Bound waits for cache synchronization. A collection error fails the scrape
	// instead of presenting a misleading zero count or a previous snapshot.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tests := &api.SmokeTestList{}
	if err := m.reader.List(ctx, tests, client.InNamespace(definition.Namespace)); err != nil {
		ch <- prometheus.NewInvalidMetric(m.runs, fmt.Errorf("list SmokeTests for metrics: %w", err))
		return
	}
	runs := &api.SmokeTestRunList{}
	if err := m.reader.List(ctx, runs, client.InNamespace(definition.Namespace)); err != nil {
		ch <- prometheus.NewInvalidMetric(m.runs, fmt.Errorf("list SmokeTestRuns for metrics: %w", err))
		return
	}
	summaries := map[string]*runMetricSummary{}
	ensure := func(test string) *runMetricSummary {
		if summaries[test] == nil {
			summaries[test] = &runMetricSummary{counts: map[string]float64{}, latest: map[string]*api.SmokeTestRun{}}
		}
		return summaries[test]
	}
	// Emit zero counts for installed definitions that have never run, and keep
	// reporting retained Runs even if their definition has been deleted.
	for _, test := range tests.Items {
		ensure(test.Name)
	}
	for i := range runs.Items {
		run := &runs.Items[i]
		summary := ensure(run.Spec.TestRef.Name)
		phase := run.Status.Phase
		if phase == "" {
			phase = "Pending"
		} else if !slices.Contains(runMetricPhases, phase) {
			phase = "Unknown"
		}
		summary.counts[phase]++
		cleaned := meta.IsStatusConditionTrue(run.Status.Conditions, "CleanupComplete")
		if run.Status.CleanupStartedAt != nil && !cleaned {
			summary.cleanup++
		}
		if phase == "Pending" && !run.CreationTimestamp.IsZero() && (summary.oldestPending.IsZero() || run.CreationTimestamp.Time.Before(summary.oldestPending)) {
			summary.oldestPending = run.CreationTimestamp.Time
		}
		if !cleaned || run.Status.CompletedAt == nil || run.Status.CompletedAt.IsZero() || (phase != "Succeeded" && phase != "Failed" && phase != "Cancelled") {
			continue
		}
		previous := summary.latest[phase]
		if previous == nil || run.Status.CompletedAt.After(previous.Status.CompletedAt.Time) || (run.Status.CompletedAt.Equal(previous.Status.CompletedAt) && run.Name > previous.Name) {
			summary.latest[phase] = run
		}
	}
	for test, summary := range summaries {
		for _, phase := range runMetricPhases {
			ch <- prometheus.MustNewConstMetric(m.runs, prometheus.GaugeValue, summary.counts[phase], test, phase)
		}
		ch <- prometheus.MustNewConstMetric(m.cleanup, prometheus.GaugeValue, summary.cleanup, test)
		if !summary.oldestPending.IsZero() {
			ch <- prometheus.MustNewConstMetric(m.oldestPending, prometheus.GaugeValue, float64(summary.oldestPending.Unix()), test)
		}
		for phase, run := range summary.latest {
			ch <- prometheus.MustNewConstMetric(m.lastCompleted, prometheus.GaugeValue, float64(run.Status.CompletedAt.Unix()), test, phase)
			if run.Status.StartedAt != nil && !run.Status.StartedAt.IsZero() && !run.Status.CompletedAt.Before(run.Status.StartedAt) {
				duration := run.Status.CompletedAt.Sub(run.Status.StartedAt.Time).Seconds()
				ch <- prometheus.MustNewConstMetric(m.lastDuration, prometheus.GaugeValue, duration, test, phase)
			}
		}
	}
}
