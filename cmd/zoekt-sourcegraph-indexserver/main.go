// Command zoekt-sourcegraph-indexserver periodically reindexes enabled
// repositories on sourcegraph
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"cloud.google.com/go/profiler"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/keegancsmith/tmpfriend"
	"github.com/peterbourgon/ff/v3/ffcli"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/automaxprocs/maxprocs"
	"golang.org/x/net/trace"

	"github.com/google/zoekt"
	"github.com/google/zoekt/build"
	"github.com/google/zoekt/debugserver"
)

var (
	metricResolveRevisionsDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "resolve_revisions_seconds",
		Help:    "A histogram of latencies for resolving all repository revisions.",
		Buckets: prometheus.ExponentialBuckets(1, 10, 6), // 1s -> 27min
	})

	metricResolveRevisionDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "resolve_revision_seconds",
		Help:    "A histogram of latencies for resolving a repository revision.",
		Buckets: prometheus.ExponentialBuckets(.25, 2, 4), // 250ms -> 2s
	}, []string{"success"}) // success=true|false

	metricGetIndexOptions = promauto.NewCounter(prometheus.CounterOpts{
		Name: "get_index_options_total",
		Help: "The total number of times we tried to get index options for a repository. Includes errors.",
	})
	metricGetIndexOptionsError = promauto.NewCounter(prometheus.CounterOpts{
		Name: "get_index_options_error_total",
		Help: "The total number of times we failed to get index options for a repository.",
	})

	metricIndexDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "index_repo_seconds",
		Help:    "A histogram of latencies for indexing a repository.",
		Buckets: prometheus.ExponentialBuckets(.1, 10, 7), // 100ms -> 27min
	}, []string{
		"state", // state is an indexState
		"name",  // name of the repository that was indexed
	})

	metricFetchDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "index_fetch_seconds",
		Help:    "A histogram of latencies for fetching a repository.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 20, 30, 60, 180, 300, 600}, // 50ms -> 10 minutes
	}, []string{
		"success", // true|false
		"name",    // the name of the repository that the commits were fetched from
	})

	metricIndexIncrementalIndexState = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "index_incremental_index_state",
		Help: "A count of the state on disk vs what we want to build. See zoekt/build.IndexState.",
	}, []string{"state"}) // state is build.IndexState

	metricNumIndexed = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "index_num_indexed",
		Help: "Number of indexed repos by code host",
	})

	metricNumAssigned = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "index_num_assigned",
		Help: "Number of repos assigned to this indexer by code host",
	})

	metricFailingTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "index_failing_total",
		Help: "Counts failures to index (indexing activity, should be used with rate())",
	})

	metricIndexingTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "index_indexing_total",
		Help: "Counts indexings (indexing activity, should be used with rate())",
	})

	metricNumStoppedTrackingTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "index_num_stopped_tracking_total",
		Help: "Counts the number of repos we stopped tracking.",
	})
)

// set of repositories that we want to capture separate indexing metrics for
var reposWithSeparateIndexingMetrics = make(map[string]struct{})

type indexState string

const (
	indexStateFail        indexState = "fail"
	indexStateSuccess     indexState = "success"
	indexStateSuccessMeta indexState = "success_meta" // We only updated metadata
	indexStateNoop        indexState = "noop"         // We didn't need to update index
	indexStateEmpty       indexState = "empty"        // index is empty (empty repo)
)

// Server is the main functionality of zoekt-sourcegraph-indexserver. It
// exists to conveniently use all the options passed in via func main.
type Server struct {
	Sourcegraph Sourcegraph
	BatchSize   int

	// IndexDir is the index directory to use.
	IndexDir string

	// Interval is how often we sync with Sourcegraph.
	Interval time.Duration

	// VacuumInterval is how often indexserver scans compound shards to remove
	// tombstones.
	VacuumInterval time.Duration

	// MergeInterval defines how often indexserver runs the merge operation in the index
	// directory.
	MergeInterval time.Duration

	// TargetSizeBytes is the target size in bytes for compound shards. The higher
	// the value the more repositories a compound shard will contain and the bigger
	// the potential for saving MEM. The savings in MEM come at the cost of a
	// degraded search performance.
	TargetSizeBytes int64

	// Compound shards smaller than minSizeBytes will be deleted by vacuum.
	minSizeBytes int64

	// CPUCount is the amount of parallelism to use when indexing a
	// repository.
	CPUCount int

	queue Queue

	// Protects the index directory from concurrent access.
	muIndexDir sync.Mutex

	// If true, shard merging is enabled.
	shardMerging bool
}

var debug = log.New(ioutil.Discard, "", log.LstdFlags)

// our index commands should output something every 100mb they process.
//
// 2020-11-24 Keegan. "This should be rather quick so 5m is more than enough
// time."  famous last words. A client was indexing a monorepo with 42
// cores... 5m was not enough.
const noOutputTimeout = 30 * time.Minute

func (s *Server) loggedRun(tr trace.Trace, cmd *exec.Cmd) (err error) {
	out := &synchronizedBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out

	tr.LazyPrintf("%s", cmd.Args)

	defer func() {
		if err != nil {
			outS := out.String()
			tr.LazyPrintf("failed: %v", err)
			tr.LazyPrintf("output: %s", out)
			tr.SetError()
			err = fmt.Errorf("command %s failed: %v\nOUT: %s", cmd.Args, err, outS)
		}
	}()

	if err := cmd.Start(); err != nil {
		return err
	}

	errC := make(chan error)
	go func() {
		errC <- cmd.Wait()
	}()

	// This channel is set after we have sent sigquit. It allows us to follow up
	// with a sigkill if the process doesn't quit after sigquit.
	kill := make(<-chan time.Time)

	lastLen := 0
	for {
		select {
		case <-time.After(noOutputTimeout):
			// Periodically check if we have had output. If not kill the process.
			if out.Len() != lastLen {
				lastLen = out.Len()
				log.Printf("still running %s", cmd.Args)
			} else {
				// Send quit (C-\) first so we get a stack dump.
				log.Printf("no output for %s, quitting %s", noOutputTimeout, cmd.Args)
				if err := cmd.Process.Signal(syscall.SIGQUIT); err != nil {
					log.Println("quit failed:", err)
				}

				// send sigkill if still running in 10s
				kill = time.After(10 * time.Second)
			}

		case <-kill:
			log.Printf("still running, killing %s", cmd.Args)
			if err := cmd.Process.Kill(); err != nil {
				log.Println("kill failed:", err)
			}

		case err := <-errC:
			if err != nil {
				return err
			}

			tr.LazyPrintf("success")
			debug.Printf("ran successfully %s", cmd.Args)
			return nil
		}
	}
}

// synchronizedBuffer wraps a strings.Builder with a mutex. Used so we can
// monitor the buffer while it is being written to.
type synchronizedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (sb *synchronizedBuffer) Write(p []byte) (int, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.b.Write(p)
}

func (sb *synchronizedBuffer) Len() int {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.b.Len()
}

func (sb *synchronizedBuffer) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.b.String()
}

// pauseFileName if present in IndexDir will stop index jobs from
// running. This is to make it possible to experiment with the content of the
// IndexDir without the indexserver writing to it.
const pauseFileName = "PAUSE"

// Run the sync loop. This blocks forever.
func (s *Server) Run() {
	removeIncompleteShards(s.IndexDir)

	// Start a goroutine which updates the queue with commits to index.
	go func() {
		// We update the list of indexed repos every Interval. To speed up manual
		// testing we also listen for SIGUSR1 to trigger updates.
		//
		// "pkill -SIGUSR1 zoekt-sourcegra"
		for range jitterTicker(s.Interval, syscall.SIGUSR1) {
			if b, err := os.ReadFile(filepath.Join(s.IndexDir, pauseFileName)); err == nil {
				log.Printf("indexserver manually paused via PAUSE file: %s", string(bytes.TrimSpace(b)))
				continue
			}

			repos, err := s.Sourcegraph.List(context.Background(), listIndexed(s.IndexDir))
			if err != nil {
				log.Println(err)
				continue
			}

			debug.Printf("updating index queue with %d repositories", len(repos.IDs))

			// Stop indexing repos we don't need to track anymore
			count := s.queue.MaybeRemoveMissing(repos.IDs)
			metricNumStoppedTrackingTotal.Add(float64(count))
			if count > 0 {
				log.Printf("stopped tracking %d repositories", count)
			}

			cleanupDone := make(chan struct{})
			go func() {
				defer close(cleanupDone)
				s.muIndexDir.Lock()
				cleanup(s.IndexDir, repos.IDs, time.Now(), s.shardMerging)
				s.muIndexDir.Unlock()
			}()

			repos.IterateIndexOptions(s.queue.AddOrUpdate)

			// IterateIndexOptions will only iterate over repositories that have
			// changed since we last called list. However, we want to add all IDs
			// back onto the queue just to check that what is on disk is still
			// correct. This will use the last IndexOptions we stored in the
			// queue. The repositories not on the queue (missing) need a forced
			// fetch of IndexOptions.
			missing := s.queue.Bump(repos.IDs)
			s.Sourcegraph.ForceIterateIndexOptions(s.queue.AddOrUpdate, func(uint32, error) {}, missing...)

			setCompoundShardCounter(s.IndexDir)

			<-cleanupDone
		}
	}()

	go func() {
		for range jitterTicker(s.VacuumInterval, syscall.SIGUSR1) {
			if s.shardMerging {
				s.vacuum()
			}
		}
	}()

	go func() {
		for range jitterTicker(s.MergeInterval, syscall.SIGUSR1) {
			if s.shardMerging {
				err := doMerge(s.IndexDir, s.TargetSizeBytes, false)
				if err != nil {
					log.Printf("error during merging: %s", err)
				}
			}
		}
	}()

	// In the current goroutine process the queue forever.
	for {
		if _, err := os.Stat(filepath.Join(s.IndexDir, pauseFileName)); err == nil {
			time.Sleep(time.Second)
			continue
		}

		opts, ok := s.queue.Pop()
		if !ok {
			time.Sleep(time.Second)
			continue
		}
		start := time.Now()
		args := s.indexArgs(opts)

		s.muIndexDir.Lock()
		state, err := s.Index(args)
		s.muIndexDir.Unlock()

		elapsed := time.Since(start)

		metricIndexDuration.WithLabelValues(string(state), repoNameForMetric(opts.Name)).Observe(elapsed.Seconds())

		if err != nil {
			log.Printf("error indexing %s: %s", args.String(), err)
		}

		switch state {
		case indexStateSuccess:
			log.Printf("updated index %s in %v", args.String(), elapsed)
		case indexStateSuccessMeta:
			log.Printf("updated meta %s in %v", args.String(), elapsed)
		}
		s.queue.SetIndexed(opts, state)
	}
}

// repoNameForMetric returns a normalized version of the given repository name that is
// suitable for use with Prometheus metrics.
func repoNameForMetric(repo string) string {
	// Check to see if we want to be able to capture separate indexing metrics for this repository.
	// If we don't, set to a default string to keep the cardinality for the Prometheus metric manageable.
	if _, ok := reposWithSeparateIndexingMetrics[repo]; ok {
		return repo
	}

	return ""
}

func batched(slice []uint32, size int) <-chan []uint32 {
	c := make(chan []uint32)
	go func() {
		for len(slice) > 0 {
			if size > len(slice) {
				size = len(slice)
			}
			c <- slice[:size]
			slice = slice[size:]
		}
		close(c)
	}()
	return c
}

// jitterTicker returns a ticker which ticks with a jitter. Each tick is
// uniformly selected from the range (d/2, d + d/2). It will tick on creation.
//
// sig is a list of signals which also cause the ticker to fire. This is a
// convenience to allow manually triggering of the ticker.
func jitterTicker(d time.Duration, sig ...os.Signal) <-chan struct{} {
	ticker := make(chan struct{})

	go func() {
		for {
			ticker <- struct{}{}
			ns := int64(d)
			jitter := rand.Int63n(ns)
			time.Sleep(time.Duration(ns/2 + jitter))
		}
	}()

	go func() {
		if len(sig) == 0 {
			return
		}

		c := make(chan os.Signal, 1)
		signal.Notify(c, sig...)
		for range c {
			ticker <- struct{}{}
		}
	}()

	return ticker
}

// Index starts an index job for repo name at commit.
func (s *Server) Index(args *indexArgs) (state indexState, err error) {
	tr := trace.New("index", args.Name)

	defer func() {
		if err != nil {
			tr.SetError()
			tr.LazyPrintf("error: %v", err)
			state = indexStateFail
			metricFailingTotal.Inc()
		}
		tr.LazyPrintf("state: %s", state)
		tr.Finish()
	}()

	tr.LazyPrintf("branches: %v", args.Branches)

	if len(args.Branches) == 0 {
		return indexStateEmpty, createEmptyShard(args)
	}

	reason := "forced"
	if args.Incremental {
		bo := args.BuildOptions()
		bo.SetDefaults()
		incrementalState, fn := bo.IndexState()
		reason = string(incrementalState)
		metricIndexIncrementalIndexState.WithLabelValues(string(incrementalState)).Inc()
		switch incrementalState {
		case build.IndexStateEqual:
			debug.Printf("%s index already up to date. Shard=%s", args.String(), fn)
			return indexStateNoop, nil

		case build.IndexStateMeta:
			log.Printf("updating index.meta %s", args.String())

			if err := mergeMeta(bo); err != nil {
				log.Printf("falling back to full update: failed to update index.meta %s: %s", args.String(), err)
			} else {
				return indexStateSuccessMeta, nil
			}

		case build.IndexStateCorrupt:
			log.Printf("falling back to full update: corrupt index: %s", args.String())
		}
	}

	log.Printf("updating index %s reason=%s", args.String(), reason)

	runCmd := func(cmd *exec.Cmd) error { return s.loggedRun(tr, cmd) }
	metricIndexingTotal.Inc()
	return indexStateSuccess, gitIndex(args, runCmd)
}

func (s *Server) indexArgs(opts IndexOptions) *indexArgs {
	return &indexArgs{
		IndexOptions: opts,

		IndexDir:    s.IndexDir,
		Parallelism: s.CPUCount,

		Incremental: true,

		// 1 MB; match https://sourcegraph.sgdev.org/github.com/sourcegraph/sourcegraph/-/blob/cmd/symbols/internal/symbols/search.go#L22
		FileLimit: 1 << 20,

		// We are downloading archives from within the same network from
		// another Sourcegraph service (gitserver). This can end up being
		// so fast that we harm gitserver's network connectivity and our
		// own. In the case of zoekt-indexserver and gitserver running on
		// the same host machine, we can even reach up to ~100 Gbps and
		// effectively DoS the Docker network, temporarily disrupting other
		// containers running on the host.
		//
		// Google Compute Engine has a network bandwidth of about 1.64 Gbps
		// between nodes, and AWS varies widely depending on instance type.
		// We play it safe and default to 1 Gbps here (~119 MiB/s), which
		// means we can fetch a 1 GiB archive in ~8.5 seconds.
		DownloadLimitMBPS: "1000", // 1 Gbps
	}
}

func createEmptyShard(args *indexArgs) error {
	bo := args.BuildOptions()
	bo.SetDefaults()
	bo.RepositoryDescription.Branches = []zoekt.RepositoryBranch{{Name: "HEAD", Version: "404aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}

	if args.Incremental && bo.IncrementalSkipIndexing() {
		return nil
	}

	builder, err := build.NewBuilder(*bo)
	if err != nil {
		return err
	}
	return builder.Finish()
}

var repoTmpl = template.Must(template.New("name").Parse(`
<html><body>
<a href="debug/requests">Traces</a><br>
{{.IndexMsg}}<br />
<br />
<h3>Re-index repository</h3>
<form action="/" method="post">
{{range .Repos}}
<button type="submit" name="repo" value="{{ .ID }}" />{{ .Name }}</button><br />
{{end}}
</form>
</body></html>
`))

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	type Repo struct {
		ID   uint32
		Name string
	}
	var data struct {
		Repos    []Repo
		IndexMsg string
	}

	if r.Method == "POST" {
		_ = r.ParseForm()
		if id, err := strconv.Atoi(r.Form.Get("repo")); err != nil {
			data.IndexMsg = err.Error()
		} else {
			data.IndexMsg, _ = s.forceIndex(uint32(id))
		}
	}

	s.queue.Iterate(func(opts *IndexOptions) {
		data.Repos = append(data.Repos, Repo{
			ID:   opts.RepoID,
			Name: opts.Name,
		})
	})

	_ = repoTmpl.Execute(w, data)
}

// forceIndex will run the index job for repo name now. It will return always
// return a string explaining what it did, even if it failed.
func (s *Server) forceIndex(id uint32) (string, error) {
	var opts IndexOptions
	var err error
	s.Sourcegraph.ForceIterateIndexOptions(func(o IndexOptions) {
		opts = o
	}, func(_ uint32, e error) {
		err = e
	}, id)
	if err != nil {
		return fmt.Sprintf("Indexing %d failed: %v", id, err), err
	}

	args := s.indexArgs(opts)
	args.Incremental = false // force re-index
	state, err := s.Index(args)
	if err != nil {
		return fmt.Sprintf("Indexing %s failed: %s", args.String(), err), err
	}
	return fmt.Sprintf("Indexed %s with state %s", args.String(), state), nil
}

func listIndexed(indexDir string) []uint32 {
	index := getShards(indexDir)
	metricNumIndexed.Set(float64(len(index)))
	repoIDs := make([]uint32, 0, len(index))
	for id := range index {
		repoIDs = append(repoIDs, id)
	}
	sort.Slice(repoIDs, func(i, j int) bool {
		return repoIDs[i] < repoIDs[j]
	})
	return repoIDs
}

func hostnameBestEffort() string {
	if h := os.Getenv("NODE_NAME"); h != "" {
		return h
	}
	if h := os.Getenv("HOSTNAME"); h != "" {
		return h
	}
	hostname, _ := os.Hostname()
	return hostname
}

// setupTmpDir sets up a temporary directory on the same volume as the
// indexes.
//
// If main is true we will delete older temp directories left around. main is
// false when this is a debug command.
func setupTmpDir(index string) error {
	tmpRoot := filepath.Join(index, ".indexserver.tmp")
	if err := os.MkdirAll(tmpRoot, 0755); err != nil {
		return err
	}
	if !tmpfriend.IsTmpFriendDir(tmpRoot) {
		_, err := tmpfriend.RootTempDir(tmpRoot)
		return err
	}
	return nil
}

func printMetaData(fn string) error {
	repo, indexMeta, err := zoekt.ReadMetadataPath(fn)
	if err != nil {
		return err
	}

	err = json.NewEncoder(os.Stdout).Encode(indexMeta)
	if err != nil {
		return err
	}

	err = json.NewEncoder(os.Stdout).Encode(repo)
	if err != nil {
		return err
	}
	return nil
}

func printShardStats(fn string) error {
	f, err := os.Open(fn)
	if err != nil {
		return err
	}

	iFile, err := zoekt.NewIndexFile(f)
	if err != nil {
		return err
	}

	return zoekt.PrintNgramStats(iFile)
}

func initializeGoogleCloudProfiler() {
	// Google cloud profiler is opt-in since we only want to run it on
	// Sourcegraph.com.
	if os.Getenv("GOOGLE_CLOUD_PROFILER_ENABLED") == "" {
		return
	}

	err := profiler.Start(profiler.Config{
		Service:        "zoekt-sourcegraph-indexserver",
		ServiceVersion: zoekt.Version,
		MutexProfiling: true,
		AllocForceGC:   true,
	})
	if err != nil {
		log.Printf("could not initialize google cloud profiler: %s", err.Error())
	}
}

func srcLogLevelIsDebug() bool {
	lvl := os.Getenv("SRC_LOG_LEVEL")
	return strings.EqualFold(lvl, "dbug") || strings.EqualFold(lvl, "debug")
}

func getEnvWithDefaultInt64(k string, defaultVal int64) int64 {
	v := os.Getenv(k)
	if v == "" {
		return defaultVal
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Fatalf("error parsing ENV %s to int64: %s", k, err)
	}
	return i
}

func getEnvWithDefaultInt(k string, defaultVal int) int {
	v := os.Getenv(k)
	if v == "" {
		return defaultVal
	}
	i, err := strconv.Atoi(k)
	if err != nil {
		log.Fatalf("error parsing ENV %s to int: %s", k, err)
	}
	return i
}

func getEnvWithDefaultString(k string, defaultVal string) string {
	v := os.Getenv(k)
	if v == "" {
		return defaultVal
	}
	return v
}

func setCompoundShardCounter(indexDir string) {
	fns, err := filepath.Glob(filepath.Join(indexDir, "compound-*.zoekt"))
	if err != nil {
		log.Printf("setCompoundShardCounter: %s\n", err)
		return
	}
	metricNumberCompoundShards.Set(float64(len(fns)))
}

func rootCmd() *ffcli.Command {
	rootFs := flag.NewFlagSet("rootFs", flag.ExitOnError)
	conf := rootConfig{}
	conf.registerRootFlags(rootFs)

	return &ffcli.Command{
		FlagSet:     rootFs,
		ShortUsage:  "zoekt-sourcegraph-indexserver [flags] [<subcommand>]",
		Subcommands: []*ffcli.Command{debugCmd()},
		Exec: func(ctx context.Context, args []string) error {
			return startServer(conf)
		},
	}
}

type rootConfig struct {
	root             string
	interval         time.Duration
	index            string
	listen           string
	hostname         string
	cpuFraction      float64
	dbg              bool
	blockProfileRate int

	// config values related to shard merging
	vacuumInterval time.Duration
	mergeInterval  time.Duration
	targetSize     int64
	minSize        int64
}

func (rc *rootConfig) registerRootFlags(fs *flag.FlagSet) {
	fs.StringVar(&rc.root, "sourcegraph_url", os.Getenv("SRC_FRONTEND_INTERNAL"), "http://sourcegraph-frontend-internal or http://localhost:3090. If a path to a directory, we fake the Sourcegraph API and index all repos rooted under path.")
	fs.DurationVar(&rc.interval, "interval", time.Minute, "sync with sourcegraph this often")
	fs.DurationVar(&rc.vacuumInterval, "vacuum_interval", 24*time.Hour, "run vacuum this often")
	fs.DurationVar(&rc.mergeInterval, "merge_interval", time.Hour, "run merge this often")
	fs.Int64Var(&rc.targetSize, "merge_target_size", getEnvWithDefaultInt64("SRC_TARGET_SIZE", 2000), "the target size of compound shards in MiB")
	fs.Int64Var(&rc.minSize, "merge_min_size", getEnvWithDefaultInt64("SRC_MIN_SIZE", 1800), "the minimum size of a compound shard in MiB")
	fs.StringVar(&rc.index, "index", getEnvWithDefaultString("DATA_DIR", build.DefaultDir), "set index directory to use")
	fs.StringVar(&rc.listen, "listen", ":6072", "listen on this address.")
	fs.StringVar(&rc.hostname, "hostname", hostnameBestEffort(), "the name we advertise to Sourcegraph when asking for the list of repositories to index. Can also be set via the NODE_NAME environment variable.")
	fs.Float64Var(&rc.cpuFraction, "cpu_fraction", 1.0, "use this fraction of the cores for indexing.")
	fs.BoolVar(&rc.dbg, "debug", srcLogLevelIsDebug(), "turn on more verbose logging.")
	fs.IntVar(&rc.blockProfileRate, "block_profile_rate", getEnvWithDefaultInt("BLOCK_PROFILE_RATE", -1), "Sampling rate of Go's block profiler in nanoseconds. Values <=0 disable the blocking profiler Var(default). A value of 1 includes every blocking event. See https://pkg.go.dev/runtime#SetBlockProfileRate")
}

func startServer(conf rootConfig) error {
	s, err := newServer(conf)
	if err != nil {
		return err
	}

	initializeGoogleCloudProfiler()
	setCompoundShardCounter(s.IndexDir)

	if conf.listen != "" {
		go func() {
			mux := http.NewServeMux()
			debugserver.AddHandlers(mux, true)
			mux.Handle("/", s)
			debug.Printf("serving HTTP on %s", conf.listen)
			log.Fatal(http.ListenAndServe(conf.listen, mux))
		}()
	}

	s.Run()
	return nil
}

func newServer(conf rootConfig) (*Server, error) {
	if conf.cpuFraction <= 0.0 || conf.cpuFraction > 1.0 {
		return nil, fmt.Errorf("cpu_fraction must be between 0.0 and 1.0")
	}
	if conf.index == "" {
		return nil, fmt.Errorf("must set -index")
	}
	if conf.root == "" {
		return nil, fmt.Errorf("must set -sourcegraph_url")
	}
	rootURL, err := url.Parse(conf.root)
	if err != nil {
		return nil, fmt.Errorf("url.Parse(%v): %v", conf.root, err)
	}

	// Tune GOMAXPROCS to match Linux container CPU quota.
	_, _ = maxprocs.Set()

	// Set the sampling rate of Go's block profiler: https://github.com/DataDog/go-profiler-notes/blob/main/guide/README.md#block-profiler.
	// The block profiler is disabled by default.
	runtime.SetBlockProfileRate(conf.blockProfileRate)

	// Automatically prepend our own path at the front, to minimize
	// required configuration.
	if l, err := os.Readlink("/proc/self/exe"); err == nil {
		os.Setenv("PATH", filepath.Dir(l)+":"+os.Getenv("PATH"))
	}

	if _, err := os.Stat(conf.index); err != nil {
		if err := os.MkdirAll(conf.index, 0755); err != nil {
			return nil, fmt.Errorf("MkdirAll %s: %v", conf.index, err)
		}
	}

	if err := setupTmpDir(conf.index); err != nil {
		return nil, fmt.Errorf("failed to setup TMPDIR under %s: %v", conf.index, err)
	}

	if conf.dbg {
		debug = log.New(os.Stderr, "", log.LstdFlags)
	}

	indexingMetricsReposAllowlist := os.Getenv("INDEXING_METRICS_REPOS_ALLOWLIST")
	if indexingMetricsReposAllowlist != "" {
		var repos []string

		for _, r := range strings.Split(indexingMetricsReposAllowlist, ",") {
			r = strings.TrimSpace(r)
			if r != "" {
				repos = append(repos, r)
			}
		}

		for _, r := range repos {
			reposWithSeparateIndexingMetrics[r] = struct{}{}
		}

		debug.Printf("capturing separate indexing metrics for: %s", repos)
	}

	var sg Sourcegraph
	if rootURL.IsAbs() {
		var batchSize int
		if v := os.Getenv("SRC_REPO_CONFIG_BATCH_SIZE"); v != "" {
			batchSize, err = strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("Invalid value for SRC_REPO_CONFIG_BATCH_SIZE, must be int")
			}
		}

		client := retryablehttp.NewClient()
		client.Logger = debug
		sg = &sourcegraphClient{
			Root:      rootURL,
			Client:    client,
			Hostname:  conf.hostname,
			BatchSize: batchSize,
		}
	} else {
		sg = sourcegraphFake{
			RootDir: rootURL.String(),
			Log:     log.New(os.Stderr, "sourcegraph: ", log.LstdFlags),
		}
	}

	cpuCount := int(math.Round(float64(runtime.GOMAXPROCS(0)) * (conf.cpuFraction)))
	if cpuCount < 1 {
		cpuCount = 1
	}
	return &Server{
		Sourcegraph:     sg,
		IndexDir:        conf.index,
		Interval:        conf.interval,
		VacuumInterval:  conf.vacuumInterval,
		MergeInterval:   conf.mergeInterval,
		CPUCount:        cpuCount,
		TargetSizeBytes: conf.targetSize * 1024 * 1024,
		minSizeBytes:    conf.minSize * 1024 * 1024,
		shardMerging:    zoekt.ShardMergingEnabled(),
	}, err
}

func main() {
	if err := rootCmd().ParseAndRun(context.Background(), os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
