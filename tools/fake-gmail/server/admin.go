package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Admin API: unauthenticated mailbox manipulation for dev/CI (ADR-008).
// These endpoints drive the scenarios the connector e2e suite needs —
// seeding deterministic corpora, source edits and deletes — and exist only
// inside the compose network. Errors use a plain {"error": "..."} shape
// (this is our surface, not Google's).

type adminError struct {
	Error string `json:"error"`
}

type seedRequest struct {
	Count int   `json:"count"`
	Seed  int64 `json:"seed"`
}

type seedResponse struct {
	Seeded    int    `json:"seeded"`
	HistoryID uint64 `json:"historyId"`
}

type addMessageRequest struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
	From    string `json:"from"`
	To      string `json:"to"`
}

type editMessageRequest struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// handleAdminSeed implements POST /admin/users/{email}/seed
// {count, seed}: generates count deterministic synthetic emails. Re-seeding
// with the same seed regenerates the same ids and is recorded as source
// edits (delete+add per id), not duplicates.
func (s *Server) handleAdminSeed(w http.ResponseWriter, r *http.Request) {
	email, ok := s.adminEmail(w, r)
	if !ok {
		return
	}
	var req seedRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}
	if req.Count <= 0 || req.Count > maxSeedCount {
		s.writeAdminError(w, http.StatusBadRequest,
			fmt.Sprintf("count must be in [1, %d]", maxSeedCount))
		return
	}
	inputs := generateSeedMessages(email, req.Seed, req.Count)
	res := s.store.seedMessages(email, inputs)
	// One notification for the whole batch: watchers diff via history.list.
	s.pusher.notify(res.watch, email, res.historyID)
	s.log.Info("seeded mailbox", "email", email, "count", req.Count, "seed", req.Seed, "history_id", res.historyID)
	s.writeJSON(w, http.StatusOK, seedResponse{Seeded: req.Count, HistoryID: res.historyID})
}

// handleAdminAddMessage implements POST /admin/users/{email}/messages.
func (s *Server) handleAdminAddMessage(w http.ResponseWriter, r *http.Request) {
	email, ok := s.adminEmail(w, r)
	if !ok {
		return
	}
	var req addMessageRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}
	if req.Subject == "" && req.Body == "" {
		s.writeAdminError(w, http.StatusBadRequest, "subject or body required")
		return
	}
	from := req.From
	if from == "" {
		from = seedContacts[0]
	}
	to := req.To
	if to == "" {
		to = email
	}
	res := s.store.addMessage(email, messageInput{
		from:    from,
		to:      to,
		subject: req.Subject,
		body:    req.Body,
	})
	s.pusher.notify(res.watch, email, res.historyID)
	s.writeJSON(w, http.StatusCreated, toWireMessage(res.msg))
}

// handleAdminEditMessage implements PUT /admin/users/{email}/messages/{id}.
// It models an EDIT AT THE SOURCE: the mailbox historyId advances and the
// history log records messageDeleted followed by messageAdded for the SAME
// message id (see store.editMessage). Connectors replaying history in order
// therefore re-fetch the message and re-ingest it under the same doc_id with
// a new version_etag.
func (s *Server) handleAdminEditMessage(w http.ResponseWriter, r *http.Request) {
	email, ok := s.adminEmail(w, r)
	if !ok {
		return
	}
	var req editMessageRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}
	if req.Subject == "" && req.Body == "" {
		s.writeAdminError(w, http.StatusBadRequest, "subject or body required")
		return
	}
	res, err := s.store.editMessage(email, r.PathValue("id"), req.Subject, req.Body)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, errMessageNotFound) {
			code = http.StatusNotFound
		}
		s.writeAdminError(w, code, err.Error())
		return
	}
	s.pusher.notify(res.watch, email, res.historyID)
	s.writeJSON(w, http.StatusOK, toWireMessage(res.msg))
}

// handleAdminDeleteMessage implements DELETE /admin/users/{email}/messages/{id}
// — the tombstone path: history records messageDeleted and the connector is
// expected to emit a Tombstone document.
func (s *Server) handleAdminDeleteMessage(w http.ResponseWriter, r *http.Request) {
	email, ok := s.adminEmail(w, r)
	if !ok {
		return
	}
	res, err := s.store.deleteMessage(email, r.PathValue("id"))
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, errMessageNotFound) {
			code = http.StatusNotFound
		}
		s.writeAdminError(w, code, err.Error())
		return
	}
	s.pusher.notify(res.watch, email, res.historyID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminEmail(w http.ResponseWriter, r *http.Request) (string, bool) {
	email := r.PathValue("email")
	if !validEmail(email) {
		s.writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("invalid email %q in path", email))
		return "", false
	}
	// Mailboxes are keyed case-insensitively, matching the auth path.
	return strings.ToLower(email), true
}

func (s *Server) writeAdminError(w http.ResponseWriter, code int, msg string) {
	s.writeJSON(w, code, adminError{Error: msg})
}
