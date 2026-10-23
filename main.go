// zedcode — partage de code temps réel, façon codeshare, ultra léger.
//
// Une salle = code.zedxp.com/<6 caractères [A-Za-z0-9]>. Tout vit en RAM :
// rien n'est écrit sur disque, donc rien ne finit dans une sauvegarde.
// Une salle disparaît après ROOM_TTL sans activité.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

//go:embed web
var webFS embed.FS

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

var (
	roomRe   = regexp.MustCompile(`^[A-Za-z0-9]{6}$`)
	clientRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	langRe   = regexp.MustCompile(`^[a-z0-9]{0,16}$`)
	fileRe   = regexp.MustCompile(`^[A-Za-z0-9]{10}$`)

	errFull     = errors.New("quota mémoire atteint")
	errTooMany  = errors.New("trop de salles ou de connexions")
	errNotFound = errors.New("salle inconnue")
	errNoFile   = errors.New("fichier inconnu")
	errBusy     = errors.New("un envoi de fichier est déjà en cours dans cette salle")
)

// ---------------------------------------------------------------- config

type config struct {
	addr          string
	roomTTL       time.Duration
	maxDoc        int
	maxFile       int
	maxFiles      int
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
		{"MAX_FILE_BYTES", &c.maxFile, 250 << 20},
		{"MAX_FILES_PER_ROOM", &c.maxFiles, 3},
		{"MAX_TOTAL_BYTES", &c.maxTotal, 256 << 20},
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
	L string `json:"l,omitempty"` // langage choisi, partagé par la salle
	F string `json:"f,omitempty"`
}

// fileInfo : ce que les appareils voient d'un fichier (pas son contenu).
type fileInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int    `json:"size"`
}

type file struct {
	fileInfo
	data    []byte // jamais modifié après l'ajout : lisible hors verrou
	seq     uint64 // ordre d'arrivée, pour évincer le plus ancien
	readers int    // téléchargements en cours
	evicted bool   // retiré de sa salle, mais encore lu : reste compté
}

type sub struct {
	doc      chan event
	presence chan int
	files    chan []fileInfo
}

type room struct {
	ev        event
	files     []*file // du plus ancien au plus récent
	uploading bool    // un envoi à la fois par salle
	updated   time.Time
	subs      map[*sub]struct{}
}

func (r *room) fileList() []fileInfo {
	l := make([]fileInfo, len(r.files))
	for i, f := range r.files {
		l[i] = f.fileInfo
	}
	return l
}

type store struct {
	cfg   config
	mu    sync.Mutex
	rooms map[string]*room
	total int // octets en RAM : textes, fichiers, envois réservés, fichiers évincés encore lus
	nsubs int
	seq   uint64
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
func (s *store) subscribe(id string) (*sub, event, []fileInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nsubs >= s.cfg.maxSubs {
		return nil, event{}, nil, errTooMany
	}
	r, ok := s.rooms[id]
	if !ok {
		if len(s.rooms) >= s.cfg.maxRooms {
			return nil, event{}, nil, errTooMany
		}
		r = &room{updated: time.Now(), subs: map[*sub]struct{}{}}
		s.rooms[id] = r
	}
	sb := &sub{doc: make(chan event, 1), presence: make(chan int, 1), files: make(chan []fileInfo, 1)}
	r.subs[sb] = struct{}{}
	s.nsubs++
	s.presenceLocked(r)
	return sb, r.ev, r.fileList(), nil
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
	if len(r.subs) == 0 && r.ev.V == 0 && len(r.files) == 0 && !r.uploading {
		delete(s.rooms, id) // salle jamais écrite : rien à garder
		return
	}
	r.updated = time.Now() // le départ du dernier appareil compte comme activité
	s.presenceLocked(r)
}

func (s *store) put(id, text, lang, from string) (uint64, error) {
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
	r.ev = event{V: r.ev.V + 1, T: text, L: lang, F: from}
	r.updated = time.Now()
	for sb := range r.subs {
		offer(sb.doc, r.ev)
	}
	return r.ev.V, nil
}

func (s *store) filesChangedLocked(r *room) {
	r.updated = time.Now()
	l := r.fileList()
	for sb := range r.subs {
		offer(sb.files, l)
	}
}

// dropLocked retire le fichier i de la salle. Sa RAM n'est rendue au quota
// qu'une fois le dernier téléchargement en cours terminé (voir doneFile).
func (s *store) dropLocked(r *room, i int) {
	f := r.files[i]
	// slices.Delete remet à nil la case libérée : avec append, le tableau sous-jacent
	// garderait un pointeur vers f et ses données ne seraient jamais libérées.
	r.files = slices.Delete(r.files, i, i+1)
	if f.readers > 0 {
		f.evicted = true
	} else {
		s.total -= f.Size
	}
}

// oldestLocked : le plus ancien fichier du serveur qu'on peut libérer tout de
// suite (aucun téléchargement en cours).
func (s *store) oldestLocked() (*room, int) {
	var best *room
	bi := -1
	for _, r := range s.rooms {
		for i, f := range r.files {
			if f.readers == 0 && (best == nil || f.seq < best.files[bi].seq) {
				best, bi = r, i
			}
		}
	}
	return best, bi
}

// reserve prépare l'envoi de n octets dans la salle id, AVANT la lecture du
// corps : la RAM ne dépasse jamais MAX_TOTAL_BYTES. Pour faire de la place, on
// supprime d'abord le plus ancien fichier de la salle si elle est pleine, puis
// les plus anciens fichiers du serveur, toutes salles confondues. Si même ça
// ne suffit pas (textes, autres envois, téléchargements en cours), rien n'est
// supprimé et l'envoi est refusé. L'appelant doit ensuite appeler setFile ou
// release.
func (s *store) reserve(id string, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rooms[id]
	if !ok {
		return errNotFound
	}
	if r.uploading {
		return errBusy
	}
	freeable := 0
	for _, o := range s.rooms {
		for _, f := range o.files {
			if f.readers == 0 {
				freeable += f.Size
			}
		}
	}
	if s.total-freeable+n > s.cfg.maxTotal {
		return errFull
	}
	changed := map[*room]bool{}
	for len(r.files) >= s.cfg.maxFiles {
		s.dropLocked(r, 0)
		changed[r] = true
	}
	for s.total+n > s.cfg.maxTotal {
		o, i := s.oldestLocked()
		if o == nil { // impossible vu le contrôle ci-dessus ; par prudence
			return errFull
		}
		s.dropLocked(o, i)
		changed[o] = true
	}
	for o := range changed {
		s.filesChangedLocked(o)
	}
	s.total += n
	r.uploading = true
	return nil
}

func (s *store) release(id string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total -= n
	if r, ok := s.rooms[id]; ok {
		r.uploading = false
	}
}

// setFile ajoute le fichier réservé par reserve. Si la salle a disparu
// entre-temps, la réservation est rendue.
func (s *store) setFile(id, name string, data []byte) (fileInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rooms[id]
	if !ok {
		s.total -= len(data)
		return fileInfo{}, errNotFound
	}
	r.uploading = false
	if len(r.files) >= s.cfg.maxFiles { // pas possible en principe : un envoi à la fois
		s.dropLocked(r, 0)
	}
	s.seq++
	f := &file{fileInfo: fileInfo{ID: genID(10), Name: name, Size: len(data)}, data: data, seq: s.seq}
	r.files = append(r.files, f)
	s.filesChangedLocked(r)
	return f.fileInfo, nil
}

// getFile ouvre un fichier en lecture ; l'appelant doit appeler doneFile.
func (s *store) getFile(id, fid string) (*file, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rooms[id]
	if !ok {
		return nil, errNotFound
	}
	for _, f := range r.files {
		if f.ID == fid {
			f.readers++
			return f, nil
		}
	}
	return nil, errNoFile
}

func (s *store) doneFile(f *file) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f.readers--
	if f.readers == 0 && f.evicted {
		s.total -= f.Size
	}
}

func (s *store) delFile(id, fid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rooms[id]
	if !ok {
		return errNotFound
	}
	for i, f := range r.files {
		if f.ID == fid {
			s.dropLocked(r, i)
			s.filesChangedLocked(r)
			return nil
		}
	}
	return errNoFile
}

func (s *store) expire(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, r := range s.rooms {
		if len(r.subs) == 0 && !r.uploading && now.Sub(r.updated) > s.cfg.roomTTL {
			for len(r.files) > 0 {
				s.dropLocked(r, 0)
			}
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
	cfg     config
	st      *store
	lim     *limiter
	static  fs.FS
	version string // empreinte des fichiers web : URL versionnées, cache long
	index   []byte
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

func genCode() string { return genID(6) }

func genID(n int) string {
	b := make([]byte, n)
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

const csp = "default-src 'none'; script-src 'self'; style-src 'self'%s; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// handleIndex sert la page avec un nonce CSP neuf : il autorise uniquement la
// balise <style> que l'éditeur (CodeMirror) injecte, sans 'unsafe-inline'.
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !roomRe.MatchString(r.PathValue("id")) {
		http.NotFound(w, r)
		return
	}
	nb := make([]byte, 16)
	rand.Read(nb)
	nonce := base64.StdEncoding.EncodeToString(nb)
	h := w.Header()
	h.Set("Content-Security-Policy", fmt.Sprintf(csp, " 'nonce-"+nonce+"'"))
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.Write(bytes.Replace(s.index, []byte("__NONCE__"), []byte(nonce), 1))
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
	sb, snap, files, err := s.st.subscribe(id)
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
	if err := writeSSE(w, "files", files); err != nil {
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
		case l := <-sb.files:
			err = writeSSE(w, "files", l)
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
		L string `json:"l"`
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
	if !clientRe.MatchString(body.F) || !langRe.MatchString(body.L) {
		http.Error(w, "requête invalide", http.StatusBadRequest)
		return
	}
	v, err := s.st.put(id, body.T, body.L, body.F)
	if err != nil {
		storeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]uint64{"v": v})
}

func storeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNotFound), errors.Is(err, errNoFile):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errFull):
		http.Error(w, err.Error(), http.StatusInsufficientStorage)
	case errors.Is(err, errBusy):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}
}

// cleanName garde un nom de fichier affichable : pas de chemin, pas de
// caractère de contrôle, 200 octets max.
func cleanName(name string) string {
	name = strings.ToValidUTF8(name, "")
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return -1
		}
		return c
	}, name)
	name = strings.TrimSpace(name)
	for len(name) > 200 {
		_, n := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-n]
	}
	if name == "" || name == "." || name == "/" || name == ".." {
		return "fichier"
	}
	return name
}

// fileTimeout : durée max d'un envoi ou d'un téléchargement (250 Mo à 1 Mbit/s
// prennent environ 35 min).
const fileTimeout = time.Hour

// handleUpload : POST /api/rooms/{id}/files?name=…, corps = contenu brut.
// Content-Type application/octet-stream obligatoire : un formulaire d'un
// autre site ne peut pas l'envoyer sans pré-vol CORS (refusé).
func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !roomRe.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/octet-stream" {
		http.Error(w, "application/octet-stream requis", http.StatusUnsupportedMediaType)
		return
	}
	n := r.ContentLength
	if n < 0 {
		http.Error(w, "Content-Length requis", http.StatusLengthRequired)
		return
	}
	if n > int64(s.cfg.maxFile) {
		http.Error(w, fmt.Sprintf("fichier trop gros (max %d Mo)", s.cfg.maxFile>>20), http.StatusRequestEntityTooLarge)
		return
	}
	if err := s.st.reserve(id, int(n)); err != nil {
		storeErr(w, err)
		return
	}
	// Les fichiers évincés par reserve ne sont plus référencés : on rend leur
	// RAM au système avant d'allouer le nouveau, sinon les deux coexistent un
	// moment (2 × 250 Mo ne tiennent pas dans le conteneur). Un []byte n'a pas
	// de pointeurs : ce GC forcé est rapide.
	if n >= 1<<20 {
		debug.FreeOSMemory()
	}
	// Gros fichier ou connexion lente : bien plus que le ReadTimeout global.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(fileTimeout))
	data := make([]byte, n)
	if _, err := io.ReadFull(r.Body, data); err != nil {
		s.st.release(id, int(n))
		http.Error(w, "envoi interrompu", http.StatusBadRequest)
		return
	}
	info, err := s.st.setFile(id, cleanName(r.URL.Query().Get("name")), data)
	if err != nil {
		storeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func (s *server) handleFile(w http.ResponseWriter, r *http.Request) {
	id, fid := r.PathValue("id"), r.PathValue("fid")
	if !roomRe.MatchString(id) || !fileRe.MatchString(fid) {
		http.NotFound(w, r)
		return
	}
	f, err := s.st.getFile(id, fid)
	if err != nil {
		storeErr(w, err)
		return
	}
	defer s.st.doneFile(f)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fileTimeout))
	// Jamais interprété par le navigateur : téléchargement forcé, page isolée.
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(f.data))
}

func (s *server) handleDelFile(w http.ResponseWriter, r *http.Request) {
	id, fid := r.PathValue("id"), r.PathValue("fid")
	if !roomRe.MatchString(id) || !fileRe.MatchString(fid) {
		http.NotFound(w, r)
		return
	}
	if err := s.st.delFile(id, fid); err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", fmt.Sprintf(csp, ""))
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

// handleStatic sert les fichiers web. Avec ?v=<version courante>, la réponse
// est mise en cache un an (l'URL change à chaque nouvelle version). Si une
// variante .gz a été générée au build, elle est servie aux navigateurs qui
// acceptent gzip.
func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !fs.ValidPath(name) || strings.HasSuffix(name, ".gz") || name == "index.html" {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	if r.URL.Query().Get("v") == s.version {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	h.Set("Vary", "Accept-Encoding")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		if gz, err := fs.ReadFile(s.static, name+".gz"); err == nil {
			h.Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
			h.Set("Content-Encoding", "gzip")
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(gz))
			return
		}
	}
	http.ServeFileFS(w, r, s.static, name)
}

func (s *server) routes() (http.Handler, error) {
	var err error
	if s.static, err = fs.Sub(webFS, "web"); err != nil {
		return nil, err
	}
	sum := sha256.New()
	err = fs.WalkDir(s.static, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(s.static, p)
		sum.Write([]byte(p))
		sum.Write(b)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.version = hex.EncodeToString(sum.Sum(nil))[:12]
	index, err := fs.ReadFile(s.static, "index.html")
	if err != nil {
		return nil, err
	}
	index = bytes.ReplaceAll(index, []byte("__V__"), []byte(s.version))
	s.index = bytes.ReplaceAll(index, []byte("__MAXFILE__"), []byte(strconv.Itoa(s.cfg.maxFile)))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleNew)
	mux.HandleFunc("GET /{id}", s.handleIndex)
	mux.HandleFunc("GET /static/{file...}", s.handleStatic)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/rooms/{id}/events", s.handleEvents)
	mux.HandleFunc("PUT /api/rooms/{id}", s.handlePut)
	mux.HandleFunc("POST /api/rooms/{id}/files", s.handleUpload)
	mux.HandleFunc("GET /api/rooms/{id}/files/{fid}", s.handleFile)
	mux.HandleFunc("DELETE /api/rooms/{id}/files/{fid}", s.handleDelFile)
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
