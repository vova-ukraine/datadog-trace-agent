package main

import (
	"sync"
	"sync/atomic"
	"time"

	log "github.com/cihub/seelog"

	"github.com/DataDog/datadog-trace-agent/config"
	"github.com/DataDog/datadog-trace-agent/filters"
	"github.com/DataDog/datadog-trace-agent/model"
	"github.com/DataDog/datadog-trace-agent/quantizer"
	"github.com/DataDog/datadog-trace-agent/sampler"
	"github.com/DataDog/datadog-trace-agent/watchdog"
)

const processStatsInterval = time.Minute
const languageHeaderKey = "X-Datadog-Reported-Languages"

type processedTrace struct {
	Trace     model.Trace
	Root      *model.Span
	Env       string
	Sublayers []model.SublayerValue
}

func (pt *processedTrace) weight() float64 {
	if pt.Root == nil {
		return 1.0
	}
	return pt.Root.Weight()
}

// Agent struct holds all the sub-routines structs and make the data flow between them
type Agent struct {
	Receiver        *HTTPReceiver
	Concentrator    *Concentrator
	Filters         []filters.Filter
	ScoreSampler    *Sampler
	PrioritySampler *Sampler
	Writer          *Writer

	// config
	conf *config.AgentConfig

	// Used to synchronize on a clean exit
	exit chan struct{}

	die func(format string, args ...interface{})
}

// NewAgent returns a new Agent object, ready to be started
func NewAgent(conf *config.AgentConfig) *Agent {
	exit := make(chan struct{})

	rates := sampler.NewRateByService(conf.PrioritySamplerTimeout)

	r := NewHTTPReceiver(conf, rates)
	c := NewConcentrator(
		conf.ExtraAggregators,
		conf.BucketInterval.Nanoseconds(),
	)
	f := filters.Setup(conf)
	ss := NewScoreSampler(conf)
	ps := NewPrioritySampler(conf, rates)

	w := NewWriter(conf)
	w.inServices = r.services

	return &Agent{
		Receiver:        r,
		Concentrator:    c,
		Filters:         f,
		ScoreSampler:    ss,
		PrioritySampler: ps,
		Writer:          w,
		conf:            conf,
		exit:            exit,
		die:             die,
	}
}

// Run starts routers routines and individual pieces then stop them when the exit order is received
func (a *Agent) Run() {
	flushTicker := time.NewTicker(a.conf.BucketInterval)
	defer flushTicker.Stop()

	// it's really important to use a ticker for this, and with a not too short
	// interval, for this is our garantee that the process won't start and kill
	// itself too fast (nightmare loop)
	watchdogTicker := time.NewTicker(a.conf.WatchdogInterval)
	defer watchdogTicker.Stop()

	// update the data served by expvar so that we don't expose a 0 sample rate
	updatePreSampler(*a.Receiver.preSampler.Stats())

	a.Receiver.Run()
	a.Writer.Run()
	a.ScoreSampler.Run()
	a.PrioritySampler.Run()

	for {
		select {
		case t := <-a.Receiver.traces:
			a.Process(t)
		case t := <-a.Receiver.distributedTraces:
			a.ProcessDistributed(t)
		case <-flushTicker.C:
			p := model.AgentPayload{
				HostName: a.conf.HostName,
				Env:      a.conf.DefaultEnv,
			}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer watchdog.LogOnPanic()
				p.Stats = a.Concentrator.Flush()
				wg.Done()
			}()
			go func() {
				defer watchdog.LogOnPanic()
				// Serializing both flushes, classic agent sampler and distributed sampler,
				// in most cases only one will be used, so in mainstream case there should
				// be no performance issue, only in transitionnal mode can both contain data.
				p.Traces = a.ScoreSampler.Flush()
				p.Traces = append(p.Traces, a.PrioritySampler.Flush()...)
				wg.Done()
			}()

			wg.Wait()
			p.SetExtra(languageHeaderKey, a.Receiver.Languages())

			a.Writer.inPayloads <- p
		case <-watchdogTicker.C:
			a.watchdog()
		case <-a.exit:
			log.Info("exiting")
			close(a.Receiver.exit)
			a.Writer.Stop()
			a.ScoreSampler.Stop()
			a.PrioritySampler.Stop()
			return
		}
	}
}

func (a *Agent) processWithSampler(t model.Trace, s *Sampler) {
	if len(t) == 0 {
		// XXX Should never happen since we reject empty traces during
		// normalization.
		log.Debugf("skipping received empty trace")
		return
	}

	root := t.GetRoot()
	if root.End() < model.Now()-2*a.conf.BucketInterval.Nanoseconds() {
		log.Errorf("skipping trace with root too far in past, root:%v", *root)

		// We get the address of the struct holding the stats associated to the tags
		ts := a.Receiver.stats.getTagStats(Tags{})

		atomic.AddInt64(&ts.TracesDropped, 1)
		atomic.AddInt64(&ts.SpansDropped, int64(len(t)))
		return
	}

	for _, f := range a.Filters {
		if f.Keep(root) {
			continue
		}

		log.Debugf("rejecting trace by filter: %T  %v", f, *root)
		ts := a.Receiver.stats.getTagStats(Tags{})
		atomic.AddInt64(&ts.TracesFiltered, 1)
		atomic.AddInt64(&ts.SpansFiltered, int64(len(t)))

		return
	}

	rate := sampler.GetTraceAppliedSampleRate(root)
	rate *= a.Receiver.preSampler.Rate()
	sampler.SetTraceAppliedSampleRate(root, rate)

	t.ComputeTopLevel()

	sublayers := model.ComputeSublayers(t)
	model.SetSublayersOnSpan(root, sublayers)

	for i := range t {
		t[i] = quantizer.Quantize(t[i])
	}

	pt := processedTrace{
		Trace:     t,
		Root:      root,
		Env:       a.conf.DefaultEnv,
		Sublayers: sublayers,
	}
	if tenv := t.GetEnv(); tenv != "" {
		pt.Env = tenv
	}

	// Need to do this computation before entering the concentrator
	// as they access the Metrics map, which is not thread safe.
	t.ComputeWeight(*root)
	t.ComputeTopLevel()
	go func() {
		defer watchdog.LogOnPanic()
		a.Concentrator.Add(pt)

	}()
	go func() {
		defer watchdog.LogOnPanic()
		s.Add(pt)
	}()
}

// Process is the default work unit that receives a trace, transforms it and
// passes it downstream
func (a *Agent) Process(t model.Trace) {
	a.processWithSampler(t, a.ScoreSampler)
}

// ProcessDistributed is the default work unit that receives a trace, transforms it and
// passes it downstream, this version for distributed traces
func (a *Agent) ProcessDistributed(t model.Trace) {
	a.processWithSampler(t, a.PrioritySampler)
}

func (a *Agent) watchdog() {
	var wi watchdog.Info
	wi.CPU = watchdog.CPU()
	wi.Mem = watchdog.Mem()
	wi.Net = watchdog.Net()

	if float64(wi.Mem.Alloc) > a.conf.MaxMemory && a.conf.MaxMemory > 0 {
		a.die("exceeded max memory (current=%d, max=%d)", wi.Mem.Alloc, int64(a.conf.MaxMemory))
	}
	if int(wi.Net.Connections) > a.conf.MaxConnections && a.conf.MaxConnections > 0 {
		a.die("exceeded max connections (current=%d, max=%d)", wi.Net.Connections, a.conf.MaxConnections)
	}

	updateWatchdogInfo(wi)

	// Adjust pre-sampling dynamically
	rate, err := sampler.CalcPreSampleRate(a.conf.MaxCPU, wi.CPU.UserAvg, a.Receiver.preSampler.RealRate())
	if rate > a.conf.PreSampleRate {
		rate = a.conf.PreSampleRate
	}
	if err != nil {
		log.Warnf("problem computing pre-sample rate: %v", err)
	}
	a.Receiver.preSampler.SetRate(rate)
	a.Receiver.preSampler.SetError(err)

	updatePreSampler(*a.Receiver.preSampler.Stats())
}
