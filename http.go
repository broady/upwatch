package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

var (
	host = flag.String("host", "localhost", "Host to listen on.")
	port = flag.Int("port", 8080, "Port to listen on.")
)

func main() {
	flag.Parse()

	http.HandleFunc("/", http.FileServer(http.Dir("static")).ServeHTTP)
	http.HandleFunc("/boom", boom)

	addr := fmt.Sprintf("%s:%d", *host, *port)
	log.Printf("Listening on %s", addr)
	http.ListenAndServe(addr, nil)
}

type result struct {
	err      error
	code     int
	duration time.Duration
}

type bundle struct {
	Err     int
	Good    int
	Bad     int
	Min     time.Duration
	Max     time.Duration
	ErrText string
}

func (b *bundle) add(r result) {
	if r.err != nil {
		b.Err++
		b.ErrText = r.err.Error()
	} else if r.code > 299 {
		b.Bad++
	} else {
		b.Good++
	}
	if r.duration > b.Max {
		b.Max = r.duration
	}
	if r.duration < b.Min {
		b.Min = r.duration
	}
}

func semaphore(n int) chan bool {
	ch := make(chan bool, n)
	for i := 0; i < n; i++ {
		ch <- true
	}
	return ch
}

func boom(w http.ResponseWriter, r *http.Request) {
	wf, ok := w.(WriteFlusher)
	if !ok {
		http.Error(w, "ResponseWriter is not a Flusher", 500)
		return
	}
	url := r.FormValue("url")
	if url == "" {
		http.Error(w, "Missing URL", http.StatusMethodNotAllowed)
		return
	}
	log.Printf("GET %q", url)
	if _, err := http.NewRequest("GET", url, nil); err != nil {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	// NOTE: ctx is cancelled when the client goes away.
	ctx := r.Context()

	w.Header().Set("Content-Type", "text/event-stream")

	results := make(chan result, 10000)

	// Write bundles to event stream.
	go func() {
		for b := range bundleResults(ctx, results, time.Second) {
			j, _ := json.Marshal(b)
			writeSSE(wf, j)
		}
	}()

	const qps = 200
	const concurrency = 50
	rate := rate.NewLimiter(rate.Every(time.Second/qps), 1)
	sem := semaphore(concurrency)

	for {
		if err := rate.Wait(ctx); err != nil {
			log.Printf("rate.Wait(%q): %v", url, err)
			return
		}
		<-sem
		go func() {
			start := time.Now()
			resp, err := http.Get(url)
			if err != nil {
				r := result{err: err, duration: time.Now().Sub(start)}
				sem <- true
				results <- r
				return
			}
			io.Copy(ioutil.Discard, resp.Body)
			resp.Body.Close()
			r := result{code: resp.StatusCode, duration: time.Now().Sub(start)}
			sem <- true
			results <- r
		}()
	}
}

type WriteFlusher interface {
	io.Writer
	http.Flusher
}

func writeSSE(wf WriteFlusher, data []byte) {
	bb := bytes.Split(data, []byte("\n"))
	for _, b := range bb {
		wf.Write([]byte("data: "))
		wf.Write(b)
		wf.Write([]byte("\n"))
	}
	wf.Write([]byte("\n"))
	wf.Flush()
}

func bundleResults(ctx context.Context, results chan result, d time.Duration) chan bundle {
	bundles := make(chan bundle, 1)
	tick := time.NewTicker(time.Second)

	go func() {
		b := bundle{Min: time.Duration(1<<63 - 1)}
		for {
			select {
			case <-ctx.Done():
				close(bundles)
				tick.Stop()
				return

			case <-tick.C:
				bundles <- b
				b = bundle{Min: time.Duration(1<<63 - 1)}

			case r := <-results:
				b.add(r)
			}
		}
	}()

	return bundles
}
