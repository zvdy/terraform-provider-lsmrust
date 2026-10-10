package mockserver

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	StatusPending  = "pending"
	StatusUpdating = "updating"
	StatusReady    = "ready"
)

var (
	validTiers  = []string{"shared", "dedicated"}
	validScopes = []string{"read", "write", "admin"}
)

type ResourceLimits struct {
	MemoryMB int64 `json:"memory_mb"`
	DiskGB   int64 `json:"disk_gb"`
}

type BackupPolicy struct {
	Schedule  string `json:"schedule"`
	Retention string `json:"retention"`
}

type Database struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Tier           string         `json:"tier"`
	Status         string         `json:"status"`
	ResourceLimits ResourceLimits `json:"resource_limits"`
	BackupPolicy   BackupPolicy   `json:"backup_policy"`

	generation uint64
}

type AccessKey struct {
	ID         string   `json:"id"`
	DatabaseID string   `json:"database_id"`
	Scopes     []string `json:"scopes"`
	Secret     string   `json:"secret,omitempty"`
}

type Config struct {
	ProvisionDelay time.Duration
	APIKey         string
	Logger         *log.Logger
}

type Server struct {
	cfg Config
	mux *http.ServeMux

	mu        sync.Mutex
	databases map[string]*Database
	keys      map[string]*AccessKey
}

func New(cfg Config) *Server {
	s := &Server{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		databases: map[string]*Database{},
		keys:      map[string]*AccessKey{},
	}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("POST /v1/databases", s.createDatabase)
	s.mux.HandleFunc("GET /v1/databases", s.listDatabases)
	s.mux.HandleFunc("GET /v1/databases/{id}", s.getDatabase)
	s.mux.HandleFunc("PATCH /v1/databases/{id}", s.updateDatabase)
	s.mux.HandleFunc("DELETE /v1/databases/{id}", s.deleteDatabase)
	s.mux.HandleFunc("POST /v1/databases/{id}/access_keys", s.createAccessKey)
	s.mux.HandleFunc("GET /v1/access_keys/{id}", s.getAccessKey)
	s.mux.HandleFunc("PATCH /v1/access_keys/{id}", s.updateAccessKey)
	s.mux.HandleFunc("DELETE /v1/access_keys/{id}", s.deleteAccessKey)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Logger != nil {
		s.cfg.Logger.Printf("%s %s", r.Method, r.URL.Path)
	}
	// Liveness/readiness probes carry no credentials and expose no state.
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		s.mux.ServeHTTP(w, r)
		return
	}
	if s.cfg.APIKey != "" && !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "missing or invalid API key")
		return
	}
	s.mux.ServeHTTP(w, r)
}

// authorized compares the bearer token in constant time, so response timing
// does not leak how much of a guessed key was correct.
func (s *Server) authorized(r *http.Request) bool {
	got := r.Header.Get("Authorization")
	want := "Bearer " + s.cfg.APIKey
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type createDatabaseRequest struct {
	Name           string         `json:"name"`
	Tier           string         `json:"tier"`
	ResourceLimits ResourceLimits `json:"resource_limits"`
	BackupPolicy   BackupPolicy   `json:"backup_policy"`
}

func (s *Server) createDatabase(w http.ResponseWriter, r *http.Request) {
	var req createDatabaseRequest
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if !slices.Contains(validTiers, req.Tier) {
		writeError(w, http.StatusBadRequest, "tier must be one of: shared, dedicated")
		return
	}
	if msg := validateLimits(req.ResourceLimits); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	s.mu.Lock()
	db := &Database{
		ID:             newID("db"),
		Name:           req.Name,
		Tier:           req.Tier,
		Status:         StatusPending,
		ResourceLimits: req.ResourceLimits,
		BackupPolicy:   req.BackupPolicy,
	}
	s.databases[db.ID] = db
	gen := s.bumpGeneration(db)
	out := *db
	s.mu.Unlock()

	s.scheduleReady(db.ID, gen)
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) listDatabases(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	out := make([]Database, 0, len(s.databases))
	for _, db := range s.databases {
		out = append(out, *db)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getDatabase(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	db, ok := s.databases[r.PathValue("id")]
	var out Database
	if ok {
		out = *db
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "database not found")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type updateDatabaseRequest struct {
	Name           *string         `json:"name"`
	Tier           *string         `json:"tier"`
	ResourceLimits *ResourceLimits `json:"resource_limits"`
	BackupPolicy   *BackupPolicy   `json:"backup_policy"`
}

func (s *Server) updateDatabase(w http.ResponseWriter, r *http.Request) {
	var req updateDatabaseRequest
	if !decode(w, r, &req) {
		return
	}
	if req.ResourceLimits != nil {
		if msg := validateLimits(*req.ResourceLimits); msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}

	s.mu.Lock()
	db, ok := s.databases[r.PathValue("id")]
	if !ok {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "database not found")
		return
	}
	if req.Tier != nil && *req.Tier != db.Tier {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "in-place tier migration is not supported; recreate the database")
		return
	}
	if req.Name != nil {
		if strings.TrimSpace(*req.Name) == "" {
			s.mu.Unlock()
			writeError(w, http.StatusBadRequest, "name must not be empty")
			return
		}
		db.Name = *req.Name
	}
	if req.BackupPolicy != nil {
		db.BackupPolicy = *req.BackupPolicy
	}
	resize := req.ResourceLimits != nil && *req.ResourceLimits != db.ResourceLimits
	if resize {
		db.ResourceLimits = *req.ResourceLimits
		db.Status = StatusUpdating
	}
	var gen uint64
	if resize {
		gen = s.bumpGeneration(db)
	}
	out := *db
	s.mu.Unlock()

	if resize {
		s.scheduleReady(out.ID, gen)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteDatabase(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	_, ok := s.databases[id]
	if ok {
		delete(s.databases, id)
		for kid, k := range s.keys {
			if k.DatabaseID == id {
				delete(s.keys, kid)
			}
		}
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "database not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) bumpGeneration(db *Database) uint64 {
	db.generation++
	return db.generation
}

// scheduleReady only applies if no newer operation has started on the
// database since, so an earlier timer cannot mark a later resize as done.
func (s *Server) scheduleReady(id string, gen uint64) {
	time.AfterFunc(s.cfg.ProvisionDelay, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if db, ok := s.databases[id]; ok && db.generation == gen {
			db.Status = StatusReady
		}
	})
}

type accessKeyRequest struct {
	Scopes []string `json:"scopes"`
}

func (s *Server) createAccessKey(w http.ResponseWriter, r *http.Request) {
	var req accessKeyRequest
	if !decode(w, r, &req) {
		return
	}
	if msg := validateScopes(req.Scopes); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	dbID := r.PathValue("id")
	s.mu.Lock()
	if _, ok := s.databases[dbID]; !ok {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "database not found")
		return
	}
	k := &AccessKey{
		ID:         newID("key"),
		DatabaseID: dbID,
		Scopes:     slices.Clone(req.Scopes),
	}
	s.keys[k.ID] = k
	out := *k
	s.mu.Unlock()

	out.Secret = randomHex(32)
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) getAccessKey(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	k, ok := s.keys[r.PathValue("id")]
	var out AccessKey
	if ok {
		out = *k
		out.Scopes = slices.Clone(k.Scopes)
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "access key not found")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) updateAccessKey(w http.ResponseWriter, r *http.Request) {
	var req accessKeyRequest
	if !decode(w, r, &req) {
		return
	}
	if msg := validateScopes(req.Scopes); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	s.mu.Lock()
	k, ok := s.keys[r.PathValue("id")]
	var out AccessKey
	if ok {
		k.Scopes = slices.Clone(req.Scopes)
		out = *k
		out.Scopes = slices.Clone(k.Scopes)
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "access key not found")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteAccessKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	_, ok := s.keys[id]
	delete(s.keys, id)
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "access key not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validateLimits(l ResourceLimits) string {
	if l.MemoryMB <= 0 || l.DiskGB <= 0 {
		return "resource_limits.memory_mb and resource_limits.disk_gb must be positive"
	}
	return ""
}

func validateScopes(scopes []string) string {
	if len(scopes) == 0 {
		return "scopes must not be empty"
	}
	for _, sc := range scopes {
		if !slices.Contains(validScopes, sc) {
			return "invalid scope " + sc + "; must be one of: read, write, admin"
		}
	}
	return ""
}

// maxBodyBytes bounds a request body; every legitimate payload is well under 1 KiB.
const maxBodyBytes = 1 << 20

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func newID(prefix string) string {
	return prefix + "-" + randomHex(6)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
