// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package lansport gives a set of tailnet peers on one LAN a way to talk to
// each other directly over TLS, without pushing their bulk traffic through
// WireGuard, while still getting their trust from the tailnet.
//
// Each participating process runs a [Server]. It has a per-process TLS
// [Identity] and two listeners. The advert listener speaks plain HTTP and
// answers only connections that arrived through the tailnet (or from a
// statically trusted host); at [AdvertPath] it returns the process's
// [Advert]: the LAN address and port of its TLS listener, the [Pin] of its
// certificate, and a routing weight. The TLS listener serves the caller's
// handler to peers that present a verified peer's client certificate.
//
// A Server learns which nodes might be peers from a [Source], typically a
// tagpeers.Tracker through [TailnetSource]. Every few seconds it fetches each
// candidate's advert over the tailnet; when a candidate's pin or address is
// new or changed, it builds a fresh pinned *http.Transport to the LAN
// address, and it checks that the LAN path works through that transport. A
// peer is reachable when both steps succeeded on the last round. A
// restarted peer shows up as a new pin; a broken LAN path shows up in the
// LAN check.
//
// Callers get reachable peers from [Server.Peers], each with a Transport
// that always connects to that peer's LAN address regardless of the request
// URL's host, follow changes to that set with [Server.Subscribe], and learn
// which peer sent a request with [FromPeer]. Choosing which peer should
// handle a given key is left to the rendezvous package.
package lansport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/net/netx"
	"tailscale.com/net/tsaddr"
	"tailscale.com/types/logger"
)

// AdvertPath is the path at which a [Server] serves its [Advert], on both
// the advert listener (to tailnet-origin connections) and the TLS listener
// (to verified peers, as a LAN liveness check).
const AdvertPath = "/lansport"

// defaultProbeInterval is how often each candidate's advert is fetched and
// its LAN path checked when [Config.ProbeInterval] is zero.
const defaultProbeInterval = 3 * time.Second

// probeTimeout bounds one advert fetch or LAN check.
const probeTimeout = 3 * time.Second

// Advert is what a [Server] publishes about itself at [AdvertPath].
type Advert struct {
	// Name is the process's display name.
	Name string `json:"name"`

	// IPPort is the LAN address and port of the process's TLS listener.
	IPPort string `json:"ipPort"`

	// TLSCertHash is the hex [Pin] of the certificate that listener
	// presents.
	TLSCertHash string `json:"tlsCertHash"`

	// Weight is the process's relative share of whatever peers divide
	// among themselves, for the rendezvous package. 1 is normal.
	Weight float64 `json:"weight"`
}

// Peer is a reachable peer: something a caller can send requests to.
type Peer struct {
	// Key is the peer's routing [Key]: its [Candidate.Key], or its
	// advertised name if the candidate had none.
	Key Key

	// Name is the peer's display name, from its advert.
	Name string

	// Addr is the peer's LAN TLS address, from its advert.
	Addr string

	// Weight is the peer's advertised routing weight.
	Weight float64

	// Transport makes HTTPS requests to the peer, pinned to its
	// certificate and presenting ours. It connects to Addr regardless of
	// the host in the request URL; use [Peer.URL] to build one.
	Transport *http.Transport
}

// URL returns an https URL for path on the peer.
func (p Peer) URL(path string) string {
	return "https://" + p.Addr + path
}

// Config configures a [Server].
type Config struct {
	// Source says which nodes might be peers. Required.
	Source Source

	// Name is the advertised display name. If empty, the hostname is used.
	Name string

	// Weight is the advertised routing weight. If zero, 1 is used.
	Weight float64

	// AdvertAddr is the listen address of the plain-HTTP advert server,
	// such as ":7890". Peers must be told the same port. Required unless
	// AdvertListener is set.
	AdvertAddr     string
	AdvertListener net.Listener

	// TLSAddr is the listen address of the LAN TLS server, such as
	// ":31367" or ":0"; peers learn the actual port from the advert.
	// Required unless TLSListener is set.
	TLSAddr     string
	TLSListener net.Listener

	// LANIP is the IPv4 address advertised for the TLS listener. If unset,
	// the address of the interface carrying the default route is used.
	LANIP netip.Addr

	// Handler serves requests from verified peers on the TLS listener.
	// Required.
	Handler http.Handler

	// Dial, if non-nil, replaces how adverts are fetched and how peers'
	// LAN addresses are connected to. The real network is used otherwise.
	// Tests use it, with the listeners above, to run several servers on an
	// in-memory network; see the lansporttest package.
	Dial netx.DialFunc

	// ResponseHeaderTimeout bounds how long requests through a peer's
	// Transport wait for response headers. If zero, 30 seconds.
	ResponseHeaderTimeout time.Duration

	// ProbeInterval is how often adverts are fetched and LAN paths
	// checked. If zero, a few seconds.
	ProbeInterval time.Duration

	// Logf receives log lines; nil discards them.
	Logf logger.Logf
}

// PeerState is everything a [Server] knows about one candidate, for
// debugging pages.
type PeerState struct {
	Candidate  Candidate
	Advert     *Advert // last successfully fetched; nil if none
	Pin        Pin     // zero until an advert has been fetched
	Reachable  bool    // advert fetched and LAN check passed on the last round
	Since      time.Time
	LastAdvert time.Time // last successful advert fetch
	AdvertErr  string    // from the last failed advert fetch; empty on success
	LastLAN    time.Time // last successful LAN check
	LANRTT     time.Duration
	LANErr     string // from the last failed LAN check; empty on success
	Rebuilds   int    // transports built for this peer (pin or address changes)
}

// Key returns the routing key the peer has, or will have once its advert is
// fetched: the candidate's key, else the advertised name, else empty.
func (st PeerState) Key() Key {
	if st.Candidate.Key != "" {
		return st.Candidate.Key
	}
	if st.Advert != nil {
		return Key(st.Advert.Name)
	}
	return ""
}

// Name returns the peer's advertised name, or the candidate's until then.
func (st PeerState) Name() string {
	if st.Advert != nil {
		return st.Advert.Name
	}
	return st.Candidate.Name
}

// peerState is a candidate's state plus its transport. The copies in
// Server.peers are guarded by Server.mu; probe works on a copy of its own.
type peerState struct {
	PeerState
	transport *http.Transport
}

// Server publishes this process's advert, serves peers over LAN TLS, and
// maintains pinned transports to reachable peers. Use [Listen].
type Server struct {
	cfg         Config
	logf        logger.Logf
	identity    *Identity
	advertLn    net.Listener
	tlsLn       net.Listener
	advertSrv   *http.Server
	tlsSrv      *http.Server
	advert      Advert
	self        Peer // Key, Name, Addr, Weight of this process
	advertJSON  []byte
	probeClient *http.Client

	// reachable is the current set of reachable peers by key, replaced
	// wholesale after each probe round. subs are the channels handed out
	// by Subscribe, each signaled when it changes.
	reachable atomic.Pointer[map[Key]Peer]
	subsMu    sync.Mutex
	subs      []chan struct{}

	mu    sync.Mutex
	peers map[string]*peerState // by Candidate.AdvertAddr

	cancel context.CancelFunc
	done   chan struct{}
}

// peerCtxKey marks request contexts on the TLS listener with the sending
// peer.
type peerCtxKey struct{}

// FromPeer returns the verified peer that sent r, if r arrived on a
// [Server]'s TLS listener.
func FromPeer(r *http.Request) (Peer, bool) {
	p, ok := r.Context().Value(peerCtxKey{}).(Peer)
	return p, ok
}

// Listen starts a Server: it opens both listeners, publishes the advert,
// and begins probing the source's candidates.
func Listen(cfg Config) (*Server, error) {
	if cfg.Source == nil {
		return nil, errors.New("lansport: Source is required")
	}
	if cfg.Handler == nil {
		return nil, errors.New("lansport: Handler is required")
	}
	if cfg.Logf == nil {
		cfg.Logf = logger.Discard
	}
	if cfg.Name == "" {
		h, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("lansport: Name not set and hostname unavailable: %w", err)
		}
		cfg.Name = h
	}
	if cfg.Weight == 0 {
		cfg.Weight = 1
	}
	if cfg.ResponseHeaderTimeout == 0 {
		cfg.ResponseHeaderTimeout = 30 * time.Second
	}
	if cfg.ProbeInterval == 0 {
		cfg.ProbeInterval = defaultProbeInterval
	}
	if cfg.Dial == nil {
		cfg.Dial = (&net.Dialer{Timeout: probeTimeout, KeepAlive: 30 * time.Second}).DialContext
	}
	identity, err := NewIdentity(cfg.Name)
	if err != nil {
		return nil, fmt.Errorf("lansport: generating identity: %w", err)
	}

	advertLn := cfg.AdvertListener
	if advertLn == nil {
		if cfg.AdvertAddr == "" {
			return nil, errors.New("lansport: AdvertAddr or AdvertListener is required")
		}
		advertLn, err = net.Listen("tcp", cfg.AdvertAddr)
		if err != nil {
			return nil, fmt.Errorf("lansport: advert listener: %w", err)
		}
	}
	tlsLn := cfg.TLSListener
	if tlsLn == nil {
		if cfg.TLSAddr == "" {
			advertLn.Close()
			return nil, errors.New("lansport: TLSAddr or TLSListener is required")
		}
		tlsLn, err = net.Listen("tcp", cfg.TLSAddr)
		if err != nil {
			advertLn.Close()
			return nil, fmt.Errorf("lansport: TLS listener: %w", err)
		}
	}
	lanIP := cfg.LANIP
	if !lanIP.IsValid() {
		lanIP, err = defaultLANIP()
		if err != nil {
			advertLn.Close()
			tlsLn.Close()
			return nil, fmt.Errorf("lansport: choosing LAN address to advertise: %w", err)
		}
	}
	tlsPort := tlsLn.Addr().(*net.TCPAddr).Port
	s := &Server{
		cfg:      cfg,
		logf:     cfg.Logf,
		identity: identity,
		advertLn: advertLn,
		tlsLn:    tlsLn,
		advert: Advert{
			Name:        cfg.Name,
			IPPort:      netip.AddrPortFrom(lanIP, uint16(tlsPort)).String(),
			TLSCertHash: identity.Pin().String(),
			Weight:      cfg.Weight,
		},
		probeClient: &http.Client{
			Transport: &http.Transport{
				DialContext:           cfg.Dial,
				ResponseHeaderTimeout: probeTimeout,
				MaxIdleConnsPerHost:   1,
				IdleConnTimeout:       time.Minute,
			},
		},
		peers: make(map[string]*peerState),
		done:  make(chan struct{}),
	}
	s.advertJSON, _ = json.Marshal(s.advert)
	s.self = Peer{Name: cfg.Name, Addr: s.advert.IPPort, Weight: cfg.Weight}
	if key, ok := cfg.Source.SelfKey(); ok {
		s.self.Key = key
	}
	empty := map[Key]Peer{}
	s.reachable.Store(&empty)

	s.advertSrv = &http.Server{
		Handler:           http.HandlerFunc(s.serveAdvert),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          logger.StdLogger(cfg.Logf),
	}
	s.tlsSrv = &http.Server{
		Handler:           http.HandlerFunc(s.serveTLS),
		TLSConfig:         identity.ServerTLSConfig(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          logger.StdLogger(cfg.Logf),
	}
	go s.advertSrv.Serve(advertLn)
	go s.tlsSrv.ServeTLS(tlsLn, "", "")

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go s.run(ctx)
	s.logf("lansport: %q advertising %s (pin %s) on %v; TLS for peers on %v",
		cfg.Name, s.advert.IPPort, identity.Pin().Short(), advertLn.Addr(), tlsLn.Addr())
	return s, nil
}

// Close stops probing and both listeners.
func (s *Server) Close() error {
	s.cancel()
	<-s.done
	err := errors.Join(s.advertSrv.Close(), s.tlsSrv.Close())
	s.probeClient.CloseIdleConnections()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.peers {
		if p.transport != nil {
			p.transport.CloseIdleConnections()
		}
	}
	return err
}

// Advert returns this process's advert.
func (s *Server) Advert() Advert { return s.advert }

// Identity returns this process's TLS identity.
func (s *Server) Identity() *Identity { return s.identity }

// Self describes this process as a Peer: its Key (if the source knows it),
// Name, Addr, and Weight. Its Transport is nil.
func (s *Server) Self() Peer {
	if s.self.Key == "" {
		if key, ok := s.cfg.Source.SelfKey(); ok {
			s.self.Key = key
		}
	}
	return s.self
}

// TLSAddr returns the address of the LAN TLS listener.
func (s *Server) TLSAddr() net.Addr { return s.tlsLn.Addr() }

// AdvertAddr returns the address of the advert listener.
func (s *Server) AdvertAddr() net.Addr { return s.advertLn.Addr() }

// Peers returns the currently reachable peers by [Key]. The map must not be
// modified.
func (s *Server) Peers() map[Key]Peer {
	return *s.reachable.Load()
}

// Subscribe returns a channel that receives a value when the reachable
// set, or a reachable peer's Transport or Weight, changes. Signals are
// coalesced: a slow receiver sees one signal for many changes and should
// re-read [Server.Peers]. Each call returns its own channel, so several
// parties can follow the server.
func (s *Server) Subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	s.subsMu.Lock()
	s.subs = append(s.subs, ch)
	s.subsMu.Unlock()
	return ch
}

// Snapshot returns the state of every candidate, for debugging pages.
func (s *Server) Snapshot() []PeerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PeerState, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, p.PeerState)
	}
	return out
}

// serveAdvert handles the plain-HTTP advert listener.
func (s *Server) serveAdvert(w http.ResponseWriter, r *http.Request) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || !s.cfg.Source.TrustedAddr(ap.Addr()) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.URL.Path != AdvertPath {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(s.advertJSON)
}

// serveTLS handles the LAN TLS listener: the advert for verified peers'
// liveness checks, and the caller's handler for everything else they send.
func (s *Server) serveTLS(w http.ResponseWriter, r *http.Request) {
	pin, ok := RequestPin(r)
	if !ok {
		http.Error(w, "forbidden: no client certificate", http.StatusForbidden)
		return
	}
	peer, ok := s.peerByPin(pin)
	if !ok {
		http.Error(w, "forbidden: unknown peer certificate", http.StatusForbidden)
		return
	}
	if r.URL.Path == AdvertPath {
		w.Header().Set("Content-Type", "application/json")
		w.Write(s.advertJSON)
		return
	}
	s.cfg.Handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerCtxKey{}, peer)))
}

// peerByPin returns the peer whose last fetched advert declared pin, if
// any. A peer counts here as soon as its advert has been fetched, whether
// or not its LAN path has been checked yet, so that a peer which learned of
// us first can reach us.
func (s *Server) peerByPin(pin Pin) (Peer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.peers {
		if p.Advert != nil && p.Pin == pin {
			return p.peer(), true
		}
	}
	return Peer{}, false
}

func (p *peerState) peer() Peer {
	return Peer{
		Key:       p.Key(),
		Name:      p.Advert.Name,
		Addr:      p.Advert.IPPort,
		Weight:    p.Advert.Weight,
		Transport: p.transport,
	}
}

// run probes every candidate each ProbeInterval, and sooner when the source
// reports a change.
func (s *Server) run(ctx context.Context) {
	defer close(s.done)
	t := time.NewTicker(s.cfg.ProbeInterval)
	defer t.Stop()
	for {
		s.probeAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.cfg.Source.Changes():
		}
	}
}

// probeAll reconciles the candidate list with the source, probes every
// online candidate concurrently, and publishes the new reachable set. It is
// the only place peer state is mutated: probe works on a copy and hands
// back the result, which is applied here under mu.
func (s *Server) probeAll(ctx context.Context) {
	cands := s.cfg.Source.Candidates()
	selfKey, _ := s.cfg.Source.SelfKey()

	s.mu.Lock()
	seen := make(map[string]bool)
	var toProbe []peerState
	for _, c := range cands {
		if c.Key != "" && c.Key == selfKey {
			continue
		}
		seen[c.AdvertAddr] = true
		p, ok := s.peers[c.AdvertAddr]
		if !ok {
			p = &peerState{PeerState: PeerState{Candidate: c}}
			s.peers[c.AdvertAddr] = p
			s.logf("lansport: candidate %s at %s", c.Name, c.AdvertAddr)
		}
		p.Candidate = c
		if c.Online {
			toProbe = append(toProbe, *p)
		} else {
			p.Reachable = false
			p.AdvertErr = "offline per source"
		}
	}
	for addr, p := range s.peers {
		if !seen[addr] {
			s.logf("lansport: %s is no longer a candidate", p.Name())
			if p.transport != nil {
				p.transport.CloseIdleConnections()
			}
			delete(s.peers, addr)
		}
	}
	s.mu.Unlock()

	results := make([]peerState, len(toProbe))
	var wg sync.WaitGroup
	for i, p := range toProbe {
		wg.Go(func() { results[i] = s.probe(ctx, p) })
	}
	wg.Wait()

	s.mu.Lock()
	for _, res := range results {
		p, ok := s.peers[res.Candidate.AdvertAddr]
		if !ok {
			// Removed by a concurrent round; nothing to update.
			if res.transport != nil {
				res.transport.CloseIdleConnections()
			}
			continue
		}
		if p.transport != nil && p.transport != res.transport {
			p.transport.CloseIdleConnections()
		}
		*p = res
	}
	s.mu.Unlock()
	s.publish()
}

// probe fetches p's advert over the source's trusted path, rebuilds its
// transport if the pin or address changed, and checks the LAN path. It works
// on its copy of p and returns the updated state; it touches no shared
// state, so it needs no lock.
func (s *Server) probe(ctx context.Context, p peerState) peerState {
	name := p.Name()
	adv, err := s.fetchAdvert(ctx, p.Candidate.AdvertAddr)
	now := time.Now()
	if err != nil {
		if p.Reachable {
			s.logf("lansport: %s: fetching advert from %s: %v", name, p.Candidate.AdvertAddr, err)
		}
		p.Reachable = false
		p.AdvertErr = err.Error()
		return p
	}
	pin, err := ParsePin(adv.TLSCertHash)
	if err != nil {
		p.Reachable = false
		p.AdvertErr = "bad tlsCertHash in advert: " + err.Error()
		return p
	}
	if pin == s.identity.Pin() {
		// The candidate list included this process (a static list shared
		// by every member, say).
		p.Reachable = false
		p.AdvertErr = "that's me"
		return p
	}
	p.AdvertErr = ""
	p.LastAdvert = now
	if p.Advert == nil || p.Pin != pin || p.Advert.IPPort != adv.IPPort {
		if p.transport != nil {
			s.logf("lansport: %s changed: now %s with pin %s", adv.Name, adv.IPPort, pin.Short())
		} else {
			s.logf("lansport: %s is %s with pin %s", adv.Name, adv.IPPort, pin.Short())
		}
		p.transport = s.newTransport(adv.IPPort, pin)
		p.Rebuilds++
		p.Reachable = false
	}
	p.Advert = adv
	p.Pin = pin

	start := time.Now()
	err = s.checkLAN(ctx, p.transport, adv)
	rtt := time.Since(start)
	if err != nil {
		if p.Reachable {
			s.logf("lansport: %s: LAN check to %s failed: %v", adv.Name, adv.IPPort, err)
		}
		p.Reachable = false
		p.LANErr = err.Error()
		return p
	}
	if !p.Reachable {
		s.logf("lansport: %s reachable at %s (%v)", adv.Name, adv.IPPort, rtt.Round(100*time.Microsecond))
		p.Since = now
	}
	p.Reachable = true
	p.LastLAN = now
	p.LANRTT = rtt
	p.LANErr = ""
	return p
}

// fetchAdvert GETs a candidate's advert with plain HTTP.
func (s *Server) fetchAdvert(ctx context.Context, addr string) (*Advert, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+addr+AdvertPath, nil)
	if err != nil {
		return nil, err
	}
	res, err := s.probeClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", res.Status)
	}
	adv := new(Advert)
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(adv); err != nil {
		return nil, fmt.Errorf("decoding advert: %w", err)
	}
	if _, err := netip.ParseAddrPort(adv.IPPort); err != nil {
		return nil, fmt.Errorf("bad ipPort %q in advert", adv.IPPort)
	}
	if adv.Weight <= 0 {
		return nil, fmt.Errorf("bad weight %v in advert", adv.Weight)
	}
	return adv, nil
}

// checkLAN fetches the peer's advert over the LAN through its pinned
// transport and checks it agrees with the one fetched over the tailnet.
func (s *Server) checkLAN(ctx context.Context, transport *http.Transport, want *Advert) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://"+want.IPPort+AdvertPath, nil)
	if err != nil {
		return err
	}
	res, err := transport.RoundTrip(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		// Most likely the peer hasn't fetched our advert yet and so
		// doesn't know our certificate; it will within its next round.
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return fmt.Errorf("status %s: %s", res.Status, msg)
	}
	var got Advert
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&got); err != nil {
		return fmt.Errorf("decoding advert over LAN: %w", err)
	}
	if got.TLSCertHash != want.TLSCertHash {
		return fmt.Errorf("LAN address %s answered with pin %s, expected %s", want.IPPort, got.TLSCertHash, want.TLSCertHash)
	}
	return nil
}

// newTransport returns a transport that connects to ipPort, pinned to pin,
// presenting our identity.
func (s *Server) newTransport(ipPort string, pin Pin) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return s.cfg.Dial(ctx, "tcp", ipPort)
		},
		TLSClientConfig:       s.identity.ClientTLSConfig(&pin),
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: s.cfg.ResponseHeaderTimeout,
		// Peers negotiate their own encodings; don't add gzip on top.
		DisableCompression: true,
	}
}

// publish recomputes the reachable set and signals if it differs from the
// last one. Two reachable peers with the same routing key is a
// misconfiguration (two static peers advertising the same name); neither
// is published, and both say why.
func (s *Server) publish() {
	s.mu.Lock()
	next := make(map[Key]Peer)
	byKey := make(map[Key][]*peerState)
	for _, p := range s.peers {
		if p.Reachable {
			byKey[p.Key()] = append(byKey[p.Key()], p)
		}
	}
	for key, ps := range byKey {
		if len(ps) > 1 {
			for _, p := range ps {
				p.Reachable = false
				p.LANErr = fmt.Sprintf("routing key %q is shared by %d peers; names must be unique", key, len(ps))
			}
			continue
		}
		next[key] = ps[0].peer()
	}
	s.mu.Unlock()
	prev := *s.reachable.Load()
	if maps.EqualFunc(prev, next, func(a, b Peer) bool {
		return a.Name == b.Name && a.Addr == b.Addr && a.Weight == b.Weight && a.Transport == b.Transport
	}) {
		return
	}
	s.reachable.Store(&next)
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	for _, ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// defaultLANIP returns the IPv4 address of the interface carrying the
// default route, found by asking the kernel which source address it would
// use for an outbound packet; nothing is sent. Tailnet addresses are
// rejected, since the point is to bypass the tailnet.
func defaultLANIP() (netip.Addr, error) {
	c, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1; never actually contacted
	if err != nil {
		return netip.Addr{}, err
	}
	defer c.Close()
	ip := c.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
	if !ip.Is4() || ip.IsLoopback() || tsaddr.IsTailscaleIP(ip) {
		return netip.Addr{}, fmt.Errorf("default route uses %v; set Config.LANIP", ip)
	}
	return ip, nil
}
