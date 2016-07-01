/*
Copyright 2015 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// A tiny web server that returns 200 on it's healthz endpoint if the command
// passed in via -cmd exits with 0. Returns 503 otherwise.
// Usage: exechealthz -port 8080 -period 2s -latency 30s -cmd 'nslookup localhost >/dev/null'
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// TODO:
// 1. Sigterm handler for docker stop
// 2. Meaningful default healthz
// 3. 404 for unknown endpoints

var (
	port            = flag.Int("port", 8080, "Port number to serve /healthz.")
	cmd             = flag.String("cmd", "echo healthz", "Command to run in response to a GET on /healthz. If the given command exits with 0, /healthz will respond with a 200.")
	queryRecord     = flag.String("query-record", "kubernetes.default.svc.cluster.local.", "A record to query for")
	queryNameserver = flag.String("query-nameserver", "127.0.0.1:53", "nameserver to connect to")
	queryTimeout    = flag.Duration("query-timeout", 2*time.Second, "default query timeouts (sets all: dial, read, write)")

	period     = flag.Duration("period", 2*time.Second, "Period to run the given cmd in an async worker.")
	maxLatency = flag.Duration("latency", 30*time.Second, "If the async worker hasn't updated the probe command output in this long, return a 503.")
	quiet      = flag.Bool("quiet", false, "Run in quiet mode by only logging errors.")
	// prober is the async worker running the cmd, the output of which is used to service /healthz.
	prober *execWorker
)

// execResult holds the result of the latest exec from the execWorker.
type execResult struct {
	msg *dns.Msg
	err error
	ts  time.Time
	rtt time.Duration
}

func (r *execResult) Copy() *execResult {
	var msg *dns.Msg
	if r.msg != nil {
		msg = r.msg.Copy()
	}
	return &execResult{
		msg: msg,
		err: r.err,
		ts:  r.ts,
		rtt: r.rtt,
	}
}

func (r *execResult) isOK() error {
	// no real result yet
	if r.msg == nil {
		return nil
	}
	if r.err != nil {
		return fmt.Errorf("healthz probe error: %v", r)
	}
	if time.Since(r.ts) > *maxLatency {
		return fmt.Errorf("latest result too old to be useful: %v", r)
	}
	if len(r.msg.Answer) == 0 {
		return fmt.Errorf("empty response from server: %v", r)
	}
	return nil
}

func (r *execResult) String() string {
	errMsg := "None"
	if r.err != nil {
		errMsg = fmt.Sprintf("%v", r.err)
	}
	return fmt.Sprintf("Result of last exec: %v, at %v, error %v", r.msg.String(), r.ts, errMsg)
}

// execWorker provides an async interface to exec.
type execWorker struct {
	client          *dns.Client
	result          *execResult
	mutex           sync.Mutex
	period          time.Duration
	probeCmd        string
	queryRecord     string
	queryNameserver string
	stopCh          chan struct{}
	timeout         time.Duration
}

// getResults returns the results of the latest execWorker run.
// The caller should treat returned results as read-only.
func (h *execWorker) getResults() *execResult {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return h.result.Copy()
}

// logf logs the message, unless we run in quiet mode.
func logf(format string, args ...interface{}) {
	if !*quiet {
		log.Printf(format, args...)
	}
}

// start attemtps to run the probeCmd every `period` seconds.
// Meant to be called as a goroutine.
func (h *execWorker) start() {
	ticker := time.NewTicker(h.period)
	defer ticker.Stop()

	for {
		select {
		// If the command takes > period, the command runs continuously.
		case <-ticker.C:
			logf("Worker query A record %v at %v", h.queryRecord, h.queryNameserver)
			result := h.query()
			logf("%v", result)
			h.setResult(result)
		case <-h.stopCh:
			return
		}
	}
}

func (h *execWorker) query() *execResult {
	msg := new(dns.Msg)
	msg.SetQuestion(h.queryRecord, dns.TypeA)
	re, rtt, err := h.client.Exchange(msg, h.queryNameserver)
	return &execResult{
		msg: re,
		rtt: rtt,
		err: err,
		ts:  time.Now().UTC(),
	}
}

func (h *execWorker) setResult(result *execResult) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.result = result
}

// newExecWorker is a constructor for execWorker.
func newExecWorker(queryRecord, queryNameserver string, queryTimeout, execPeriod time.Duration) *execWorker {
	client := &dns.Client{
		Timeout:      queryTimeout,
		DialTimeout:  queryTimeout,
		ReadTimeout:  queryTimeout,
		WriteTimeout: queryTimeout,
	}
	return &execWorker{
		// Initializing the result with a timestamp here allows us to
		// wait maxLatency for the worker goroutine to start, and for each
		// iteration of the worker to complete.
		client:          client,
		result:          &execResult{ts: time.Now()},
		period:          execPeriod,
		queryRecord:     queryRecord,
		queryNameserver: queryNameserver,
		stopCh:          make(chan struct{}),
	}
}

func main() {
	flag.Parse()
	links := []struct {
		link, desc string
	}{
		{"/healthz", "healthz probe. Returns \"ok\" if the command given through -cmd exits with 0."},
		{"/quit", "Cause this container to exit."},
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "<b> Kubernetes healthz sidecar container </b><br/><br/>")
		for _, v := range links {
			fmt.Fprintf(w, `<a href="%v">%v: %v</a><br/>`, v.link, v.link, v.desc)
		}
	})

	http.HandleFunc("/quit", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Shutdown requested via /quit by %v", r.RemoteAddr)
		os.Exit(0)
	})
	prober = newExecWorker(*queryRecord, *queryNameserver, *queryTimeout, *period)
	defer close(prober.stopCh)
	go prober.start()

	http.HandleFunc("/healthz", healthzHandler)
	log.Fatal(http.ListenAndServe(fmt.Sprintf("0.0.0.0:%d", *port), nil))
}

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	logf("Client ip %v requesting /healthz probe servicing cmd %v", r.RemoteAddr, *cmd)
	result := prober.getResults()

	err := result.isOK()
	if err != nil {
		log.Printf(err.Error())
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintf(w, fmt.Sprintf("ok: %v", result))
}
