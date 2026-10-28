package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yulo-dev/robotaxi-dispatch/internal/api"
	"github.com/yulo-dev/robotaxi-dispatch/internal/dispatch"
	"github.com/yulo-dev/robotaxi-dispatch/internal/outbox"
)

var (
	_ api.Observer      = (*Metrics)(nil)
	_ dispatch.Observer = (*Metrics)(nil)
	_ outbox.Observer   = (*Metrics)(nil)
)

// Every metric named in the design must be exported.
var required = []string{
	"dispatch_rides_requested_total",
	"dispatch_rides_assigned_total",
	"dispatch_rides_failed_total",
	"dispatch_dispatch_rejections_total",
	"dispatch_assignment_conflicts_total",
	"dispatch_lock_contention_total",
	"dispatch_outbox_published_total",
	"dispatch_outbox_publish_errors_total",
	"dispatch_idempotent_replays_total",
	"dispatch_time_to_assign_seconds",
	"dispatch_batch_size",
	"dispatch_hungarian_solve_seconds",
	"dispatch_astar_route_seconds",
	"dispatch_http_request_duration_seconds",
	"dispatch_outbox_backlog",
	"dispatch_available_vehicles",
	"dispatch_dispatch_queue_depth",
}

func TestAllMetricsExposed(t *testing.T) {
	m := New()
	m.HTTPRequest("/v1/rides", 202, 3*time.Millisecond) // vec appears after first observation
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	text := string(body)
	for _, name := range required {
		if !strings.Contains(text, "# TYPE "+name+" ") {
			t.Errorf("missing metric %s", name)
		}
	}
	if !strings.Contains(text, `dispatch_http_request_duration_seconds_count{code="202",route="/v1/rides"} 1`) {
		t.Error("http histogram labels not recorded")
	}
}
