package main

import (
	"errors"
	"flag"
	"labix.org/v2/mgo"
	"log"
	. "replay"
	"runtime"
	"sync/atomic"
	"time"
)

func panicOnError(err error) {
	if err != nil {
		panic(err)
	}
}

var (
	opsFilename string
	url         string
	workers     int
	maxOps      int
	style       string
	sampleRate  float64
)

func init() {
	flag.StringVar(&opsFilename, "ops_filename", "",
		"The file for the serialized ops, generated by the record scripts.")
	flag.StringVar(&url, "url", "",
		"The database server's url, in the format of <host>[:<port>]")
	flag.StringVar(&style, "style", "",
		"How to replay the the ops. You can choose: \n"+
			"	stress: repaly ops at fast as possible\n"+
			"	real: repaly ops in accordance to ops' timestamps")
	flag.IntVar(&workers, "workers", 10,
		"Number of workers that sends ops to database.")
	flag.IntVar(&maxOps, "maxOps", 0,
		"[Optional] Maximal amount of ops to be replayed from the "+
			"ops_filename file. By setting it to `0`, replayer will "+
			"replay all the ops.")
	flag.Float64Var(&sampleRate, "sample_rate", 0.0, "sample ops for latency")
}

func parseFlags() error {
	flag.Parse()
	if style != "stress" && style != "real" {
		return errors.New("Cannot recognize the style: " + style)
	}
	if workers <= 0 {
		return errors.New("`workers` should be a positive number")
	}
	if maxOps == 0 {
		maxOps = 4294967295
	}
	return nil
}

func main() {
	// Will enable system threads to make sure all cpus can be well utilized.
	runtime.GOMAXPROCS(100)
	err := parseFlags()
	panicOnError(err)

	// Prepare to dispatch ops
	var reader OpsReader
	var opsChan chan *Op
	if style == "stress" {
		err, reader = NewFileByLineOpsReader(opsFilename)
		panicOnError(err)
		opsChan = NewBestEffortOpsDispatcher(reader, maxOps)
	} else {
		// TODO NewCyclicOpsReader: do we really want to make it cyclic?
		reader = NewCyclicOpsReader(func() OpsReader {
			err, reader := NewFileByLineOpsReader(opsFilename)
			panicOnError(err)
			return reader
		})
		opsChan = NewByTimeOpsDispatcher(reader, maxOps)
	}

	latencyChan := make(chan Latency, workers)

	// Set up workers to do the job
	exit := make(chan int)
	opsExecuted := int64(0)
	fetch := func(id int, statsCollector IStatsCollector) {
		log.Printf("Worker #%d report for duty\n", id)
		session, err := mgo.Dial(url)
		panicOnError(err)
		defer session.Close()
		exec := OpsExecutorWithStats(session, statsCollector)
		for {
			op := <-opsChan
			if op == nil {
				break
			}
			exec.Execute(op)
			atomic.AddInt64(&opsExecuted, 1)
		}
		exit <- 1
		log.Printf("Worker #%d done!\n", id)
	}
	statsCollectorList := make([]*StatsCollector, workers)
	for i := 0; i < workers; i++ {
		statsCollectorList[i] = NewStatsCollector()
		statsCollectorList[i].SampleLatencies(sampleRate, latencyChan)
		go fetch(i, statsCollectorList[i])
	}

	// Periodically report execution status
	go func() {
		StatsAnalyzer := NewStatsAnalyzer(statsCollectorList, &opsExecuted,
			latencyChan, int(sampleRate*float64(maxOps)))
		toFloat := func(nano int64) float64 {
			return float64(nano) / float64(1e6)
		}
		report := func() {
			status := StatsAnalyzer.GetStatus()
			log.Printf("Executed %d ops, %.2f ops/sec", opsExecuted,
				status.OpsPerSec)
			for _, opType := range AllOpTypes {
				allTime := status.AllTimeLatencies[opType]
				sinceLast := status.SinceLastLatencies[opType]
				log.Printf("  Op type: %s, count: %d, ops/sec: %.2f",
					opType, status.Counts[opType], status.TypeOpsSec[opType])
				template := "   %s: P50: %.2fms, P70: %.2fms, P90: %.2fms, " +
					"P95 %.2fms, P99 %.2fms, Max %.2fms\n"
				log.Printf(template, "Total", toFloat(allTime[P50]),
					toFloat(allTime[P70]), toFloat(allTime[P90]),
					toFloat(allTime[P95]), toFloat(allTime[P99]),
					toFloat(allTime[P100]))
				log.Printf(template, "Last ", toFloat(sinceLast[P50]),
					toFloat(sinceLast[P70]), toFloat(sinceLast[P90]),
					toFloat(sinceLast[P95]), toFloat(sinceLast[P99]),
					toFloat(sinceLast[P100]))
			}
		}
		defer report()

		for opsExecuted < int64(maxOps) {
			time.Sleep(5 * time.Second)
			report()
		}
	}()

	// Wait for workers
	received := 0
	for received < workers {
		<-exit
		received += 1
	}
}
