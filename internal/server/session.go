package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"

	"omasend/internal/protocol"
)

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
func (s *sessionStore) create(peer protocol.DeviceInfo, ip string, files map[string]protocol.FileMetadata) (*session, map[string]string) {
	ctx, cancel := context.WithCancel(context.Background())
	sess := &session{
		id:     randToken(),
		peer:   peer,
		ip:     ip,
		files:  make(map[string]*fileEntry, len(files)),
		ctx:    ctx,
		cancel: cancel,
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
	return sess, tokens
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
