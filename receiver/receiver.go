//
// Copyright 2016 Gregory Trubetskoy. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package receiver manages the receiving end of the data. All of the
// queueing, caching, perioding flushing and cluster forwarding logic
// is here.
package receiver

import (
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tgres/tgres/aggregator"
	"github.com/tgres/tgres/cluster"
	"github.com/tgres/tgres/serde"
)

var debug bool

func init() {
	debug = os.Getenv("TGRES_RCVR_DEBUG") != ""
}

// Receiver receives and directs incoming datapoints to one of n
// workers, which is done to provide some parallelism, especially when
// it comes to flushing data to the database. The job of the workers
// is to update and maintain an in-memory RRD, and the job of the
// flushers is to persist the data to a database. The Receiver
// orchestrates this flow, providing a caching layer which reduces the
// database I/O.
//
// The Receiver is cluster-aware. In a clustered set up points are
// forwarded to the node responsible for a particular DS.
//
// The Receiver also creates an Aggregator which can aggregate metrics
// and send as aggregated data points periodically. In a clustered set
// up there is one Aggregator per cluster. Default aggregation period
// is 10 seconds.
//
// Receiver also handles paced metrics. A paced metric is a metric
// that can come in at a very fast rate (e.g. counting function calls
// within a process). Paced metrics are similar to aggregator metrics,
// but in a clustered set up they are accumulated locally in the
// process, and then sent to the aggregator (counter) or to the
// receiver (gauge), at which point they may end up getting forwarded
// to the appropriate node for handling. By default metrics are paced
// to be send once per second.
type Receiver struct {
	NWorkers int // number of workers, must be > 0

	// Cache parameters. These are tracked per Data Source.
	// MinCacheDuration means data points will always be kept in the cache at least this long,
	// or, in other words, the DS will not be flushed more frequently than every MinCacheDuration
	MinCacheDuration time.Duration
	// MaxMaxCacheDuration is the most time data points will be kept
	// in the cache, or, in other words, the DS will be flushed at
	// least every MaxFlushDuration provided it has data points. This
	// parameter mostly matters when the data stopped coming in and
	// some data points are still cached in memory.
	MaxCacheDuration time.Duration
	// MaxCachedPoints is the maximum number of cached points (as
	// returned by DS.PointCoont(), which is the sum of all RRAs) for
	// a Data Source. Note that MinCacheDuration trumps this
	// parameter. This number is only relevant if it is below the
	// total possible number of points in a MaxCacheDuration.
	MaxCachedPoints int

	// MaxFlushRatePerSecond controls how frequently we write to the
	// database across all DSs. This trumps all other caching parameters.
	MaxFlushRatePerSecond int

	StatFlushDuration time.Duration // Period after which stats are flushed
	StatsNamePrefix   string        // Stat names are prefixed with this

	ReportStats       bool   // report internal stats?
	ReportStatsPrefix string // prefix for internal stats

	// unexported internal stuff

	cluster clusterer   // cluster or nil
	serde   serde.SerDe // the database, required
	dsc     *dsCache    // the DS cache

	flusher       dsFlusherBlocking        // orchestration of flush queues
	dpCh          chan *incomingDP         // incoming data points
	workerChs     workerChannels           // incoming data points with ds
	aggCh         chan *aggregator.Command // aggregator commands (for statsd type stuff)
	pacedMetricCh chan *pacedMetric        // paced metrics (only flushed periodically)

	workerWg      sync.WaitGroup
	flusherWg     sync.WaitGroup
	aggWg         sync.WaitGroup
	directorWg    sync.WaitGroup
	pacedMetricWg sync.WaitGroup

	stopped bool
}

// incomingDP is incoming data (aka observation, measurement or
// sample). This is not the internal representation of a data point,
// it's the format in which points are expected to arrive and is easy
// to create from most any data point representation out there. This
// data point representation has no notion of duration and therefore
// must rely on some kind of a separately stored "last update" time.
type incomingDP struct {
	Ident     serde.Ident
	TimeStamp time.Time
	Value     float64
	Hops      int
}

// Create a Receiver. The first argument is a SerDe, the second is a
// MatchingDSSpecFinder used to match previously unknown DS names to a
// DSSpec with which the DS is to be created. If you pass nil, then
// the default SimpleDSFinder is used which always returns DftDSSPec.
func New(serde serde.SerDe, finder MatchingDSSpecFinder) *Receiver {
	if finder == nil {
		finder = &SimpleDSFinder{DftDSSPec}
	}
	r := &Receiver{
		serde:                 serde,
		NWorkers:              4,
		MaxCacheDuration:      5 * time.Second,
		MinCacheDuration:      1 * time.Second,
		MaxCachedPoints:       256,
		MaxFlushRatePerSecond: 100,
		StatFlushDuration:     10 * time.Second,
		StatsNamePrefix:       "stats",
		dpCh:                  make(chan *incomingDP, 65536), // to be on the safe side
		aggCh:                 make(chan *aggregator.Command, 1024),
		pacedMetricCh:         make(chan *pacedMetric, 1024),
		ReportStats:           false,
		ReportStatsPrefix:     "tgres",
	}

	r.flusher = &dsFlusher{db: serde.Flusher(), sr: r}
	r.dsc = newDsCache(serde.Fetcher(), finder, r.flusher)
	return r
}

// Before using the receiver it must be Started. This starts all the
// worker and flusher goroutines, etc.
func (r *Receiver) Start() {
	doStart(r)
}

// Stops processing, waits for everything to finish and shuts down all
// workers/flushers.
func (r *Receiver) Stop() {
	r.stopped = true
	doStop(r, r.cluster)
}

// In a clustered set up informes other nodes that we are ready to
// handle data.
func (r *Receiver) ClusterReady(ready bool) {
	r.cluster.Ready(ready)
}

// Make the receiver clustered. It will also cause internal stats to
// be prefixed with the node address by setting ReportStatsPrefix.
func (r *Receiver) SetCluster(c clusterer) {
	r.cluster = c
	r.dsc.clstr = c
	ln := c.LocalNode()
	if ln != nil {
		// if this is a cluster, append the node address to the prefix
		addr := strings.Replace(ln.Addr.String(), ".", "_", -1)
		if r.ReportStatsPrefix != "" {
			r.ReportStatsPrefix += ("." + addr)
		} else {
			r.ReportStatsPrefix = addr
		}
	}
}

// Sends a data point to the receiver channel. A Data Source PDP
// always treats incoming data as a rate, it is the responsibility of
// the caller to present non-rate values such as counters as a
// rate. Consider using the Aggregator (QueueAggregatorCommand) or
// paced metrics (QueueSum/QueueGauge) for non-rate data.
func (r *Receiver) QueueDataPoint(ident serde.Ident, ts time.Time, v float64) {
	if !r.stopped {
		r.dpCh <- &incomingDP{Ident: ident, TimeStamp: ts, Value: v}
	}
}

// Sends a data point (in the form of an aggregator.Command) to the
// aggregator.
func (r *Receiver) QueueAggregatorCommand(agg *aggregator.Command) {
	if !r.stopped {
		r.aggCh <- agg
	}
}

// Send a counter/sum. This is a paced metric which will periodically
// be passed to the aggregator and from the aggregator to the data
// source as a rate.
func (r *Receiver) QueueSum(ident serde.Ident, v float64) {
	if !r.stopped {
		r.pacedMetricCh <- &pacedMetric{kind: pacedSum, ident: ident, value: v}
	}
}

// Send a gauge (i.e. a rate). This is a paced metric.
func (r *Receiver) QueueGauge(ident serde.Ident, v float64) {
	if !r.stopped {
		r.pacedMetricCh <- &pacedMetric{kind: pacedGauge, ident: ident, value: v}
	}
}

// Reporting internal to Tgres: count
func (r *Receiver) reportStatCount(name string, f float64) {
	if r != nil && r.ReportStats && f != 0 {
		r.QueueSum(serde.Ident{"name": r.ReportStatsPrefix + "." + name}, f)
	}
}

// Reporting internal to Tgres: gauge
func (r *Receiver) reportStatGauge(name string, f float64) {
	if r != nil && r.ReportStats {
		r.QueueGauge(serde.Ident{"name": r.ReportStatsPrefix + "." + name}, f)
	}
}

type dataPointQueuer interface {
	QueueDataPoint(serde.Ident, time.Time, float64)
}

type aggregatorCommandQueuer interface {
	QueueAggregatorCommand(*aggregator.Command)
}

type statReporter interface {
	reportStatCount(string, float64)
	reportStatGauge(string, float64)
}

type clusterer interface {
	RegisterMsgType() (chan *cluster.Msg, chan *cluster.Msg)
	NumMembers() int
	LoadDistData(func() ([]cluster.DistDatum, error)) error
	NodesForDistDatum(cluster.DistDatum) []*cluster.Node
	LocalNode() *cluster.Node
	NotifyClusterChanges() chan bool
	Transition(time.Duration) error
	Ready(bool) error
	Leave(timeout time.Duration) error
	Shutdown() error
	//NewMsg(*cluster.Node, interface{}) (*cluster.Msg, error)
}
