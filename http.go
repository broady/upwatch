package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"github.com/davecgh/go-spew/spew"

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

func boom(w http.ResponseWriter, r *http.Request) {
	url := r.FormValue("url")
	if url == "" {
		http.Error(w, "Missing URL", http.StatusMethodNotAllowed)
		return
	}

	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "ResponseWriter is not a Flusher", 500)
		return
	}

	ctx := r.Context()
	const qps = 200
	const concurrency = 50
	rate := rate.NewLimiter(rate.Every(time.Second/qps), 1)
	sem := make(chan bool, concurrency)
	for i := 0; i < cap(sem); i++ {
		sem <- true
	}
	results := make(chan result, 10000)

	w.Header().Set("Content-Type", "text/event-stream")

	log.Printf("GET %q", url)

	_, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("NewRequest: %v", err)
		return
	}

	tick := time.Tick(time.Second)
	go func() {
		b := bundle{Min: time.Duration(1<<63 - 1)}
		for {
			select {
			case <-ctx.Done():
				return
			case r := <-results:
				if r.err != nil {
					b.Err++
					b.ErrText = spew.Sdump(r.err)
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
			case <-tick:
				j, _ := json.Marshal(b)
				writeData(w, j)
				f.Flush()
				b = bundle{Min: time.Duration(1<<63 - 1)}
			}
		}
	}()

	for {
		if err := rate.Wait(ctx); err != nil {
			log.Printf("rate.Wait(%q): %v", url, err)
			return
		}
		<-sem
		go func() {
			start := time.Now()
			req, _ := http.NewRequest("GET", url, nil)
			resp, err := http.DefaultClient.Do(req)
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

func writeData(w io.Writer, data []byte) {
	bb := bytes.Split(data, []byte("\n"))
	for _, b := range bb {
		w.Write([]byte("data: "))
		w.Write(b)
		w.Write([]byte("\n"))
	}
	w.Write([]byte("\n"))
}
