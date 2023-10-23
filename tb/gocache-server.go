package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bradfitz/go-tool-cache/cachers"
)

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
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	outputID, diskPath, err := s.cache.Get(ctx, actionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if outputID == "" {
		http.Error(w, "not found ()", http.StatusNotFound)
		return
	}
	fi, err := os.Stat(diskPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found (post-stat)", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
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
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, s.cache.OutputFilename(outputID))
}

func (s *goCacheServer) handlePut(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != "PUT" {
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
		return
	}
	actionID, outputID, ok := strings.Cut(r.RequestURI[len("/"):], "/")
	if !ok || !validHex(actionID) || !validHex(outputID) {
		http.Error(w, "bad URI", http.StatusBadRequest)
		return
	}
	if r.ContentLength == -1 {
		http.Error(w, "missing Content-Length", http.StatusBadRequest)
		return
	}
	_, err := s.cache.Put(ctx, actionID, outputID, r.ContentLength, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
