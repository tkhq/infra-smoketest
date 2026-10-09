package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	api "github.com/tkhq/infra-smoketest/api/v1alpha1"
	"github.com/tkhq/infra-smoketest/internal/definition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type runMetricKey struct{ name, test, phase string }

func runMetricsRegistry(t *testing.T, reader client.Reader) *prometheus.Registry {
	t.Helper()
	registry := prometheus.NewPedanticRegistry()
	if err := registry.Register(NewRunMetrics(reader)); err != nil {
		t.Fatal(err)
	}
	return registry
}

func gatherRunMetrics(t *testing.T, registry *prometheus.Registry) map[runMetricKey]float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := map[runMetricKey]float64{}
	for _, family := range families {
		for _, metric := range family.Metric {
			key := runMetricKey{name: family.GetName()}
			for _, label := range metric.Label {
				switch label.GetName() {
				case "smoketest":
					key.test = label.GetValue()
				case "phase":
					key.phase = label.GetValue()
				default:
					t.Fatalf("unexpected metric label %q", label.GetName())
				}
			}
			if metric.Gauge == nil {
				t.Fatalf("expected a gauge: %v", metric)
			}
			values[key] = metric.Gauge.GetValue()
		}
	}
	return values
}

func expectRunMetric(t *testing.T, values map[runMetricKey]float64, name, test, phase string, want float64) {
	t.Helper()
	key := runMetricKey{name, test, phase}
	if got, ok := values[key]; !ok || got != want {
		t.Fatalf("metric %+v = %v (present=%v), want %v", key, got, ok, want)
	}
}

func metricRun(name, test, phase string, created time.Time) *api.SmokeTestRun {
	return &api.SmokeTestRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: definition.Namespace, CreationTimestamp: metav1.NewTime(created)},
		Spec:       api.SmokeTestRunSpec{TestRef: api.TestReference{Name: test}},
		Status:     api.SmokeTestRunStatus{Phase: phase},
	}
}

func TestRunMetricsFollowStoredLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	run := metricRun("run", "cert-manager", "", now)
	definitionObject := &api.SmokeTest{ObjectMeta: metav1.ObjectMeta{Name: "cert-manager", Namespace: definition.Namespace}}
	neverRun := &api.SmokeTest{ObjectMeta: metav1.ObjectMeta{Name: "never-run", Namespace: definition.Namespace}}
	foreign := metricRun("foreign", "foreign", "Running", now)
	foreign.Namespace = "other"
	r := fakeReconciler(run, definitionObject, neverRun, foreign)
	registry := runMetricsRegistry(t, r.Client)
	values := gatherRunMetrics(t, registry)
	expectRunMetric(t, values, "infra_smoketest_runs", "cert-manager", "Pending", 1)
	expectRunMetric(t, values, "infra_smoketest_runs", "never-run", "Pending", 0)
	expectRunMetric(t, values, "infra_smoketest_oldest_pending_run_timestamp_seconds", "cert-manager", "", float64(now.Unix()))
	for key := range values {
		if key.test == "foreign" {
			t.Fatal("exported another namespace's Run")
		}
	}
	if err := r.Get(ctx, key(run.Name), run); err != nil {
		t.Fatal(err)
	}
	save := func() {
		t.Helper()
		if err := r.Status().Update(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	run.Status.Phase = "Running"
	run.Status.StartedAt = ptr.To(metav1.NewTime(now.Add(time.Minute)))
	save()
	values = gatherRunMetrics(t, registry)
	expectRunMetric(t, values, "infra_smoketest_runs", "cert-manager", "Pending", 0)
	expectRunMetric(t, values, "infra_smoketest_runs", "cert-manager", "Running", 1)
	if _, ok := values[runMetricKey{"infra_smoketest_oldest_pending_run_timestamp_seconds", "cert-manager", ""}]; ok {
		t.Fatal("retained a stale pending timestamp")
	}
	run.Status.Phase = "CleaningUp"
	run.Status.CleanupStartedAt = ptr.To(metav1.NewTime(now.Add(2 * time.Minute)))
	save()
	values = gatherRunMetrics(t, registry)
	expectRunMetric(t, values, "infra_smoketest_cleanup_pending_runs", "cert-manager", "", 1)
	// A terminal phase/Complete condition alone must not hide stuck cleanup.
	run.Status.Phase = "Failed"
	condition(run, "Complete", true, "CleanupFailed", "cleanup stalled", now)
	save()
	values = gatherRunMetrics(t, registry)
	expectRunMetric(t, values, "infra_smoketest_runs", "cert-manager", "Failed", 1)
	expectRunMetric(t, values, "infra_smoketest_cleanup_pending_runs", "cert-manager", "", 1)
	if _, ok := values[runMetricKey{"infra_smoketest_last_run_completed_timestamp_seconds", "cert-manager", "Failed"}]; ok {
		t.Fatal("reported completion before cleanup finished")
	}
	run.Status.CompletedAt = ptr.To(metav1.NewTime(now.Add(8 * time.Minute)))
	condition(run, "CleanupComplete", true, "Cleaned", "", now)
	save()
	values = gatherRunMetrics(t, registry)
	expectRunMetric(t, values, "infra_smoketest_cleanup_pending_runs", "cert-manager", "", 0)
	expectRunMetric(t, values, "infra_smoketest_last_run_duration_seconds", "cert-manager", "Failed", 7*60)
	// Repeated scrapes and a fresh collector (controller restart) agree.
	if !reflect.DeepEqual(values, gatherRunMetrics(t, registry)) || !reflect.DeepEqual(values, gatherRunMetrics(t, runMetricsRegistry(t, r.Client))) {
		t.Fatal("scrape or restart changed metrics without a resource change")
	}
	if err := r.Delete(ctx, run); err != nil {
		t.Fatal(err)
	}
	values = gatherRunMetrics(t, registry)
	expectRunMetric(t, values, "infra_smoketest_runs", "cert-manager", "Failed", 0)
	if _, ok := values[runMetricKey{"infra_smoketest_last_run_duration_seconds", "cert-manager", "Failed"}]; ok {
		t.Fatal("retained duration after Run deletion")
	}
	if err := r.Delete(ctx, definitionObject); err != nil {
		t.Fatal(err)
	}
	for key := range gatherRunMetrics(t, registry) {
		if key.test == "cert-manager" {
			t.Fatal("retained labels after definition and Runs were deleted")
		}
	}
}

func TestRunMetricsAggregateRetainedRuns(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	old := metricRun("old", "storage", "Succeeded", now)
	newer := metricRun("new", "storage", "Succeeded", now)
	cancelled := metricRun("cancelled", "storage", "Cancelled", now)
	for i, run := range []*api.SmokeTestRun{old, newer, cancelled} {
		run.Status.CompletedAt = ptr.To(metav1.NewTime(now.Add(time.Duration(i+1) * time.Minute)))
		condition(run, "CleanupComplete", true, "Cleaned", "", now)
	}
	old.Status.StartedAt = ptr.To(metav1.NewTime(now))
	newer.Status.StartedAt = ptr.To(metav1.NewTime(now.Add(30 * time.Second)))
	r := fakeReconciler(old, newer, cancelled,
		metricRun("waiting-new", "storage", "Pending", now),
		metricRun("waiting-old", "storage", "Pending", now.Add(-time.Minute)),
		metricRun("unknown", "missing-definition", "unexpected-value", now),
	)
	registry := runMetricsRegistry(t, r.Client)
	values := gatherRunMetrics(t, registry)
	expectRunMetric(t, values, "infra_smoketest_runs", "storage", "Succeeded", 2)
	expectRunMetric(t, values, "infra_smoketest_runs", "storage", "Pending", 2)
	expectRunMetric(t, values, "infra_smoketest_runs", "missing-definition", "Unknown", 1)
	expectRunMetric(t, values, "infra_smoketest_last_run_completed_timestamp_seconds", "storage", "Succeeded", float64(newer.Status.CompletedAt.Unix()))
	expectRunMetric(t, values, "infra_smoketest_last_run_duration_seconds", "storage", "Succeeded", 90)
	expectRunMetric(t, values, "infra_smoketest_last_run_completed_timestamp_seconds", "storage", "Cancelled", float64(cancelled.Status.CompletedAt.Unix()))
	expectRunMetric(t, values, "infra_smoketest_oldest_pending_run_timestamp_seconds", "storage", "", float64(now.Add(-time.Minute).Unix()))
	if _, ok := values[runMetricKey{"infra_smoketest_last_run_duration_seconds", "storage", "Cancelled"}]; ok {
		t.Fatal("reported duration for a Run that never started")
	}
	// Exercise concurrent HTTP scrapes as well as the pedantic registry checks.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			response := httptest.NewRecorder()
			promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `infra_smoketest_runs{phase="Succeeded",smoketest="storage"} 2`) {
				t.Errorf("unexpected metrics response: %d %s", response.Code, response.Body.String())
			}
		})
	}
	wg.Wait()
}

type failingMetricsReader struct {
	client.Reader
	failRuns bool
}

func (r failingMetricsReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, isRuns := list.(*api.SmokeTestRunList); isRuns == r.failRuns {
		return errors.New("cache unavailable")
	}
	return r.Reader.List(ctx, list, opts...)
}

func TestRunMetricsFailScrapeOnListError(t *testing.T) {
	for _, failRuns := range []bool{false, true} {
		registry := runMetricsRegistry(t, failingMetricsReader{Reader: fakeReconciler().Client, failRuns: failRuns})
		response := httptest.NewRecorder()
		promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "cache unavailable") {
			t.Fatalf("list failure reported a successful scrape: %d %s", response.Code, response.Body.String())
		}
	}
}
