package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"omasend/internal/protocol"
)

// Bounds on what accepted-but-unused sessions may cost us. A peer that calls
// prepare-upload and then never uploads used to leave its metadata map behind
// for good; with auto-accept on, repeating that was unbounded growth.
const (
	// sessionTTL is how long an idle session survives. Only sessions with no
	// upload in flight are ever expired, so a slow transfer is never cut off.
	sessionTTL = 10 * time.Minute

	// maxSessions bounds concurrent sessions across all peers.
	maxSessions = 64

	// maxSessionFiles bounds the total file entries held across all sessions,
	// since one session may declare very many files.
	maxSessionFiles = 20000
)

// ErrTooManySessions is returned when accepting another session would exceed
// the bounds above.
var ErrTooManySessions = errors.New("too many pending sessions")

// fileEntry tracks one file within a session.
type fileEntry struct {
	meta  protocol.FileMetadata
	token string
	done  bool
}

// session is one accepted prepare-upload, holding per-file tokens and a cancel
// hook that aborts in-flight writes.
type session struct {
	id     string
	peer   protocol.DeviceInfo
	ip     string
	files  map[string]*fileEntry // by fileId
	ctx    context.Context
	cancel context.CancelFunc

	lastUsed time.Time // for idle expiry
	active   int       // uploads in flight; a busy session is never expired
}

// sessionStore is the concurrency-safe registry of active sessions.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*session
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]*session)}
}

// randToken returns a random 32-hex-char token/id.
func randToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// create builds a session for the given files and returns it plus the
// fileId->token map for the prepare-upload response.
func (s *sessionStore) create(peer protocol.DeviceInfo, ip string, files map[string]protocol.FileMetadata) (*session, map[string]string, error) {
	// Clear anything that has aged out before deciding whether we are full;
	// the common case is that an abandoned session simply expires.
	s.sweep()

	s.mu.Lock()
	held := 0
	for _, existing := range s.sessions {
		held += len(existing.files)
	}
	if len(s.sessions) >= maxSessions || held+len(files) > maxSessionFiles {
		s.mu.Unlock()
		return nil, nil, ErrTooManySessions
	}
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	sess := &session{
		id:       randToken(),
		peer:     peer,
		ip:       ip,
		files:    make(map[string]*fileEntry, len(files)),
		ctx:      ctx,
		cancel:   cancel,
		lastUsed: time.Now(),
	}
	tokens := make(map[string]string, len(files))
	for fileID, meta := range files {
		tok := randToken()
		sess.files[fileID] = &fileEntry{meta: meta, token: tok}
		tokens[fileID] = tok
	}

	s.mu.Lock()
	s.sessions[sess.id] = sess
	s.mu.Unlock()
	return sess, tokens, nil
}

// sweep drops sessions that have been idle past sessionTTL. A session with an
// upload in flight is left alone however long it takes.
func (s *sessionStore) sweep() {
	now := time.Now()
	var dead []*session
	s.mu.Lock()
	for id, sess := range s.sessions {
		if sess.active == 0 && now.Sub(sess.lastUsed) > sessionTTL {
			delete(s.sessions, id)
			dead = append(dead, sess)
		}
	}
	s.mu.Unlock()
	for _, sess := range dead {
		sess.cancel()
	}
}

// beginUpload marks a session busy so the sweep cannot expire it mid-transfer.
func (s *sessionStore) beginUpload(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok {
		sess.active++
		sess.lastUsed = time.Now()
	}
}

// endUpload releases the busy mark and restarts the idle clock.
func (s *sessionStore) endUpload(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok {
		if sess.active > 0 {
			sess.active--
		}
		sess.lastUsed = time.Now()
	}
}

// lookup returns the session and file entry for an upload, validating the token.
func (s *sessionStore) lookup(sessionID, fileID, token string) (*session, *fileEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil, nil, false
	}
	fe, ok := sess.files[fileID]
	if !ok || fe.token != token {
		return nil, nil, false
	}
	return sess, fe, true
}

// cancel aborts a session's in-flight writes and removes it.
func (s *sessionStore) cancel(sessionID string) {
	s.mu.Lock()
	sess, ok := s.sessions[sessionID]
	if ok {
		delete(s.sessions, sessionID)
	}
	s.mu.Unlock()
	if ok {
		sess.cancel()
	}
}

// complete marks a file done and, if all files are done, removes the session.
func (s *sessionStore) complete(sessionID, fileID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok {
		return
	}
	if fe, ok := sess.files[fileID]; ok {
		fe.done = true
	}
	for _, fe := range sess.files {
		if !fe.done {
			return
		}
	}
	sess.cancel()
	delete(s.sessions, sessionID)
}
