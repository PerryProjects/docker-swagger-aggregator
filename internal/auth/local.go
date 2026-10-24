package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/securecookie"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/perryprojects/docker-swagger-aggregator/internal/store"
)

const (
	minPasswordLen = 10
	maxPasswordLen = 72 // bcrypt ignores everything after 72 bytes; refusing is safer than silently truncating
	maxBody        = 8 << 10
	maxThrottle    = 30 * time.Second
	throttleKeys   = 10_000 // purge bound of the in-memory failure table
	setupKey       = "\x00setup"
)

var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

// LocalConfig configures the built-in (username + password) authentication.
type LocalConfig struct {
	Store *store.Store
	// SessionSecret signs and encrypts the session cookie (at least 32 characters).
	SessionSecret string
	// BasePath is the public path prefix the UI is served under ("" or "/docs").
	BasePath   string
	SessionTTL time.Duration // default 8h
	// SetupToken is required to create the first user (while there is none). Empty: one is generated.
	SetupToken string
	// BcryptCost defaults to 12; tests lower it.
	BcryptCost int
	// SameSite of the session cookie: Lax (the default, also for the zero value) or None for a frontend on another
	// *site* (None implies Secure, so it needs HTTPS or localhost).
	SameSite http.SameSite
	// Heartbeat is the keep-alive interval of the user event stream (default 15s).
	Heartbeat time.Duration
	// ThrottleBase is the delay after the first failed attempt (default 1s); it doubles up to 30s.
	ThrottleBase time.Duration
}

// Local is the built-in authentication: login form + users in the database. Create it with NewLocal.
type Local struct {
	cfg     LocalConfig
	log     *zap.Logger
	cookies *securecookie.SecureCookie
	dummy   []byte // a hash to compare against for unknown users, so timing does not reveal them

	mu       sync.Mutex
	failures map[string]failure
	users    hub // told whenever the user list changes, for the event stream
}

// hub tells subscribers that something changed (coalesced: a pending signal is not queued twice).
type hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func (h *hub) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[chan struct{}]struct{}{}
	}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *hub) notify() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

type failure struct {
	count int
	until time.Time
}

type localSession struct {
	UID   uint `json:"u"`
	Epoch int  `json:"e"`
}

// publicUser is what the API tells about a user; never the hash.
type publicUser struct {
	ID       uint   `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Disabled bool   `json:"disabled"`
}

func toPublic(u store.User) publicUser {
	return publicUser{ID: u.ID, Username: u.Username, Role: u.Role, Disabled: u.Disabled}
}

// NewLocal validates the configuration and prepares the setup token. It does not create any user: see Bootstrap.
func NewLocal(cfg LocalConfig, log *zap.Logger) (*Local, error) {
	if cfg.Store == nil {
		return nil, errors.New("built-in auth needs a database")
	}
	if len(cfg.SessionSecret) < minSecretLen {
		return nil, fmt.Errorf("session secret must be at least %d characters", minSecretLen)
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = defaultTTL
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 15 * time.Second
	}
	if cfg.ThrottleBase <= 0 {
		cfg.ThrottleBase = time.Second
	}
	if cfg.BcryptCost == 0 {
		cfg.BcryptCost = 12
	}
	hashKey := sha256.Sum256([]byte("hash\x00" + cfg.SessionSecret))
	blockKey := sha256.Sum256([]byte("block\x00" + cfg.SessionSecret))
	dummy, err := bcrypt.GenerateFromPassword([]byte("dummy"), cfg.BcryptCost)
	if err != nil {
		return nil, err
	}
	return &Local{
		cfg: cfg, log: log, dummy: dummy, failures: map[string]failure{},
		cookies: securecookie.New(hashKey[:], blockKey[:]).SetSerializer(securecookie.JSONEncoder{}),
	}, nil
}

// Bootstrap prepares the first start: with no user yet it creates the admin from adminUser plus adminPassword
// (or a bcrypt adminHash) if given, otherwise it makes sure a setup token exists and logs it. With users in
// the database it does nothing: the environment never overwrites an existing account.
func (l *Local) Bootstrap(ctx context.Context, adminUser, adminPassword, adminHash string) error {
	n, err := l.cfg.Store.CountUsers(ctx)
	if err != nil || n > 0 {
		return err
	}
	if adminUser != "" {
		hash := adminHash
		if hash == "" {
			if err = validatePassword(adminPassword); err != nil {
				return fmt.Errorf("admin password: %w", err)
			}
			if hash, err = l.hash(adminPassword); err != nil {
				return err
			}
		} else if _, costErr := bcrypt.Cost([]byte(hash)); costErr != nil {
			return errors.New("admin password hash is not a bcrypt hash")
		}
		name := strings.ToLower(adminUser)
		if !usernameRe.MatchString(name) {
			return fmt.Errorf("admin user %q: use 3-64 characters from a-z 0-9 . _ -", adminUser)
		}
		if _, err = l.cfg.Store.CreateFirstUser(ctx, name, hash, store.RoleAdmin); err != nil && !errors.Is(err, store.ErrNotEmpty) {
			return err
		}
		l.log.Info("created the admin user from the environment", zap.String("user", name))
		return nil
	}
	if l.cfg.SetupToken == "" {
		l.cfg.SetupToken = random()
	}
	l.log.Warn("no user exists yet: open the UI and register the first admin with the setup token",
		zap.String("setupToken", l.cfg.SetupToken), zap.String("path", l.cfg.BasePath+"/"))
	return nil
}

func (l *Local) hash(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), l.cfg.BcryptCost)
	return string(b), err
}

func validatePassword(p string) error {
	switch {
	case len(p) < minPasswordLen:
		return fmt.Errorf("must be at least %d characters", minPasswordLen)
	case len(p) > maxPasswordLen:
		return fmt.Errorf("must be at most %d bytes", maxPasswordLen)
	}
	return nil
}

// Wrap serves the /auth/* endpoints and requires a session for /api/*. Everything else (the static UI and the
// /healthz probe) stays public: the UI shows the login form itself, and it contains no data.
func (l *Local) Wrap(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/session", l.sessionInfo)
	mux.HandleFunc("POST /auth/login", l.login)
	mux.HandleFunc("POST /auth/logout", l.logout)
	mux.HandleFunc("POST /auth/setup", l.setup)
	mux.HandleFunc("POST /auth/password", l.requireUser(l.changeOwnPassword))
	mux.HandleFunc("GET /auth/users", l.requireAdmin(l.listUsers))
	mux.HandleFunc("GET /auth/users/events", l.requireAdmin(l.usersEvents))
	mux.HandleFunc("POST /auth/users", l.requireAdmin(l.createUser))
	mux.HandleFunc("PATCH /auth/users/{id}", l.requireAdmin(l.updateUser))
	mux.HandleFunc("DELETE /auth/users/{id}", l.requireAdmin(l.deleteUser))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/auth/"):
			mux.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/"):
			if _, ok := l.current(r); !ok {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
				return
			}
			w.Header().Set("Cache-Control", "private, no-store")
			next.ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// current returns the signed-in user: the cookie must be valid and match the user's current state.
func (l *Local) current(r *http.Request) (store.User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return store.User{}, false
	}
	var s localSession
	if err = l.cookies.Decode(sessionCookie, c.Value, &s); err != nil {
		return store.User{}, false
	}
	u, err := l.cfg.Store.UserByID(r.Context(), s.UID)
	if err != nil || u.Disabled || u.SessionEpoch != s.Epoch {
		return store.User{}, false
	}
	return u, true
}

func (l *Local) requireUser(h func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := l.current(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
			return
		}
		h(w, r, u)
	}
}

func (l *Local) requireAdmin(h func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return l.requireUser(func(w http.ResponseWriter, r *http.Request, u store.User) {
		if u.Role != store.RoleAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin only"})
			return
		}
		h(w, r, u)
	})
}

func (l *Local) sessionInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	n, err := l.cfg.Store.CountUsers(r.Context())
	if err != nil {
		l.fail(w, err)
		return
	}
	out := map[string]any{"mode": "build_in", "setupRequired": n == 0}
	if u, ok := l.current(r); ok {
		out["user"] = toPublic(u)
	}
	writeJSON(w, http.StatusOK, out)
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (l *Local) login(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if !decode(w, r, &c) {
		return
	}
	key := strings.ToLower(c.Username)
	if wait := l.throttled(key); wait > 0 {
		tooMany(w, wait)
		return
	}
	u, err := l.cfg.Store.UserByName(r.Context(), key)
	hash := l.dummy
	if err == nil {
		hash = []byte(u.PasswordHash)
	}
	passwordOK := bcrypt.CompareHashAndPassword(hash, []byte(c.Password)) == nil
	if err != nil || !passwordOK || u.Disabled {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			l.fail(w, err)
			return
		}
		l.failed(key)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong username or password"})
		return
	}
	l.succeeded(key)
	if !l.startSession(w, r, u) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": toPublic(u)})
}

func (l *Local) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, l.cookie(r, "", -1))
	w.WriteHeader(http.StatusNoContent)
}

type setupRequest struct {
	Token string `json:"token"`
	credentials
}

func (l *Local) setup(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	if !decode(w, r, &req) {
		return
	}
	if wait := l.throttled(setupKey); wait > 0 {
		tooMany(w, wait)
		return
	}
	n, err := l.cfg.Store.CountUsers(r.Context())
	if err != nil {
		l.fail(w, err)
		return
	}
	if n > 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already set up"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(l.cfg.SetupToken)) != 1 || l.cfg.SetupToken == "" {
		l.failed(setupKey)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "wrong setup token"})
		return
	}
	name, hash, ok := l.validateNew(w, req.Username, req.Password)
	if !ok {
		return
	}
	u, err := l.cfg.Store.CreateFirstUser(r.Context(), name, hash, store.RoleAdmin)
	if errors.Is(err, store.ErrNotEmpty) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already set up"})
		return
	}
	if err != nil {
		l.fail(w, err)
		return
	}
	l.succeeded(setupKey)
	l.users.notify()
	l.log.Info("first admin registered", zap.String("user", u.Username))
	if l.startSession(w, r, u) {
		writeJSON(w, http.StatusCreated, map[string]any{"user": toPublic(u)})
	}
}

// validateNew checks a new username and password; it writes the 400 itself.
func (l *Local) validateNew(w http.ResponseWriter, username, password string) (name, hash string, ok bool) {
	name = strings.ToLower(strings.TrimSpace(username))
	if !usernameRe.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username: use 3-64 characters from a-z 0-9 . _ -"})
		return "", "", false
	}
	if err := validatePassword(password); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password: " + err.Error()})
		return "", "", false
	}
	hash, err := l.hash(password)
	if err != nil {
		l.fail(w, err)
		return "", "", false
	}
	return name, hash, true
}

type passwordChange struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

func (l *Local) changeOwnPassword(w http.ResponseWriter, r *http.Request, u store.User) {
	var req passwordChange
	if !decode(w, r, &req) {
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Current)) != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "current password is wrong"})
		return
	}
	if err := validatePassword(req.New); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password: " + err.Error()})
		return
	}
	hash, err := l.hash(req.New)
	if err == nil {
		err = l.cfg.Store.SetPassword(r.Context(), u.ID, hash)
	}
	if err != nil {
		l.fail(w, err)
		return
	}
	// every session of this user ended; this browser gets a fresh one
	fresh, err := l.cfg.Store.UserByID(r.Context(), u.ID)
	if err == nil && l.startSession(w, r, fresh) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (l *Local) listUsers(w http.ResponseWriter, r *http.Request, _ store.User) {
	us, err := l.cfg.Store.ListUsers(r.Context())
	if err != nil {
		l.fail(w, err)
		return
	}
	out := make([]publicUser, len(us))
	for i, u := range us {
		out[i] = toPublic(u)
	}
	writeJSON(w, http.StatusOK, out)
}

// usersEvents streams the user list as Server-Sent Events: the whole list on connect and after every change, so the
// user management page stays current when another admin (or another tab) changes something. It ends as soon as the
// session behind it is no longer an admin's (signed out, demoted, disabled or deleted).
func (l *Local) usersEvents(w http.ResponseWriter, r *http.Request, _ store.User) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx; Traefik does not buffer SSE
	changed, unsubscribe := l.users.subscribe()
	defer unsubscribe() // subscribe before the first snapshot: no change falls in between

	send := func() bool {
		if u, ok := l.current(r); !ok || u.Role != store.RoleAdmin {
			return false
		}
		us, err := l.cfg.Store.ListUsers(r.Context())
		if err != nil {
			return false
		}
		out := make([]publicUser, len(us))
		for i, u := range us {
			out[i] = toPublic(u)
		}
		b, err := json.Marshal(out)
		if err != nil {
			return false
		}
		_, err = fmt.Fprintf(w, "event: users\ndata: %s\n\n", b)
		return err == nil && rc.Flush() == nil
	}
	if !send() {
		return
	}
	tick := time.NewTicker(l.cfg.Heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-changed:
			if !send() {
				return
			}
		case <-tick.C:
			if u, ok := l.current(r); !ok || u.Role != store.RoleAdmin {
				return
			}
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

type newUser struct {
	credentials
	Role string `json:"role"`
}

func (l *Local) createUser(w http.ResponseWriter, r *http.Request, _ store.User) {
	var req newUser
	if !decode(w, r, &req) {
		return
	}
	if req.Role == "" {
		req.Role = store.RoleUser
	}
	if !validRole(req.Role) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role must be admin or user"})
		return
	}
	name, hash, ok := l.validateNew(w, req.Username, req.Password)
	if !ok {
		return
	}
	u, err := l.cfg.Store.CreateUser(r.Context(), name, hash, req.Role)
	if errors.Is(err, store.ErrExists) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "username is taken"})
		return
	}
	if err != nil {
		l.fail(w, err)
		return
	}
	l.users.notify()
	writeJSON(w, http.StatusCreated, toPublic(u))
}

type userPatch struct {
	Password *string `json:"password"`
	Role     *string `json:"role"`
	Disabled *bool   `json:"disabled"`
}

func (l *Local) updateUser(w http.ResponseWriter, r *http.Request, actor store.User) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req userPatch
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	var err error
	if req.Role != nil {
		if !validRole(*req.Role) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role must be admin or user"})
			return
		}
		err = l.cfg.Store.SetRole(ctx, id, *req.Role)
	}
	if err == nil && req.Disabled != nil {
		err = l.cfg.Store.SetDisabled(ctx, id, *req.Disabled)
	}
	if err == nil && req.Password != nil {
		if perr := validatePassword(*req.Password); perr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password: " + perr.Error()})
			return
		}
		var hash string
		if hash, err = l.hash(*req.Password); err == nil {
			err = l.cfg.Store.SetPassword(ctx, id, hash)
		}
	}
	if !l.storeError(w, err) {
		return
	}
	l.users.notify()
	u, err := l.cfg.Store.UserByID(ctx, id)
	if !l.storeError(w, err) {
		return
	}
	if u.ID == actor.ID && l.startSession(w, r, u) { // the admin changed themself: keep this browser signed in
		writeJSON(w, http.StatusOK, toPublic(u))
		return
	}
	if u.ID != actor.ID {
		writeJSON(w, http.StatusOK, toPublic(u))
	}
}

func (l *Local) deleteUser(w http.ResponseWriter, r *http.Request, actor store.User) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if id == actor.ID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "you cannot delete yourself"})
		return
	}
	if !l.storeError(w, l.cfg.Store.DeleteUser(r.Context(), id)) {
		return
	}
	l.users.notify()
	w.WriteHeader(http.StatusNoContent)
}

// storeError maps store errors to responses; it returns true if err is nil.
func (l *Local) storeError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
	case errors.Is(err, store.ErrLastAdmin):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		l.fail(w, err)
	}
	return false
}

func (l *Local) fail(w http.ResponseWriter, err error) {
	l.log.Error("auth request failed", zap.Error(err))
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

// startSession sets the session cookie; false means a response was written.
func (l *Local) startSession(w http.ResponseWriter, r *http.Request, u store.User) bool {
	sc := *l.cookies
	sc.MaxAge(int(l.cfg.SessionTTL.Seconds()))
	enc, err := sc.Encode(sessionCookie, localSession{UID: u.ID, Epoch: u.SessionEpoch})
	if err != nil {
		l.fail(w, err)
		return false
	}
	http.SetCookie(w, l.cookie(r, enc, int(l.cfg.SessionTTL.Seconds())))
	return true
}

func (l *Local) cookie(r *http.Request, value string, maxAge int) *http.Cookie {
	path := l.cfg.BasePath
	if path == "" {
		path = "/"
	}
	return &http.Cookie{
		Name: sessionCookie, Value: value, Path: path, MaxAge: maxAge, HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" || l.cfg.SameSite == http.SameSiteNoneMode,
		SameSite: sameSiteOrLax(l.cfg.SameSite),
	}
}

// throttled returns how long the caller still has to wait after earlier failures (0: go ahead).
func (l *Local) throttled(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return max(0, time.Until(l.failures[key].until))
}

// failed delays the next attempt for key: base, 2*base, 4*base ... up to 30s. A delay (not a lock-out) so that
// somebody hammering a username cannot keep its owner out for longer than that.
func (l *Local) failed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.failures) > throttleKeys {
		for k, f := range l.failures {
			if time.Now().After(f.until) {
				delete(l.failures, k)
			}
		}
	}
	f := l.failures[key]
	f.count++
	f.until = time.Now().Add(min(l.cfg.ThrottleBase<<min(f.count-1, 5), maxThrottle))
	l.failures[key] = f
}

func (l *Local) succeeded(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many attempts, try again shortly"})
}

func validRole(r string) bool { return r == store.RoleAdmin || r == store.RoleUser }

func pathID(w http.ResponseWriter, r *http.Request) (uint, bool) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad user id"})
		return 0, false
	}
	return uint(id), true
}

// decode reads a small JSON body. Requiring application/json keeps the mutations out of reach of plain
// cross-site HTML forms (on top of the SameSite cookie, and of the origin check when CORS is on).
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "application/json" && !strings.HasPrefix(ct, "application/json;") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "content type must be application/json"})
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
