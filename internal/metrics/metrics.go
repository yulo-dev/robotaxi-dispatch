// Package metrics defines the Prometheus collectors and adapts them to the
// observer interfaces of the api, dispatch, and outbox packages.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const ns = "dispatch"

// Metrics holds every collector. Its methods implement the observer
// interfaces of the api, dispatch, and outbox packages.
type Metrics struct {
	Registry *prometheus.Registry

	ridesRequested, ridesAssigned, ridesFailed   prometheus.Counter
	rejections, conflicts, lockContention        prometheus.Counter
	outboxPublished, outboxErrors, replays       prometheus.Counter
	timeToAssign, batchSize, hungarian, astar    prometheus.Histogram
	httpDuration                                 *prometheus.HistogramVec
	outboxBacklog, availableVehicles, queueDepth prometheus.Gauge
}

// New registers all collectors on a fresh registry (plus Go and process
// collectors).
func New() *Metrics {
	r := prometheus.NewRegistry()
	counter := func(name, help string) prometheus.Counter {
		c := prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: name, Help: help})
		r.MustRegister(c)
		return c
	}
	hist := func(name, help string, buckets []float64) prometheus.Histogram {
		h := prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: ns, Name: name, Help: help, Buckets: buckets})
		r.MustRegister(h)
		return h
	}
	gauge := func(name, help string) prometheus.Gauge {
		g := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: name, Help: help})
		r.MustRegister(g)
		return g
	}
	m := &Metrics{
		Registry:        r,
		ridesRequested:  counter("rides_requested_total", "Rides created (idempotent replays excluded)."),
		ridesAssigned:   counter("rides_assigned_total", "Rides accepted by a vehicle."),
		ridesFailed:     counter("rides_failed_total", "Rides that found no vehicle before the match timeout."),
		rejections:      counter("dispatch_rejections_total", "Dispatches rejected by a vehicle or timed out."),
		conflicts:       counter("assignment_conflicts_total", "Dispatch commits refused by a version check or the active-ride unique index."),
		lockContention:  counter("lock_contention_total", "Ride or vehicle locks already held by another worker."),
		outboxPublished: counter("outbox_published_total", "Outbox rows handed to the dispatch queue."),
		outboxErrors:    counter("outbox_publish_errors_total", "Failed outbox publish rounds."),
		replays:         counter("idempotent_replays_total", "Ride requests answered from a stored idempotent response."),
		timeToAssign: hist("time_to_assign_seconds", "Ride request to vehicle accept.",
			[]float64{1, 2, 5, 10, 15, 20, 30, 45, 60, 90, 120}),
		batchSize: hist("batch_size", "Rides per dispatch batch.",
			[]float64{1, 2, 5, 10, 20, 50, 100, 200}),
		hungarian: hist("hungarian_solve_seconds", "Assignment solve time per batch.",
			prometheus.ExponentialBuckets(1e-5, 4, 10)),
		astar: hist("astar_route_seconds", "Uncached A* route computation time.",
			prometheus.ExponentialBuckets(1e-5, 4, 10)),
		outboxBacklog:     gauge("outbox_backlog", "Unpublished outbox rows."),
		availableVehicles: gauge("available_vehicles", "Vehicles in the Redis GEO index."),
		queueDepth:        gauge("dispatch_queue_depth", "Rides waiting in the in-process dispatch queue."),
	}
	m.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns, Name: "http_request_duration_seconds", Help: "HTTP request latency by route and status code.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "code"})
	r.MustRegister(m.httpDuration)
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// api.Observer

func (m *Metrics) RideRequested()    { m.ridesRequested.Inc() }
func (m *Metrics) IdempotentReplay() { m.replays.Inc() }
func (m *Metrics) HTTPRequest(route string, code int, d time.Duration) {
	m.httpDuration.WithLabelValues(route, strconv.Itoa(code)).Observe(d.Seconds())
}

// dispatch.Observer

func (m *Metrics) RideAssigned(d time.Duration) {
	m.ridesAssigned.Inc()
	m.timeToAssign.Observe(d.Seconds())
}
func (m *Metrics) RideFailed(n int)               { m.ridesFailed.Add(float64(n)) }
func (m *Metrics) Rejection()                     { m.rejections.Inc() }
func (m *Metrics) AssignmentConflict()            { m.conflicts.Inc() }
func (m *Metrics) LockContention()                { m.lockContention.Inc() }
func (m *Metrics) BatchSize(n int)                { m.batchSize.Observe(float64(n)) }
func (m *Metrics) HungarianSolve(d time.Duration) { m.hungarian.Observe(d.Seconds()) }
func (m *Metrics) QueueDepth(n int)               { m.queueDepth.Set(float64(n)) }

// outbox.Observer

func (m *Metrics) Published(n int) { m.outboxPublished.Add(float64(n)) }
func (m *Metrics) PublishError()   { m.outboxErrors.Inc() }
func (m *Metrics) Backlog(n int)   { m.outboxBacklog.Set(float64(n)) }

// AStar records an uncached route computation (geo.Router.OnRoute).
func (m *Metrics) AStar(d time.Duration) { m.astar.Observe(d.Seconds()) }

// AvailableVehicles sets the size of the GEO index.
func (m *Metrics) AvailableVehicles(n int64) { m.availableVehicles.Set(float64(n)) }
