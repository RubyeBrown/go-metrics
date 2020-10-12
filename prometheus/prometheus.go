// +build go1.9

package prometheus

import (
	"fmt"
	"log"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/armon/go-metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
)

var (
	// DefaultPrometheusOpts is the default set of options used when creating a
	// PrometheusSink.
	DefaultPrometheusOpts = PrometheusOpts{
		Expiration: 60 * time.Second,
	}
)

// PrometheusOpts is used to configure the Prometheus Sink
type PrometheusOpts struct {
	// Expiration is the duration a metric is valid for, after which it will be
	// untracked. If the value is zero, a metric is never expired.
	Expiration time.Duration
	Registerer prometheus.Registerer

	// Gauges, Summaries, and Counters allow us to pre-declare metrics by giving their Name and ConstLabels to the
	// PrometheusSink when it is created. Metrics declared in this way will be initialized at zero and will not be
	// deleted when their expiry is reached.
	// - Gauges and Summaries will be set to NaN when they expire.
	// - Counters continue to Collect their last known value.
	// Ex:
	// PrometheusOpts{
	//     Expiration: 10 * time.Second,
	//     Gauges: []PrometheusGauge{
	//         {
	//	         Name: []string{ "application", "component", "measurement"},
	//           ConstLabels: []metrics.Label{ { Name: "datacenter", Value: "dc1" }, },
	//         },
	//     },
	// }
	Gauges     []PrometheusGauge
	Summaries  []PrometheusSummary
	Counters   []PrometheusCounter
}

type PrometheusSink struct {
	// If these will ever be copied, they should be converted to *sync.Map values and initialized appropriately
	gauges     sync.Map
	summaries  sync.Map
	counters   sync.Map
	expiration time.Duration
}

type PrometheusGauge struct {
	Name []string
	ConstLabels []metrics.Label
	prometheus.Gauge
	updatedAt time.Time
	// canDelete is set if the metric is created during runtime so we know it's ephemeral and can delete it on expiry.
	canDelete bool
}

type PrometheusSummary struct {
	Name []string
	ConstLabels []metrics.Label
	prometheus.Summary
	updatedAt time.Time
	canDelete bool
}

type PrometheusCounter struct {
	Name []string
	ConstLabels []metrics.Label
	prometheus.Counter
	updatedAt time.Time
	canDelete bool
}

// NewPrometheusSink creates a new PrometheusSink using the default options.
func NewPrometheusSink() (*PrometheusSink, error) {
	return NewPrometheusSinkFrom(DefaultPrometheusOpts)
}

// NewPrometheusSinkFrom creates a new PrometheusSink using the passed options.
func NewPrometheusSinkFrom(opts PrometheusOpts) (*PrometheusSink, error) {
	sink := &PrometheusSink{
		gauges:     sync.Map{},
		summaries:  sync.Map{},
		counters:   sync.Map{},
		expiration: opts.Expiration,
	}

	initGauges(&sink.gauges, opts.Gauges)
	initSummaries(&sink.summaries, opts.Summaries)
	initCounters(&sink.counters, opts.Counters)

	reg := opts.Registerer
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	return sink, reg.Register(sink)
}

// Describe is needed to meet the Collector interface.
func (p *PrometheusSink) Describe(c chan<- *prometheus.Desc) {
	// We must emit some description otherwise an error is returned. This
	// description isn't shown to the user!
	prometheus.NewGauge(prometheus.GaugeOpts{Name: "Dummy", Help: "Dummy"}).Describe(c)
}

// Collect meets the collection interface and allows us to enforce our expiration
// logic to clean up ephemeral metrics if their value haven't been set for a
// duration exceeding our allowed expiration time.
func (p *PrometheusSink) Collect(c chan<- prometheus.Metric) {
	expire := p.expiration != 0
	now := time.Now()
	p.gauges.Range(func(k, v interface{}) bool {
		if v != nil {
			lastUpdate := v.(*PrometheusGauge).updatedAt
			if expire && lastUpdate.Add(p.expiration).Before(now) {
				if v.(*PrometheusGauge).canDelete {
					p.gauges.Delete(k)
					return true
				}
				// We have not observed the gauge this interval so we don't know its value.
				v.(*PrometheusGauge).Set(math.NaN())
			}
			v.(*PrometheusGauge).Collect(c)
		}
		return true
	})
	p.summaries.Range(func(k, v interface{}) bool {
		if v != nil {
			lastUpdate := v.(*PrometheusSummary).updatedAt
			if expire && lastUpdate.Add(p.expiration).Before(now) {
				if v.(*PrometheusSummary).canDelete {
					p.summaries.Delete(k)
					return true
				}
				// We have observed nothing in this interval.
				v.(*PrometheusSummary).Observe(math.NaN())
			}
			v.(*PrometheusSummary).Collect(c)
		}
		return true
	})
	p.counters.Range(func(k, v interface{}) bool {
		if v != nil {
			lastUpdate := v.(*PrometheusCounter).updatedAt
			if expire && lastUpdate.Add(p.expiration).Before(now) {
				if v.(PrometheusCounter).canDelete {
					p.counters.Delete(k)
					return true
				}
				// Counters remain at their previous value when not observed, so we do not set it to NaN.
			}
			v.(*PrometheusCounter).Collect(c)
		}
		return true
	})
}

func initGauges(m *sync.Map, gauges []PrometheusGauge) {
	for _, gauge := range gauges {
		key, hash := flattenKey(gauge.Name, gauge.ConstLabels)
		g := prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        key,
			Help:        key,
			ConstLabels: prometheusLabels(gauge.ConstLabels),
		})
		g.Set(float64(0)) // Initialize at zero
		gauge.Gauge = g
		m.Store(hash, &gauge)
	}
	return
}

func initSummaries(m *sync.Map, summaries []PrometheusSummary) {
	for _, summary := range summaries {
		key, hash := flattenKey(summary.Name, summary.ConstLabels)
		s := prometheus.NewSummary(prometheus.SummaryOpts{
			Name:        key,
			Help:        key,
			ConstLabels: prometheusLabels(summary.ConstLabels),
		})
		s.Observe(float64(0)) // Initialize at zero
		summary.Summary = s
		m.Store(hash, &summary)
	}
	return
}

func initCounters(m *sync.Map, counters []PrometheusCounter) {
	for _, counter := range counters {
		key, hash := flattenKey(counter.Name, counter.ConstLabels)
		c := prometheus.NewCounter(prometheus.CounterOpts{
			Name:        key,
			Help:        key,
			ConstLabels: prometheusLabels(counter.ConstLabels),
		})
		c.Add(float64(0)) // Initialize at zero
		counter.Counter = c
		m.Store(hash, &counter)
	}
	return
}

var forbiddenChars = regexp.MustCompile("[ .=\\-/]")

func flattenKey(parts []string, labels []metrics.Label) (string, string) {
	key := strings.Join(parts, "_")
	key = forbiddenChars.ReplaceAllString(key, "_")

	hash := key
	for _, label := range labels {
		hash += fmt.Sprintf(";%s=%s", label.Name, label.Value)
	}

	return key, hash
}

func prometheusLabels(labels []metrics.Label) prometheus.Labels {
	l := make(prometheus.Labels)
	for _, label := range labels {
		l[label.Name] = label.Value
	}
	return l
}

func (p *PrometheusSink) SetGauge(parts []string, val float32) {
	p.SetGaugeWithLabels(parts, val, nil)
}

func (p *PrometheusSink) SetGaugeWithLabels(parts []string, val float32, labels []metrics.Label) {
	key, hash := flattenKey(parts, labels)
	pg, ok := p.gauges.Load(hash)

	// The sync.Map underlying gauges stores pointers to our structs. If we need to make updates,
	// rather than modifying the underlying value directly, which would be racy, we make a local
	// copy by dereferencing the pointer we get back, making the appropriate changes, and then
	// storing a pointer to our local copy. The underlying Prometheus types are threadsafe,
	// so there's no issues there. It's possible for racy updates to occur to the updatedAt
	// value, but since we're always setting it to time.Now(), it doesn't really matter.
	if ok {
		localGauge := *pg.(*PrometheusGauge)
		localGauge.Set(float64(val))
		localGauge.updatedAt = time.Now()
		p.gauges.Store(hash, &localGauge)

	// The gauge does not exist, create the gauge and allow it to be deleted
	} else {
		g := prometheus.NewGauge(prometheus.GaugeOpts{
			Name:        key,
			Help:        key,
			ConstLabels: prometheusLabels(labels),
		})
		g.Set(float64(val))
		pg = &PrometheusGauge{
			Gauge: g,
			updatedAt: time.Now(),
			canDelete: true,
		}
		p.gauges.Store(hash, pg)
	}
}

func (p *PrometheusSink) AddSample(parts []string, val float32) {
	p.AddSampleWithLabels(parts, val, nil)
}

func (p *PrometheusSink) AddSampleWithLabels(parts []string, val float32, labels []metrics.Label) {
	key, hash := flattenKey(parts, labels)
	ps, ok := p.summaries.Load(hash)

	// Does the summary already exist for this sample type?
	if ok {
		localSummary := *ps.(*PrometheusSummary)
		localSummary.Observe(float64(val))
		localSummary.updatedAt = time.Now()
		p.summaries.Store(hash, &localSummary)

	// The summary does not exist, create the Summary and allow it to be deleted
	} else {
		s := prometheus.NewSummary(prometheus.SummaryOpts{
			Name:        key,
			Help:        key,
			MaxAge:      10 * time.Second,
			ConstLabels: prometheusLabels(labels),
			Objectives:  map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		})
		s.Observe(float64(val))
		ps = &PrometheusSummary{
			Summary: s,
			updatedAt: time.Now(),
			canDelete: true,
		}
		p.summaries.Store(hash, ps)
	}
}

// EmitKey is not implemented. Prometheus doesn’t offer a type for which an
// arbitrary number of values is retained, as Prometheus works with a pull
// model, rather than a push model.
func (p *PrometheusSink) EmitKey(key []string, val float32) {
}

func (p *PrometheusSink) IncrCounter(parts []string, val float32) {
	p.IncrCounterWithLabels(parts, val, nil)
}

func (p *PrometheusSink) IncrCounterWithLabels(parts []string, val float32, labels []metrics.Label) {
	key, hash := flattenKey(parts, labels)
	pc, ok := p.counters.Load(hash)

	// Does the counter exist?
	if ok {
		localCounter := *pc.(*PrometheusCounter)
		localCounter.Add(float64(val))
		localCounter.updatedAt = time.Now()
		p.counters.Store(hash, &localCounter)

	// The counter does not exist yet, create it and allow it to be deleted
	} else {
		c := prometheus.NewCounter(prometheus.CounterOpts{
			Name:        key,
			Help:        key,
			ConstLabels: prometheusLabels(labels),
		})
		c.Add(float64(val))
		pc = &PrometheusCounter{
			Counter: c,
			updatedAt: time.Now(),
			canDelete: true,
		}
		p.counters.Store(hash, pc)
	}
}

// PrometheusPushSink wraps a normal prometheus sink and provides an address and facilities to export it to an address
// on an interval.
type PrometheusPushSink struct {
	*PrometheusSink
	pusher       *push.Pusher
	address      string
	pushInterval time.Duration
	stopChan     chan struct{}
}

// NewPrometheusPushSink creates a PrometheusPushSink by taking an address, interval, and destination name.
func NewPrometheusPushSink(address string, pushInterval time.Duration, name string) (*PrometheusPushSink, error) {
	promSink := &PrometheusSink{
		gauges:     sync.Map{},
		summaries:  sync.Map{},
		counters:   sync.Map{},
		expiration: 60 * time.Second,
	}

	pusher := push.New(address, name).Collector(promSink)

	sink := &PrometheusPushSink{
		promSink,
		pusher,
		address,
		pushInterval,
		make(chan struct{}),
	}

	sink.flushMetrics()
	return sink, nil
}

func (s *PrometheusPushSink) flushMetrics() {
	ticker := time.NewTicker(s.pushInterval)

	go func() {
		for {
			select {
			case <-ticker.C:
				err := s.pusher.Push()
				if err != nil {
					log.Printf("[ERR] Error pushing to Prometheus! Err: %s", err)
				}
			case <-s.stopChan:
				ticker.Stop()
				return
			}
		}
	}()
}

func (s *PrometheusPushSink) Shutdown() {
	close(s.stopChan)
}
