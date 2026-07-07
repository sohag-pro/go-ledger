package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestHandlerServesMetrics exercises Handler(), which metrics_test.go never
// calls: it wires the shared registry into promhttp and serves it over HTTP.
// This drives a real request through it and checks the response actually
// contains the ledger's own metric names in Prometheus text exposition
// format, not just a 200 with an empty body.
func TestHandlerServesMetrics(t *testing.T) {
	// A distinct label value ties this scrape to this test, so the assertion
	// cannot pass merely because some other test happened to touch the metric.
	PostDuration.WithLabelValues("coverage-test-outcome").Observe(0.01)

	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL) //nolint:noctx,gosec // test server URL, not user input
	if err != nil {
		t.Fatalf("GET metrics endpoint: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(raw)

	for _, want := range []string{
		"transaction_post_duration_seconds",
		"transaction_post_serialization_retries_total",
		"coverage-test-outcome",
		"go_goroutines", // proves the Go runtime collector is wired in too
	} {
		if !strings.Contains(text, want) {
			t.Errorf("response missing %q; body:\n%s", want, text)
		}
	}
}

// TestCollectorsRecordObservableValues increments or observes each of the
// four domain collectors once against a captured baseline, then reads the
// value back from the shared registry, the same style
// TestRunInTxRetriesThenCommits in the postgres package uses via
// testutil.ToFloat64. It asserts the actual recorded value, not merely that
// the call did not panic.
func TestCollectorsRecordObservableValues(t *testing.T) {
	beforeReplays := testutil.ToFloat64(IdempotencyReplays)
	IdempotencyReplays.Inc()
	if got := testutil.ToFloat64(IdempotencyReplays); got != beforeReplays+1 {
		t.Errorf("IdempotencyReplays = %v, want %v", got, beforeReplays+1)
	}

	beforeConflicts := testutil.ToFloat64(IdempotencyConflicts)
	IdempotencyConflicts.Inc()
	if got := testutil.ToFloat64(IdempotencyConflicts); got != beforeConflicts+1 {
		t.Errorf("IdempotencyConflicts = %v, want %v", got, beforeConflicts+1)
	}

	beforeRetries := testutil.ToFloat64(SerializationRetries)
	SerializationRetries.Inc()
	if got := testutil.ToFloat64(SerializationRetries); got != beforeRetries+1 {
		t.Errorf("SerializationRetries = %v, want %v", got, beforeRetries+1)
	}

	const outcome = "collector-value-test"
	PostDuration.WithLabelValues(outcome).Observe(0.005)
	count, sum := histogramSample(t, outcome)
	if count != 1 {
		t.Errorf("PostDuration count for outcome %q = %d, want 1", outcome, count)
	}
	if sum <= 0 {
		t.Errorf("PostDuration sum for outcome %q = %v, want > 0", outcome, sum)
	}
}

// histogramSample gathers the shared registry and returns the sample count and
// sum for PostDuration's series labeled with the given outcome.
func histogramSample(t *testing.T, outcome string) (count uint64, sum float64) {
	t.Helper()
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "transaction_post_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelValue(m, "outcome") == outcome {
				h := m.GetHistogram()
				return h.GetSampleCount(), h.GetSampleSum()
			}
		}
	}
	t.Fatalf("no histogram series found for outcome %q", outcome)
	return 0, 0
}

// labelValue returns the value of the named label on m, or "" if absent.
func labelValue(m *dto.Metric, name string) string {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}
