// zedcode — partage de code temps réel, chiffré de bout en bout.
//
// Le serveur ne stocke et ne relaie que des blobs opaques : le navigateur
// chiffre le texte en AES-256-GCM avec une clé qui vit dans le fragment
// d'URL (#...), lequel n'est jamais transmis au serveur.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

var (
	roomRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
	clientRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

	errFull     = errors.New("quota de stockage atteint")
	errTooMany  = errors.New("trop de salles ou de connexions")
	sessionName = "zc_session"
)

// ---------------------------------------------------------------- config

type config struct {
	addr         string
	token        string
	dataDir      string
	roomTTL      time.Duration
	maxDoc       int
	maxTotal     int
	maxRooms     int
	maxSubs      int
	cookieSecure bool
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
		addr:         envOr("ADDR", ":8080"),
		token:        os.Getenv("ACCESS_TOKEN"),
		dataDir:      os.Getenv("DATA_DIR"),
		cookieSecure: os.Getenv("COOKIE_INSECURE") != "1",
	}
	var err error
	if c.roomTTL, err = time.ParseDuration(envOr("ROOM_TTL", "72h")); err != nil {
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
	} {
		if *x.dst, err = envInt(x.k, x.def); err != nil {
			return c, err
		}
	}
	switch {
	case c.token == "" && os.Getenv("INSECURE_NO_AUTH") == "1":
		slog.Warn("authentification désactivée (INSECURE_NO_AUTH=1)")
	case len(c.token) < 16:
		return c, errors.New("ACCESS_TOKEN manquant ou trop court (16 caractères min). Génère-le avec : openssl rand -hex 32")
	}
	return c, nil
}

// ----------------------------------------------------------------- store

type event struct {
	V uint64 `json:"v"`
	D string `json:"d"`
	F string `json:"f,omitempty"`
}

type persisted struct {
	V uint64 `json:"v"`
	D string `json:"d"`
	T int64  `json:"t"`
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
	dirty map[string]struct{}
	gone  map[string]struct{}
}

func newStore(cfg config) *store {
	return &store{
		cfg:   cfg,
		rooms: map[string]*room{},
		dirty: map[string]struct{}{},
		gone:  map[string]struct{}{},
	}
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

func (s *store) roomLocked(id string) (*room, error) {
	if r, ok := s.rooms[id]; ok {
		return r, nil
	}
	if len(s.rooms) >= s.cfg.maxRooms {
		return nil, errTooMany
	}
	r := &room{updated: time.Now(), subs: map[*sub]struct{}{}}
	s.rooms[id] = r
	return r, nil
}

func (s *store) presenceLocked(r *room) {
	for sb := range r.subs {
		offer(sb.presence, len(r.subs))
	}
}

func (s *store) subscribe(id string) (*sub, event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nsubs >= s.cfg.maxSubs {
		return nil, event{}, errTooMany
	}
	r, err := s.roomLocked(id)
	if err != nil {
		return nil, event{}, err
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
	s.presenceLocked(r)
}

func (s *store) put(id, data, from string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.roomLocked(id)
	if err != nil {
		return 0, err
	}
	if s.total-len(r.ev.D)+len(data) > s.cfg.maxTotal {
		return 0, errFull
	}
	s.total += len(data) - len(r.ev.D)
	r.ev = event{V: r.ev.V + 1, D: data, F: from}
	r.updated = time.Now()
	s.dirty[id] = struct{}{}
	delete(s.gone, id)
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
			s.total -= len(r.ev.D)
			delete(s.rooms, id)
			delete(s.dirty, id)
			s.gone[id] = struct{}{}
		}
	}
}

func (s *store) path(id string) string { return filepath.Join(s.cfg.dataDir, id+".json") }

// flush écrit les salles modifiées sur disque. Appelée depuis une seule
// goroutine : pas de course entre écritures concurrentes d'un même fichier.
func (s *store) flush() {
	if s.cfg.dataDir == "" {
		return
	}
	s.mu.Lock()
	writes := make(map[string]persisted, len(s.dirty))
	for id := range s.dirty {
		if r, ok := s.rooms[id]; ok {
			writes[id] = persisted{V: r.ev.V, D: r.ev.D, T: r.updated.Unix()}
		}
	}
	dels := s.gone
	s.dirty = map[string]struct{}{}
	s.gone = map[string]struct{}{}
	s.mu.Unlock()

	for id, p := range writes {
		if err := writeAtomic(s.path(id), p); err != nil {
			slog.Error("écriture salle", "room", id, "err", err)
		}
	}
	for id := range dels {
		if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Error("suppression salle", "room", id, "err", err)
		}
	}
}

func writeAtomic(path string, v any) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(v); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (s *store) load() error {
	if s.cfg.dataDir == "" {
		return nil
	}
	if err := os.MkdirAll(s.cfg.dataDir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.cfg.dataDir)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !roomRe.MatchString(id) {
			continue
		}
		b, err := os.ReadFile(s.path(id))
		if err != nil {
			return err
		}
		var p persisted
		if err := json.Unmarshal(b, &p); err != nil {
			slog.Warn("fichier de salle illisible, ignoré", "room", id, "err", err)
			continue
		}
		updated := time.Unix(p.T, 0)
		if now.Sub(updated) > s.cfg.roomTTL {
			os.Remove(s.path(id))
			continue
		}
		s.rooms[id] = &room{ev: event{V: p.V, D: p.D}, updated: updated, subs: map[*sub]struct{}{}}
		s.total += len(p.D)
	}
	slog.Info("salles chargées", "count", len(s.rooms), "bytes", s.total)
	return nil
}

func (s *store) run(ctx context.Context) {
	flushT := time.NewTicker(2 * time.Second)
	expireT := time.NewTicker(time.Minute)
	defer flushT.Stop()
	defer expireT.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flush()
			return
		case <-flushT.C:
			s.flush()
		case now := <-expireT.C:
			s.expire(now)
		}
	}
}

// ---------------------------------------------------------------- server

type server struct {
	cfg     config
	st      *store
	session string // valeur attendue du cookie de session
	index   []byte
}

func (s *server) authed(r *http.Request) bool {
	if s.cfg.token == "" {
		return true
	}
	c, err := r.Cookie(sessionName)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.session)) == 1
}

func (s *server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	})
}

// isJSON impose application/json sur les écritures : un formulaire
// cross-site ne peut pas l'envoyer sans requête preflight CORS.
func isJSON(r *http.Request) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return mt == "application/json"
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if id := r.PathValue("id"); id != "" && !roomRe.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(s.index)
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !isJSON(r) {
		http.Error(w, "application/json requis", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "requête invalide", http.StatusBadRequest)
		return
	}
	// Comparaison de condensats : longueur fixe, temps constant.
	got, want := sha256.Sum256([]byte(body.Token)), sha256.Sum256([]byte(s.cfg.token))
	if s.cfg.token == "" || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		time.Sleep(time.Second) // freine le brute-force
		http.Error(w, "jeton invalide", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionName, Value: s.session, Path: "/",
		MaxAge: 30 * 24 * 3600, HttpOnly: true, Secure: s.cfg.cookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.cookieSecure, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
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
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.cfg.maxDoc)+1024)
	var body struct {
		D string `json:"d"`
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
	if len(body.D) > s.cfg.maxDoc {
		http.Error(w, "document trop gros", http.StatusRequestEntityTooLarge)
		return
	}
	if !clientRe.MatchString(body.F) {
		http.Error(w, "identifiant client invalide", http.StatusBadRequest)
		return
	}
	v, err := s.st.put(id, body.D, body.F)
	switch {
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
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /r/{id}", s.handleIndex)
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.Handle("GET /api/rooms/{id}/events", s.auth(s.handleEvents))
	mux.Handle("PUT /api/rooms/{id}", s.auth(s.handlePut))
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
	st := newStore(cfg)
	if err := st.load(); err != nil {
		slog.Error("chargement des données", "err", err)
		os.Exit(1)
	}

	mac := hmac.New(sha256.New, []byte(cfg.token))
	mac.Write([]byte("zedcode-session-v1"))
	srv := &server{cfg: cfg, st: st, session: hex.EncodeToString(mac.Sum(nil))}
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

	storeDone := make(chan struct{})
	storeCtx, stopStore := context.WithCancel(context.Background())
	go func() { st.run(storeCtx); close(storeDone) }()

	go func() {
		slog.Info("zedcode démarré", "addr", cfg.addr, "persistence", cfg.dataDir != "", "auth", cfg.token != "")
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
	stopStore()
	<-storeDone
}
