package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/antonlindstrom/mesos_stats"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/log"
)

const concurrentFetch = 100

// Commandline flags.
var (
	addr                 = flag.String("web.listen-address", ":9105", "Address to listen on for web interface and telemetry")
	autoDiscoverInterval = flag.Duration("exporter.discover-interval", 10*time.Minute, "Interval at which to update available slaves from a Mesos Master. Only used if exporter.scrape-mode=discover.")
	queryURL             = flag.String("exporter.url", "", "The URL of a Mesos Slave, if mode=slave. The URL of a Mesos Master, if mode=discover or mode=master.")
	metricsPath          = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics")
	scrapeInterval       = flag.Duration("exporter.interval", 10*time.Second, "Scrape interval duration")
	scrapeMode           = flag.String("exporter.scrape-mode", "", "The mode in which to run the exporter: 'discover', 'master' or 'slave'.")
)

var (
	variableLabels = []string{"task", "slave", "framework_id", "framework_name", "task_name"}
	variableMesosUpLabels = []string{"host"}
	cpuLimitDesc = prometheus.NewDesc(
		"mesos_task_cpu_limit",
		"Fractional CPU limit.",
		variableLabels, nil,
	)
	cpuSysDesc = prometheus.NewDesc(
		"mesos_task_cpu_system_seconds_total",
		"Cumulative system CPU time in seconds.",
		variableLabels, nil,
	)
	cpuUsrDesc = prometheus.NewDesc(
		"mesos_task_cpu_user_seconds_total",
		"Cumulative user CPU time in seconds.",
		variableLabels, nil,
	)
	memLimitDesc = prometheus.NewDesc(
		"mesos_task_memory_limit_bytes",
		"Task memory limit in bytes.",
		variableLabels, nil,
	)
	memRssDesc = prometheus.NewDesc(
		"mesos_task_memory_rss_bytes",
		"Task memory RSS usage in bytes.",
		variableLabels, nil,
	)
	
	MesosUp = prometheus.NewDesc(
		"mesos_up",
		"Mesos state",
		variableMesosUpLabels, nil,
	)
	frameworkLabels = []string{"id", "name"}

	frameworkResourcesUsedCPUs = prometheus.NewDesc(
		"mesos_framework_resources_used_cpus",
		"Fractional CPUs used by a framework.",
		frameworkLabels, nil,
	)

	frameworkResourcesUsedDisk = prometheus.NewDesc(
		"mesos_framework_resources_used_disk_bytes",
		"Disk space used by a framework.",
		frameworkLabels, nil,
	)

	frameworkResourcesUsedMemory = prometheus.NewDesc(
		"mesos_framework_resources_used_memory_bytes",
		"Memory used by a framework.",
		frameworkLabels, nil,
	)

	masterMetricsLabels = []string{"host"}

	masterMetrics = []snapshotMetric{
		snapshotMetric{
			desc: prometheus.NewDesc(
				"mesos_master_tasks_error_total",
				"Number of tasks that have errored.",
				masterMetricsLabels, nil,
			),
			snapshotKey: "master/tasks_error",
			valueType:   prometheus.CounterValue,
		},
		snapshotMetric{
			desc: prometheus.NewDesc(
				"mesos_master_tasks_failed_total",
				"Number of tasks that have failed.",
				masterMetricsLabels, nil,
			),
			snapshotKey: "master/tasks_failed",
			valueType:   prometheus.CounterValue,
		},
		snapshotMetric{
			desc: prometheus.NewDesc(
				"mesos_master_tasks_finished_total",
				"Number of tasks that have finished.",
				masterMetricsLabels, nil,
			),
			snapshotKey: "master/tasks_finished",
			valueType:   prometheus.CounterValue,
		},
		snapshotMetric{
			desc: prometheus.NewDesc(
				"mesos_master_tasks_killed_total",
				"Number of tasks that got killed.",
				masterMetricsLabels, nil,
			),
			snapshotKey: "master/tasks_killed",
			valueType:   prometheus.CounterValue,
		},
		snapshotMetric{
			desc: prometheus.NewDesc(
				"mesos_master_tasks_lost_total",
				"Number of tasks that got lost.",
				masterMetricsLabels, nil,
			),
			snapshotKey: "master/tasks_lost",
			valueType:   prometheus.CounterValue,
		},
		snapshotMetric{
			desc: prometheus.NewDesc(
				"mesos_master_tasks_running",
				"Number of tasks that are running.",
				masterMetricsLabels, nil,
			),
			snapshotKey: "master/tasks_running",
			valueType:   prometheus.GaugeValue,
		},
		snapshotMetric{
			desc: prometheus.NewDesc(
				"mesos_master_tasks_staging",
				"Number of tasks that are staging.",
				masterMetricsLabels, nil,
			),
			snapshotKey: "master/tasks_staging",
			valueType:   prometheus.GaugeValue,
		},
		snapshotMetric{
			desc: prometheus.NewDesc(
				"mesos_master_tasks_starting",
				"Number of tasks that are starting.",
				masterMetricsLabels, nil,
			),
			snapshotKey: "master/tasks_starting",
			valueType:   prometheus.GaugeValue,
		},
	}

	slaveMetricsLabels = []string{"host"}

	slaveMetrics = []snapshotMetric{
		snapshotMetric{
			desc: prometheus.NewDesc(
				"mesos_slave_cpus",
				"CPUs advertised by a Mesos Slave.",
				slaveMetricsLabels, nil,
			),
			snapshotKey: "slave/cpus_total",
			valueType:   prometheus.GaugeValue,
		},
		snapshotMetric{
			convertFn: megabytesToBytes,
			desc: prometheus.NewDesc(
				"mesos_slave_disk_bytes",
				"Disk space advertised by a Mesos Slave.",
				slaveMetricsLabels, nil,
			),
			snapshotKey: "slave/disk_total",
			valueType:   prometheus.GaugeValue,
		},
		snapshotMetric{
			convertFn: megabytesToBytes,
			desc: prometheus.NewDesc(
				"mesos_slave_memory_bytes",
				"Memory advertised by a Mesos Slave.",
				slaveMetricsLabels, nil,
			),
			snapshotKey: "slave/mem_total",
			valueType:   prometheus.GaugeValue,
		},
	}
)

var httpClient = http.Client{
	Timeout: 5 * time.Second,
}

type exporterOpts struct {
	autoDiscoverInterval time.Duration
	interval             time.Duration
	queryURL             string
}

type exporterTaskInfo struct {
	FrameworkName string
	TaskName      string
}

type framework struct {
	ID            string
	Name          string
	UsedResources *resources `json:"used_resources"`
}

type masterState struct {
	Frameworks []*framework
	Hostname   string
	Slaves     []*slave
}

type snapshotMetric struct {
	convertFn   func(float64) float64
	desc        *prometheus.Desc
	snapshotKey string
	valueType   prometheus.ValueType
}

type metricsSnapshot map[string]float64

type periodicExporter struct {
	sync.RWMutex
	errors   *prometheus.CounterVec
	metrics  []prometheus.Metric
	opts     *exporterOpts
	queryURL *url.URL
	slaves   struct {
		sync.Mutex
		urls []string
	}
}

type resources struct {
	CPUs float64
	Disk float64
	Mem  float64
}

type slave struct {
	Active   bool
	Hostname string
	PID      string
}

type slaveState struct {
	Frameworks []*slaveStateFramework
}

type slaveStateExecutor struct {
	Tasks []*slaveStateTask
}

type slaveStateFramework struct {
	Name      string
	Executors []*slaveStateExecutor
}

type slaveStateTask struct {
	FrameworkID string `json:"framework_id"`
	ID          string
	Name        string
}

func newMesosExporter(opts *exporterOpts) *periodicExporter {
	e := &periodicExporter{
		errors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "mesos_exporter",
				Name:      "slave_scrape_errors_total",
				Help:      "Current total scrape errors",
			},
			[]string{"slave"},
		),
		opts: opts,
	}

	if opts.queryURL == "" {
		log.Fatal("Flag '-exporter.url' not set")
	}

	log.Info("starting mesos_exporter in scrape mode 'master'")
	e.queryURL = parseMasterURL(opts.queryURL, e)

	return e
}

func (e *periodicExporter) Describe(ch chan<- *prometheus.Desc) {
	e.scrapeMaster()
	e.rLockMetrics(func() {
		for _, m := range e.metrics {
			ch <- m.Desc()
		}
	})
	e.errors.MetricVec.Describe(ch)
}

func (e *periodicExporter) Collect(ch chan<- prometheus.Metric) {
	e.scrapeMaster()
	e.rLockMetrics(func() {
		for _, m := range e.metrics {
			ch <- m
		}
	})
	e.errors.MetricVec.Collect(ch)
}

func (e *periodicExporter) fetch(urlChan <-chan string, metricsChan chan<- prometheus.Metric, wg *sync.WaitGroup) {
	defer wg.Done()

	for u := range urlChan {
		u, err := url.Parse(u)
		if err != nil {
			log.Warn("could not parse slave URL: ", err)
			continue
		}

		host, _, err := net.SplitHostPort(u.Host)
		if err != nil {
			log.Warn("could not parse network address: ", err)
			continue
		}

		taskInfo := map[string]exporterTaskInfo{}
		var state slaveState
		stateURL := fmt.Sprintf("%s/state", u)

		err = getJSON(&state, stateURL)
		if err != nil {
			log.Warn(err)
			e.errors.WithLabelValues(host).Inc()
			continue
		}

		for _, fw := range state.Frameworks {
			for _, ex := range fw.Executors {
				for _, t := range ex.Tasks {
					taskInfo[t.ID] = exporterTaskInfo{fw.Name, t.Name}
				}
			}
		}

		monitorURL := fmt.Sprintf("%s/monitor/statistics", u)
		var stats []mesos_stats.Monitor

		err = getJSON(&stats, monitorURL)
		if err != nil {
			log.Warn(err)
			e.errors.WithLabelValues(host).Inc()
			continue
		}

		for _, stat := range stats {
			tinfo, ok := taskInfo[stat.Source]
			if !ok {
				continue
			}
			metricsChan <- prometheus.MustNewConstMetric(
				cpuLimitDesc,
				prometheus.GaugeValue,
				float64(stat.Statistics.CpusLimit),
				stat.Source, host, stat.FrameworkId, tinfo.FrameworkName, tinfo.TaskName,
			)
			metricsChan <- prometheus.MustNewConstMetric(
				cpuSysDesc,
				prometheus.CounterValue,
				float64(stat.Statistics.CpusSystemTimeSecs),
				stat.Source, host, stat.FrameworkId, tinfo.FrameworkName, tinfo.TaskName,
			)
			metricsChan <- prometheus.MustNewConstMetric(
				cpuUsrDesc,
				prometheus.CounterValue,
				float64(stat.Statistics.CpusUserTimeSecs),
				stat.Source, host, stat.FrameworkId, tinfo.FrameworkName, tinfo.TaskName,
			)
			metricsChan <- prometheus.MustNewConstMetric(
				memLimitDesc,
				prometheus.GaugeValue,
				float64(stat.Statistics.MemLimitBytes),
				stat.Source, host, stat.FrameworkId, tinfo.FrameworkName, tinfo.TaskName,
			)
			metricsChan <- prometheus.MustNewConstMetric(
				memRssDesc,
				prometheus.GaugeValue,
				float64(stat.Statistics.MemRssBytes),
				stat.Source, host, stat.FrameworkId, tinfo.FrameworkName, tinfo.TaskName,
			)
		}

		metricsSnapshotURL := fmt.Sprintf("%s/metrics/snapshot", u)
		var ms metricsSnapshot

		err = getJSON(&ms, metricsSnapshotURL)
		if err != nil {
			log.Warn(err)
			e.errors.WithLabelValues(host).Inc()
			continue
		}

		for _, mm := range slaveMetrics {
			metricValue, ok := ms[mm.snapshotKey]
			if !ok {
				continue
			}

			if mm.convertFn != nil {
				metricValue = mm.convertFn(metricValue)
			}

			metricsChan <- prometheus.MustNewConstMetric(
				mm.desc, mm.valueType, metricValue, host,
			)
		}

	}
}

func (e *periodicExporter) rLockMetrics(f func()) {
	e.RLock()
	defer e.RUnlock()
	f()
}

func (e *periodicExporter) setMetrics(ch chan prometheus.Metric) {
	metrics := []prometheus.Metric{}
	for metric := range ch {
		metrics = append(metrics, metric)
	}

	e.Lock()
	e.metrics = metrics
	e.Unlock()
}

func (e *periodicExporter) scrapeMaster() {
	stateURL := fmt.Sprintf("%s://%s/master/state", e.queryURL.Scheme, e.queryURL.Host)
	up := float64(1)
	response, err := http.Get(stateURL)
    if err != nil {
        up = 0
    }
	if response != nil {} 
	log.Debugf("Scraping master at %s", stateURL)

	var state masterState

	err = getJSON(&state, stateURL)
	metrics := []prometheus.Metric{}
	if err != nil {
			e.metrics = append(e.metrics, prometheus.MustNewConstMetric(
			MesosUp,
			prometheus.GaugeValue,
			up, 
			" ",
	))
		log.Warn(err)
		return
	}


	for _, fw := range state.Frameworks {
		metrics = append(metrics, prometheus.MustNewConstMetric(
			frameworkResourcesUsedCPUs,
			prometheus.GaugeValue,
			fw.UsedResources.CPUs,
			fw.ID, fw.Name,
		))
		metrics = append(metrics, prometheus.MustNewConstMetric(
			frameworkResourcesUsedDisk,
			prometheus.GaugeValue,
			megabytesToBytes(fw.UsedResources.Disk),
			fw.ID, fw.Name,
		))
		metrics = append(metrics, prometheus.MustNewConstMetric(
			frameworkResourcesUsedMemory,
			prometheus.GaugeValue,
			megabytesToBytes(fw.UsedResources.Mem),
			fw.ID, fw.Name,
		))
	}

	snapshotURL := fmt.Sprintf("%s://%s/metrics/snapshot", e.queryURL.Scheme, e.queryURL.Host)

	var ms metricsSnapshot

	err = getJSON(&ms, snapshotURL)
	if err != nil {
		e.metrics = append(e.metrics, prometheus.MustNewConstMetric(
		MesosUp,
		prometheus.GaugeValue,
		up, 
		" ",
			))
		log.Warn(err)
		return
	}
	for _, mm := range masterMetrics {
		metricValue, ok := ms[mm.snapshotKey]
		if !ok {
			e.metrics = append(e.metrics, prometheus.MustNewConstMetric(
			MesosUp,
			prometheus.GaugeValue,
			up, 
			" ",
				))
			continue
		}

		if mm.convertFn != nil {
			metricValue = mm.convertFn(metricValue)
		}

		metrics = append(metrics, prometheus.MustNewConstMetric(
			mm.desc, mm.valueType, metricValue, state.Hostname,
		))
	}
	metrics = append(metrics,  prometheus.MustNewConstMetric(
			MesosUp,
			prometheus.GaugeValue,
			up, 
			" ",
			))
	e.Lock()
	e.metrics = metrics
	e.Unlock()
}

func getJSON(data interface{}, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(data); err != nil {
		return err
	}

	return nil
}

func megabytesToBytes(v float64) float64 { return v * 1024 * 1024 }

func parseMasterURL(masterURL string, e *periodicExporter) *url.URL {
	
	parsedMasterURL, err := url.Parse(masterURL)
	if err != nil {
		log.Fatalf("unable to parse master URL '%s': ", masterURL, err)
	}
	if strings.HasPrefix(parsedMasterURL.Scheme, "http") == false {
		log.Fatalf("invalid scheme '%s' in master url - use 'http' or 'https'", parsedMasterURL.Scheme)
	}
	return parsedMasterURL
}

func runEvery(f func(), interval time.Duration) {
	for _ = range time.NewTicker(interval).C {
		f()
	}
}

func main() {
	flag.Parse()

	opts := &exporterOpts{
		autoDiscoverInterval: *autoDiscoverInterval,
		interval:             *scrapeInterval,
		queryURL:             strings.TrimRight(*queryURL, "/"),
	}
	exporter := newMesosExporter(opts)
	prometheus.MustRegister(exporter)

	http.Handle(*metricsPath, prometheus.Handler())
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "OK")
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, *metricsPath, http.StatusMovedPermanently)
	})

	log.Info("starting mesos_exporter on ", *addr)

	log.Fatal(http.ListenAndServe(*addr, nil))
}
