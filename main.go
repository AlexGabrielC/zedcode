// zedcode — partage de code temps réel, façon codeshare, ultra léger.
//
// Une salle = code.zedxp.com/<6 caractères [A-Za-z0-9]>. Tout vit en RAM :
// rien n'est écrit sur disque, donc rien ne finit dans une sauvegarde.
// Une salle disparaît après ROOM_TTL sans activité.
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

var (
	roomRe   = regexp.MustCompile(`^[A-Za-z0-9]{6}$`)
	clientRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

	errFull     = errors.New("quota mémoire atteint")
	errTooMany  = errors.New("trop de salles ou de connexions")
	errNotFound = errors.New("salle inconnue")
)

// ---------------------------------------------------------------- config

type config struct {
	addr          string
	roomTTL       time.Duration
	maxDoc        int
	maxTotal      int
	maxRooms      int
	maxSubs       int
	joinPerMinute int
	trustProxy    bool
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) (int, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s: entier positif attendu", k)
	}
	return n, nil
}

func loadConfig() (config, error) {
	c := config{
		addr:       envOr("ADDR", ":8080"),
		trustProxy: os.Getenv("TRUST_PROXY") == "1",
	}
	var err error
	if c.roomTTL, err = time.ParseDuration(envOr("ROOM_TTL", "2h")); err != nil {
		return c, fmt.Errorf("ROOM_TTL: %w", err)
	}
	for _, x := range []struct {
		k   string
		dst *int
		def int
	}{
		{"MAX_DOC_BYTES", &c.maxDoc, 1 << 20},
		{"MAX_TOTAL_BYTES", &c.maxTotal, 32 << 20},
		{"MAX_ROOMS", &c.maxRooms, 500},
		{"MAX_CONNECTIONS", &c.maxSubs, 200},
		{"JOIN_PER_MINUTE", &c.joinPerMinute, 30},
	} {
		if *x.dst, err = envInt(x.k, x.def); err != nil {
			return c, err
		}
	}
	return c, nil
}

// ----------------------------------------------------------------- store

type event struct {
	V uint64 `json:"v"`
	T string `json:"t"`
	F string `json:"f,omitempty"`
}

type sub struct {
	doc      chan event
	presence chan int
}

type room struct {
	ev      event
	updated time.Time
	subs    map[*sub]struct{}
}

type store struct {
	cfg   config
	mu    sync.Mutex
	rooms map[string]*room
	total int
	nsubs int
}

func newStore(cfg config) *store {
	return &store{cfg: cfg, rooms: map[string]*room{}}
}

// offer envoie v sans bloquer ; si le canal est plein, la valeur en attente
// est remplacée (seul l'état le plus récent compte).
func offer[T any](ch chan T, v T) {
	select {
	case ch <- v:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- v:
	default:
	}
}

func (s *store) exists(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.rooms[id]
	return ok
}

func (s *store) presenceLocked(r *room) {
	for sb := range r.subs {
		offer(sb.presence, len(r.subs))
	}
}

// subscribe ouvre (et crée au besoin) une salle. C'est la seule façon de
// créer une salle : un PUT sur une salle inconnue est refusé, ce qui force
// tout balayage de codes à passer par le limiteur de débit.
func (s *store) subscribe(id string) (*sub, event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nsubs >= s.cfg.maxSubs {
		return nil, event{}, errTooMany
	}
	r, ok := s.rooms[id]
	if !ok {
		if len(s.rooms) >= s.cfg.maxRooms {
			return nil, event{}, errTooMany
		}
		r = &room{updated: time.Now(), subs: map[*sub]struct{}{}}
		s.rooms[id] = r
	}
	sb := &sub{doc: make(chan event, 1), presence: make(chan int, 1)}
	r.subs[sb] = struct{}{}
	s.nsubs++
	s.presenceLocked(r)
	return sb, r.ev, nil
}

func (s *store) unsubscribe(id string, sb *sub) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rooms[id]
	if !ok {
		return
	}
	delete(r.subs, sb)
	s.nsubs--
	if len(r.subs) == 0 && r.ev.V == 0 {
		delete(s.rooms, id) // salle jamais écrite : rien à garder
		return
	}
	r.updated = time.Now() // le départ du dernier appareil compte comme activité
	s.presenceLocked(r)
}

func (s *store) put(id, text, from string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rooms[id]
	if !ok {
		return 0, errNotFound
	}
	if s.total-len(r.ev.T)+len(text) > s.cfg.maxTotal {
		return 0, errFull
	}
	s.total += len(text) - len(r.ev.T)
	r.ev = event{V: r.ev.V + 1, T: text, F: from}
	r.updated = time.Now()
	for sb := range r.subs {
		offer(sb.doc, r.ev)
	}
	return r.ev.V, nil
}

func (s *store) expire(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, r := range s.rooms {
		if len(r.subs) == 0 && now.Sub(r.updated) > s.cfg.roomTTL {
			s.total -= len(r.ev.T)
			delete(s.rooms, id)
		}
	}
}

// --------------------------------------------------------------- limiter

// limiter : seau à jetons par IP, pour l'ouverture de salles.
type limiter struct {
	mu      sync.Mutex
	rate    float64 // jetons par seconde
	burst   float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(perMinute int) *limiter {
	return &limiter{
		rate:    float64(perMinute) / 60,
		burst:   float64(perMinute),
		buckets: map[string]*bucket{},
	}
}

func (l *limiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok {
		if len(l.buckets) >= 10000 {
			l.buckets = map[string]*bucket{} // borne la mémoire
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *limiter) prune(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, b := range l.buckets {
		if now.Sub(b.last) > 10*time.Minute {
			delete(l.buckets, ip)
		}
	}
}

// ---------------------------------------------------------------- server

type server struct {
	cfg   config
	st    *store
	lim   *limiter
	index []byte
}

// clientIP : derrière le proxy de Coolify (Traefik/Caddy), l'IP réelle est
// la dernière entrée de X-Forwarded-For, ajoutée par le proxy lui-même.
func (s *server) clientIP(r *http.Request) string {
	if s.cfg.trustProxy {
		if vals := r.Header.Values("X-Forwarded-For"); len(vals) > 0 {
			parts := strings.Split(vals[len(vals)-1], ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func genCode() string {
	b := make([]byte, 6)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			panic(err)
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
}

func isJSON(r *http.Request) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return mt == "application/json"
}

func (s *server) handleNew(w http.ResponseWriter, r *http.Request) {
	code := genCode()
	for i := 0; i < 10 && s.st.exists(code); i++ {
		code = genCode()
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/"+code, http.StatusFound)
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !roomRe.MatchString(r.PathValue("id")) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(s.index)
}

func writeSSE(w http.ResponseWriter, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	return err
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !roomRe.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	if !s.lim.allow(s.clientIP(r), time.Now()) {
		http.Error(w, "trop de tentatives, réessaie dans une minute", http.StatusTooManyRequests)
		return
	}
	sb, snap, err := s.st.subscribe(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer s.st.unsubscribe(id, sb)

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // flux long : pas de WriteTimeout
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if err := writeSSE(w, "doc", snap); err != nil {
		return
	}
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		if err := rc.Flush(); err != nil {
			return
		}
		var err error
		select {
		case <-r.Context().Done():
			return
		case ev := <-sb.doc:
			err = writeSSE(w, "doc", ev)
		case n := <-sb.presence:
			err = writeSSE(w, "presence", map[string]int{"n": n})
		case <-ping.C:
			_, err = fmt.Fprint(w, ": ping\n\n")
		}
		if err != nil {
			return
		}
	}
}

func (s *server) handlePut(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !roomRe.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	if !isJSON(r) {
		http.Error(w, "application/json requis", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.cfg.maxDoc)*2+1024) // marge pour l'échappement JSON
	var body struct {
		T string `json:"t"`
		F string `json:"f"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "document trop gros", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "requête invalide", http.StatusBadRequest)
		return
	}
	if len(body.T) > s.cfg.maxDoc {
		http.Error(w, "document trop gros", http.StatusRequestEntityTooLarge)
		return
	}
	if !clientRe.MatchString(body.F) {
		http.Error(w, "identifiant client invalide", http.StatusBadRequest)
		return
	}
	v, err := s.st.put(id, body.T, body.F)
	switch {
	case errors.Is(err, errNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case errors.Is(err, errFull):
		http.Error(w, err.Error(), http.StatusInsufficientStorage)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]uint64{"v": v})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Robots-Tag", "noindex, nofollow")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		next.ServeHTTP(w, r)
	})
}

func (s *server) routes() (http.Handler, error) {
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, err
	}
	if s.index, err = fs.ReadFile(static, "index.html"); err != nil {
		return nil, err
	}
	files := http.StripPrefix("/static/", http.FileServerFS(static))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleNew)
	mux.HandleFunc("GET /{id}", s.handleIndex)
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/rooms/{id}/events", s.handleEvents)
	mux.HandleFunc("PUT /api/rooms/{id}", s.handlePut)
	return securityHeaders(mux), nil
}

// ------------------------------------------------------------------ main

// healthcheck sert au HEALTHCHECK Docker (l'image scratch n'a pas de curl).
func healthcheck() int {
	_, port, err := net.SplitHostPort(envOr("ADDR", ":8080"))
	if err != nil {
		return 1
	}
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func main() {
	hc := flag.Bool("healthcheck", false, "vérifie que le serveur local répond puis quitte")
	flag.Parse()
	if *hc {
		os.Exit(healthcheck())
	}

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("configuration", "err", err)
		os.Exit(1)
	}
	srv := &server{cfg: cfg, st: newStore(cfg), lim: newLimiter(cfg.joinPerMinute)}
	handler, err := srv.routes()
	if err != nil {
		slog.Error("routes", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Annuler baseCtx ferme les flux SSE ouverts, sinon Shutdown les attendrait.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}

	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				srv.st.expire(now)
				srv.lim.prune(now)
			}
		}
	}()

	go func() {
		slog.Info("zedcode démarré", "addr", cfg.addr, "ttl", cfg.roomTTL, "trust_proxy", cfg.trustProxy)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("serveur HTTP", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("arrêt…")
	cancelBase()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
}
