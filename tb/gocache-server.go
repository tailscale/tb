package main

import (
	"encoding/json"
	"expvar"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bradfitz/go-tool-cache/cachers"
	"tailscale.com/metrics"
)

var (
	metricPut                 = expvar.NewInt("gocache_put")
	metricActionGetHit        = expvar.NewInt("gocache_action_get_hit")
	metricActionGetMiss       = expvar.NewInt("gocache_action_get_miss")
	metricActionOuputHit      = expvar.NewInt("gocache_output_get_hit")
	metricActionOuputHitBytes = expvar.NewInt("gocache_output_get_hit_bytes")
	metricActionOutputMiss    = expvar.NewInt("gocache_output_get_miss")
	metricGoCacheErr          = &metrics.LabelMap{Label: "err"}
)

func init() {
	expvar.Publish("gocache_error", metricGoCacheErr)
}

type goCacheServer struct {
	cache   *cachers.DiskCache // TODO: add interface for things other than disk cache? when needed.
	verbose bool
	latency time.Duration
}

func (s *goCacheServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	time.Sleep(s.latency)
	if s.verbose {
		log.Printf("%s %s", r.Method, r.RequestURI)
	}
	if r.Method == "PUT" {
		s.handlePut(w, r)
		return
	}
	if r.Method != "GET" {
		http.Error(w, "bad method", http.StatusBadRequest)
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/action/"):
		s.handleGetAction(w, r)
	case strings.HasPrefix(r.URL.Path, "/output/"):
		s.handleGetOutput(w, r)
	case r.URL.Path == "/":
		io.WriteString(w, "hi")
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func getHexSuffix(r *http.Request, prefix string) (hexSuffix string, ok bool) {
	hexSuffix, _ = strings.CutPrefix(r.RequestURI, prefix)
	if !validHex(hexSuffix) {
		return "", false
	}
	return hexSuffix, true
}

func validHex(x string) bool {
	if len(x) < 4 || len(x) > 1000 || len(x)%2 == 1 {
		return false
	}
	for i := range x {
		b := x[i]
		if b >= '0' && b <= '9' || b >= 'a' && b <= 'f' {
			continue
		}
		return false
	}
	return true
}

func (s *goCacheServer) handleGetAction(w http.ResponseWriter, r *http.Request) {
	actionID, ok := getHexSuffix(r, "/action/")
	if !ok {
		metricGoCacheErr.Add("action_suffix", 1)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	outputID, diskPath, err := s.cache.Get(ctx, actionID)
	if err != nil {
		metricGoCacheErr.Add("action_get", 1)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if outputID == "" {
		metricGoCacheErr.Add("action_empty_outputid", 1)
		http.Error(w, "not found ()", http.StatusNotFound)
		return
	}
	fi, err := os.Stat(diskPath)
	if err != nil {
		if os.IsNotExist(err) {
			metricActionGetMiss.Add(1)
			http.Error(w, "not found (post-stat)", http.StatusNotFound)
			return
		}
		metricGoCacheErr.Add("action_state", 1)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	metricActionGetHit.Add(1)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(&cachers.ActionValue{
		OutputID: outputID,
		Size:     fi.Size(),
	})
}

func (s *goCacheServer) handleGetOutput(w http.ResponseWriter, r *http.Request) {
	outputID, ok := getHexSuffix(r, "/output/")
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	filename := s.cache.OutputFilename(outputID)
	if fi, err := os.Stat(filename); err != nil {
		if os.IsNotExist(err) {
			metricActionOutputMiss.Add(1)
		} else {
			metricGoCacheErr.Add("output_stat_err", 1)
		}
	} else if fi.Mode().IsRegular() {
		metricActionOuputHit.Add(1)
		metricActionOuputHitBytes.Add(fi.Size())
	} else {
		metricGoCacheErr.Add("output_stat_nonreg", 1)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, filename)
}

func (s *goCacheServer) handlePut(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != "PUT" {
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
		return
	}
	actionID, outputID, ok := strings.Cut(r.RequestURI[len("/"):], "/")
	if !ok || !validHex(actionID) || !validHex(outputID) {
		metricGoCacheErr.Add("put_bad_uri", 1)
		http.Error(w, "bad URI", http.StatusBadRequest)
		return
	}
	if r.ContentLength == -1 {
		metricGoCacheErr.Add("put_content_len", 1)
		http.Error(w, "missing Content-Length", http.StatusBadRequest)
		return
	}
	_, err := s.cache.Put(ctx, actionID, outputID, r.ContentLength, r.Body)
	if err != nil {
		metricGoCacheErr.Add("put_err", 1)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	metricPut.Add(1)
	w.WriteHeader(http.StatusNoContent)
}
