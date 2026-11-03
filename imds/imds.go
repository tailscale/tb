// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package imds is the instance metadata service a guest reaches at
// http://169.254.169.254/. Each guest gets its own instance, so the caller's
// identity is implicit and no tokens are needed for reads.
//
// Endpoints:
//
//	GET  /                      index
//	GET  /v0/meta               JSON [Meta]
//	GET  /v0/user-data          JSON object of all user data
//	GET  /v0/user-data/{key}    raw value
//	GET  /v0/time               RFC 3339 host time
//	POST /v0/ready              guest says it is usable
//	POST /v0/log                append body as a log line
package imds

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Meta is what the guest learns about itself.
type Meta struct {
	Name        string    `json:"name"`
	Slot        int       `json:"slot"`
	Image       string    `json:"image"`
	CPUs        int       `json:"cpus"`
	MemoryBytes uint64    `json:"memory_bytes"`
	BootMode    string    `json:"boot_mode"` // "cold" or "restored"
	BootTime    time.Time `json:"boot_time"`
	Host        string    `json:"host"`
	GuestIP     string    `json:"guest_ip"`
	GatewayIP   string    `json:"gateway_ip"`
}

// Handler serves the metadata API for one guest.
type Handler struct {
	mu       sync.Mutex
	meta     Meta
	userData map[string]string

	ready     chan struct{}
	readyOnce sync.Once

	// OnLog receives lines the guest posts to /v0/log. Optional.
	OnLog func(line string)
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time

	mux *http.ServeMux
}

// New returns a Handler serving meta and userData.
func New(meta Meta, userData map[string]string) *Handler {
	h := &Handler{
		meta:     meta,
		userData: make(map[string]string, len(userData)),
		ready:    make(chan struct{}),
		Now:      time.Now,
		mux:      http.NewServeMux(),
	}
	for k, v := range userData {
		h.userData[k] = v
	}
	h.mux.HandleFunc("GET /{$}", h.serveIndex)
	h.mux.HandleFunc("GET /v0/meta", h.serveMeta)
	h.mux.HandleFunc("GET /v0/user-data", h.serveUserDataAll)
	h.mux.HandleFunc("GET /v0/user-data/{key}", h.serveUserData)
	h.mux.HandleFunc("GET /v0/time", h.serveTime)
	h.mux.HandleFunc("POST /v0/ready", h.serveReady)
	h.mux.HandleFunc("POST /v0/log", h.serveLog)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", "mantsana-imds")
	h.mux.ServeHTTP(w, r)
}

// Ready returns a channel closed when the guest posts to /v0/ready.
func (h *Handler) Ready() <-chan struct{} { return h.ready }

// SetMeta updates the metadata (for example the boot mode once known).
func (h *Handler) SetMeta(f func(*Meta)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(&h.meta)
}

// SetUserData sets or (with an empty value) removes a user-data key.
func (h *Handler) SetUserData(key, value string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if value == "" {
		delete(h.userData, key)
	} else {
		h.userData[key] = value
	}
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "mantsana instance metadata\n\nv0/meta\nv0/user-data\nv0/user-data/{key}\nv0/time\nv0/ready (POST)\nv0/log (POST)\n")
}

func (h *Handler) serveMeta(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	m := h.meta
	h.mu.Unlock()
	writeJSON(w, m)
}

func (h *Handler) serveUserDataAll(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	out := make(map[string]string, len(h.userData))
	for k, v := range h.userData {
		out[k] = v
	}
	h.mu.Unlock()
	writeJSON(w, out)
}

func (h *Handler) serveUserData(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	h.mu.Lock()
	v, ok := h.userData[key]
	h.mu.Unlock()
	if !ok {
		http.Error(w, "no such key", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	io.WriteString(w, v)
}

func (h *Handler) serveTime(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, h.Now().UTC().Format(time.RFC3339Nano))
}

func (h *Handler) serveReady(w http.ResponseWriter, r *http.Request) {
	h.readyOnce.Do(func() { close(h.ready) })
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) serveLog(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.OnLog != nil {
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			h.OnLog(line)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// Keys returns the user-data keys, sorted.
func (h *Handler) Keys() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]string, 0, len(h.userData))
	for k := range h.userData {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
