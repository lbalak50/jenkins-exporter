package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jamiealquiza/envy"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/simplesurance/jenkins-exporter/internal/cli"
	"github.com/simplesurance/jenkins-exporter/internal/jenkins"
	"github.com/simplesurance/jenkins-exporter/internal/prometheus"
	"github.com/simplesurance/jenkins-exporter/internal/store"
)

const (
	appName = "jenkins-exporter"
	version = "0.1"
)

const (
	blockedTimeMetricDescr  = "Time spent in the queue being blocked"
	buildAbleMetricDesc     = "Time spent in the queue while buildable"
	buildDurationMetricDesc = "Time from queuing to completion"
	executionTimeMetricDesc = "" // TODO: what is this metric
	waitingTimeMetricDesc   = "Time spent in the queue while waiting"
)

var logger *log.Logger

var (
	listenAddr = flag.String("listen-addr", ":8123", "Listening address of the metric HTTP server")

	stateFilePath = flag.String("state-file", appName+".state.json", "path to the state file")

	httpTimeout = flag.Uint64("http-timeout", 180, "Timeout for jenkins http requests (seconds)")

	jenkinsUsername     = flag.String("jenkins-user", "", "Jenkins API username")
	jenkinsPassword     = flag.String("jenkins-password", "", "Jenkins API password or token")
	jenkinsURL          = flag.String("jenkins-url", "", "URL to the Jenkins Server")
	jenkinsJobWhitelist = cli.StrMapFlag{}
	pollIntervalSec     = flag.Uint64("poll-interval", 30, "Interval in which data is fetched from jenkins (seconds)")

	prometheusNamespace = flag.String("prometheus-namespace", strings.ReplaceAll(appName, "-", "_"), "metric name prefix")

	recordBlockedTime      = flag.Bool("enable-blocked-time-metric", true, "record the blocked_time metric\n"+blockedTimeMetricDescr)
	recordBuildAbleTime    = flag.Bool("enable-buildable-time-metric", true, "record the buildable_time metric\n"+buildAbleMetricDesc)
	recordBuildingDuration = flag.Bool("enable-building-duration-metric", true, "record the building_duration metric\n"+buildDurationMetricDesc)
	recordExecutionTime    = flag.Bool("enable-execution-time-metric", false, "record the execution_time metric"+executionTimeMetricDesc) // no '\n' because executionTimeMetricDesc is an empty strinng
	recordWaitingTime      = flag.Bool("enable-waiting-time-metric", false, "record the waiting_time metric\n"+waitingTimeMetricDesc)
	printVersion           = flag.Bool("version", false, "print the version and exit")
)

func init() {
	flag.Var(&jenkinsJobWhitelist, "jenkins-job-whitelist", "Comma-separated list of jenkins job names for that metrics are recorded.\nIf empty metrics for all jobs are recorded.\nMultibranch jobs are identified by their multibranch jobname")
}

func recordMetric(c *prometheus.Collector, jobName, metricType, buildResult, help string, duration time.Duration) {
	const key = "job_duration_seconds"

	c.Summary(key,
		float64(duration/time.Second),
		help,
		map[string]string{
			// The label "job" is already used by Prometheus and
			// applied to all scrape targets.
			"jenkins_job": jobName,
			"type":        metricType,
			"result":      strings.ToLower(buildResult),
		},
	)
}

func recordBuildMetric(c *prometheus.Collector, b *jenkins.Build) {
	// store multibranchjobs by their multibranchjobname, instead of
	// <MultibranchJobName>/<JobName>
	jobName := b.JobName
	if b.MultibranchJobName != "" {
		jobName = b.MultibranchJobName
	}

	// TODO: sanitize jobname?, sometimes contains %20 and other weird
	// chars

	if *recordBlockedTime {
		recordMetric(c, jobName, "blocked_time", b.Result, blockedTimeMetricDescr, b.BlockedTime)
	}
	if *recordBuildAbleTime {
		recordMetric(c, jobName, "buildable_time", b.Result, buildAbleMetricDesc, b.BuildableTime)
	}
	if *recordBuildingDuration {
		recordMetric(c, jobName, "building_duration", b.Result, buildDurationMetricDesc, b.BuildingDuration)
	}
	if *recordExecutionTime {
		recordMetric(c, jobName, "executing_time", b.Result, executionTimeMetricDesc, b.ExecutingTime)
	}
	if *recordWaitingTime {
		recordMetric(c, jobName, "waiting_time", b.Result, waitingTimeMetricDesc, b.WaitingTime)
	}
}

func buildsByJob(builds []*jenkins.Build) map[string][]*jenkins.Build {
	res := map[string][]*jenkins.Build{}

	for _, b := range builds {
		var jobName string

		if b.MultibranchJobName != "" {
			jobName = b.MultibranchJobName + "/"
		}
		jobName += b.JobName

		jobBuilds := res[jobName]

		res[jobName] = append(jobBuilds, b)
	}

	return res
}

func sortDescByID(in map[string][]*jenkins.Build) {
	for _, builds := range in {

		sort.Slice(builds, func(i, j int) bool {
			return builds[i].ID > builds[j].ID
		})
	}
}

func jobIsWhitelisted(build *jenkins.Build) bool {
	// TODO: would be more efficient to only retrieve the information for the jobs
	// that we are interesting in from the API, instead of retrieving all
	// and ignoring some

	jobName := build.JobName
	if build.MultibranchJobName != "" {
		jobName = build.MultibranchJobName
	}

	if len(jenkinsJobWhitelist) == 0 {
		return true
	}

	if _, exist := jenkinsJobWhitelist[jobName]; exist {
		return true
	}

	return false
}

func fetchAndRecord(clt *jenkins.Client, store *store.Store, collector *prometheus.Collector) error {
	// TODO: remove records of jobs that do not exist anymore(?)
	// otherwise they will be carried arround in the state file
	// forever

	var recordCnt int
	fetchStart := time.Now()

	builds, err := clt.Builds(false)
	if err != nil {
		return err
	}

	logger.Printf("retrieved %d builds from jenkins in %s", len(builds), time.Since(fetchStart))

	buildMap := buildsByJob(builds)
	sortDescByID(buildMap)

	for job, builds := range buildMap {
		if len(builds) == 0 {
			// should we record a highest buildID of -1 instead,
			// so that the 0 builds will be recorded when it ran?
			continue
		}

		// We can not pass job here because it's in the format
		// <MultiBranchJobName>/<JobName>. The whitelist contains
		// either the MultiBranchJobName or the JobName
		if !jobIsWhitelisted(builds[0]) {
			continue
		}

		highestID, exist := store.Get(job)
		if !exist {
			// if we do not have a record for a job,
			// only store the highest ID and skip it.
			// On a subsequent runs new builds will be recorded.
			// This prevents that we record multiple times the
			// same builds if no state store file exist of the
			// previous run.

			store.Set(job, builds[0].ID)
			logger.Printf("%s: seen the first time, skipping existing builds, highest build ID: %d", job, builds[0].ID)
			continue
		}
		if highestID > builds[0].ID {
			logger.Printf("%s: highest job ID on jenkins server is higher then the stored one, resetting ID to: %d", job, builds[0].ID)
			store.Set(job, builds[0].ID)
		}

		for _, b := range builds {
			if b.ID <= highestID {
				// all following builds are know
				break
			}

			recordBuildMetric(collector, b)
			recordCnt++
		}
		if builds[0].ID > highestID {
			logger.Printf("%s: recorded metrics for %d build(s), new highest build ID: %d", job, builds[0].ID-highestID, builds[0].ID)
			logger.Printf("%s: highest seen build ID is %d", job, builds[0].ID)
			store.Set(job, builds[0].ID)
		}
	}

	logger.Printf("recorded metrics for %d build(s)", recordCnt)

	return nil
}

func loadOrCreateStateStore() *store.Store {
	stateStore, err := store.FromFile(*stateFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("state file '%s' does not exist", *stateFilePath)
			return store.New()
		}

		log.Fatalf("loading state file failed: %s", err)
	}

	log.Printf("state loaded from '%s'", *stateFilePath)

	return stateStore
}

func registerSigHandler(s *store.Store) {
	sigChan := make(chan os.Signal)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		sig := <-sigChan

		logger.Printf("received %s signal, terminating...", sig)

		err := s.ToFile(*stateFilePath)
		if err != nil {
			logger.Printf("saving statefile failed: %s", err)
		} else {
			logger.Printf("state written to %s", *stateFilePath)
		}

		os.Exit(0)
	}()
}

func logConfiguration() {
	const fmtSpec = "%-30s: %v\n"

	str := "Configuration:\n"
	str += fmt.Sprintf(fmtSpec, "Jenkins URL", *jenkinsURL)
	str += fmt.Sprintf(fmtSpec, "Jenkins Username", *jenkinsUsername)
	str += fmt.Sprintf(fmtSpec, "Jenkins Password", "##hidden##")
	str += fmt.Sprintf(fmtSpec, "Jenkins Job Whitelist", jenkinsJobWhitelist.String())
	str += fmt.Sprintf(fmtSpec, "Listen Address", *listenAddr)
	str += fmt.Sprintf(fmtSpec, "State File", *stateFilePath)
	str += fmt.Sprintf(fmtSpec, "Poll Interval (sec)", *pollIntervalSec)
	str += fmt.Sprintf(fmtSpec, "HTTP Timeout (sec)", *httpTimeout)
	str += fmt.Sprintf(fmtSpec, "Prometheus Namespace", *prometheusNamespace)
	str += fmt.Sprintf(fmtSpec, "Record blocked_time", *recordBlockedTime)
	str += fmt.Sprintf(fmtSpec, "Record buildable_time", *recordBuildAbleTime)
	str += fmt.Sprintf(fmtSpec, "Record building_duration", *recordBuildingDuration)
	str += fmt.Sprintf(fmtSpec, "Record execution_time", *recordExecutionTime)
	str += fmt.Sprintf(fmtSpec, "Record waiting_time", *recordWaitingTime)

	logger.Printf(str)
}

func validateFlags() {
	if *jenkinsURL == "" {
		fmt.Printf("Error: -jenkins-url parameter must be specified\n\n")
		flag.Usage()
		os.Exit(1)
	}
}

func main() {
	logger = log.New(os.Stderr, "", log.LstdFlags)

	envy.Parse("JE")
	flag.Parse()
	if *printVersion {
		fmt.Printf("version: %s\n", version)
		os.Exit(0)
	}

	validateFlags()
	logConfiguration()

	pollInterval := time.Duration(*pollIntervalSec) * time.Second
	timeout := time.Duration(*httpTimeout) * time.Second

	http.Handle("/", promhttp.Handler())
	go func() {
		logger.Printf("prometheus http server listening on %s", *listenAddr)
		err := http.ListenAndServe(*listenAddr, nil)
		if err != http.ErrServerClosed {
			logger.Fatal("prometheus http server terminated:", err.Error())
		}

	}()

	stateStore := loadOrCreateStateStore()
	registerSigHandler(stateStore)

	collector := prometheus.NewCollector(*prometheusNamespace, nil)
	clt := jenkins.NewClient(*jenkinsURL).
		WithAuth(*jenkinsUsername, *jenkinsPassword).
		WithLogger(logger).
		WithTimeout(timeout)

	for {
		err := fetchAndRecord(clt, stateStore, collector)
		if err != nil {
			log.Printf("fetching and recording builds metrics failed: %s", err)
			if _, ok := err.(*jenkins.ErrHTTPRequestFailed); !ok {
				os.Exit(1)
			}
		}

		logger.Printf("fetching and recording the next build metrics in %s", pollInterval)
		time.Sleep(pollInterval)
	}
}
