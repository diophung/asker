// Package server implements an in-memory fake of the Gmail REST v1 API
// subset used by the Asker Gmail connector (ADR-008: dev/CI only, never
// deployed to production). It is wire-compatible with the generated
// google.golang.org/api/gmail/v1 client pointed at this server via
// option.WithEndpoint: the client resolves "gmail/v1/users/..." paths
// against the endpoint, so all API routes live under /gmail/v1/.
//
// Trust model: this is a development tool. The Gmail API surface requires a
// dev shim bearer token ("fake-gmail-token:<email>") purely so the connector
// exercises its real auth plumbing; the /admin API is intentionally
// unauthenticated and must never be exposed outside the compose network.
package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// tokenPrefix is the dev auth shim: "fake-gmail-token:<email>".
	tokenPrefix = "fake-gmail-token:"

	// pushURLHeader carries the push delivery URL on users.watch calls.
	// Real Gmail pushes via Cloud Pub/Sub; this header is our dev shim.
	pushURLHeader = "X-Asker-Push-Url"

	watchTTL = 7 * 24 * time.Hour

	defaultPageSize = 100
	maxPageSize     = 500

	maxSeedCount = 100000
)

// Server is the fake Gmail HTTP server. It implements http.Handler.
type Server struct {
	log    *slog.Logger
	store  *store
	pusher *pusher
	mux    *http.ServeMux
	now    func() time.Time
}

// Option customizes a Server.
type Option func(*Server)

// WithLogger sets the logger (default: slog.Default()).
func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.log = l } }

// WithPushClient overrides the HTTP client used for push deliveries.
func WithPushClient(c *http.Client) Option {
	return func(s *Server) { s.pusher.client = c }
}

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option {
	return func(s *Server) {
		s.now = now
		s.store.now = now
		s.pusher.now = now
	}
}

// New constructs a ready-to-serve fake Gmail server.
func New(opts ...Option) *Server {
	s := &Server{
		log: slog.Default(),
		now: time.Now,
	}
	s.store = newStore(func() time.Time { return s.now() })
	s.pusher = newPusher(slog.Default(), nil, func() time.Time { return s.now() })
	for _, opt := range opts {
		opt(s)
	}
	s.pusher.log = s.log

	mux := http.NewServeMux()
	// Gmail API surface (bearer-token authenticated).
	mux.HandleFunc("GET /gmail/v1/users/{userId}/profile", s.requireAuth(s.handleGetProfile))
	mux.HandleFunc("GET /gmail/v1/users/{userId}/messages", s.requireAuth(s.handleListMessages))
	mux.HandleFunc("GET /gmail/v1/users/{userId}/messages/{id}", s.requireAuth(s.handleGetMessage))
	mux.HandleFunc("GET /gmail/v1/users/{userId}/history", s.requireAuth(s.handleListHistory))
	mux.HandleFunc("POST /gmail/v1/users/{userId}/watch", s.requireAuth(s.handleWatch))
	// Admin surface (dev only, unauthenticated).
	mux.HandleFunc("POST /admin/users/{email}/seed", s.handleAdminSeed)
	mux.HandleFunc("POST /admin/users/{email}/messages", s.handleAdminAddMessage)
	mux.HandleFunc("PUT /admin/users/{email}/messages/{id}", s.handleAdminEditMessage)
	mux.HandleFunc("DELETE /admin/users/{email}/messages/{id}", s.handleAdminDeleteMessage)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, "ok")
	})
	s.mux = mux
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Close waits for in-flight push deliveries to drain.
func (s *Server) Close() {
	s.pusher.wait()
}

// requireAuth wraps a Gmail API handler: it maps the dev bearer token
// ("fake-gmail-token:<email>") to a mailbox, resolves the {userId} path
// segment ("me" or the literal address of the authenticated user) and calls
// next with the resolved email. Failures use Google's JSON error shape so
// the generated client surfaces proper *googleapi.Error values.
func (s *Server) requireAuth(next func(w http.ResponseWriter, r *http.Request, email string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		const bearer = "Bearer "
		if !strings.HasPrefix(authz, bearer) {
			s.writeGoogleError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authError",
				"Login Required: expected Authorization: Bearer fake-gmail-token:<email>")
			return
		}
		token := strings.TrimPrefix(authz, bearer)
		email, ok := strings.CutPrefix(token, tokenPrefix)
		if !ok || !validEmail(email) {
			s.writeGoogleError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authError",
				"Invalid Credentials")
			return
		}
		email = strings.ToLower(email)

		userID := r.PathValue("userId")
		switch {
		case userID == "me", strings.EqualFold(userID, email):
			// ok
		default:
			s.writeGoogleError(w, http.StatusForbidden, "PERMISSION_DENIED", "forbidden",
				fmt.Sprintf("Delegation denied for %s", email))
			return
		}
		next(w, r, email)
	}
}

// --- Gmail API handlers -----------------------------------------------

// handleGetProfile implements GET /gmail/v1/users/{userId}/profile
// (users.getProfile): the canonical way for clients to learn the mailbox's
// current historyId (e.g. before starting a full sync).
func (s *Server) handleGetProfile(w http.ResponseWriter, _ *http.Request, email string) {
	p := s.store.profile(email)
	s.writeJSON(w, http.StatusOK, wireProfile{
		EmailAddress:  email,
		MessagesTotal: p.messagesTotal,
		ThreadsTotal:  p.threadsTotal,
		HistoryID:     p.historyID,
	})
}

// handleListMessages implements GET /gmail/v1/users/{userId}/messages
// (users.messages.list): id+threadId refs, newest first, paginated via
// maxResults/pageToken.
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request, email string) {
	maxResults, err := pageSize(r.URL.Query().Get("maxResults"))
	if err != nil {
		s.writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalidArgument", err.Error())
		return
	}
	refs, next, total, err := s.store.listMessages(email, maxResults, r.URL.Query().Get("pageToken"))
	if err != nil {
		s.writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalidArgument", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, wireListMessagesResponse{
		Messages:           refs,
		NextPageToken:      next,
		ResultSizeEstimate: int64(total),
	})
}

// handleGetMessage implements GET /gmail/v1/users/{userId}/messages/{id}
// (users.messages.get). The fake always renders format=full — the only
// format the Asker connector requests.
func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request, email string) {
	id := r.PathValue("id")
	msg, err := s.store.getMessage(email, id)
	if err != nil {
		s.writeGoogleError(w, http.StatusNotFound, "NOT_FOUND", "notFound",
			fmt.Sprintf("Requested entity was not found: message %q", id))
		return
	}
	if f := r.URL.Query().Get("format"); f != "" && !strings.EqualFold(f, "full") {
		s.log.Debug("ignoring unsupported message format, serving full", "format", f)
	}
	s.writeJSON(w, http.StatusOK, toWireMessage(msg))
}

// handleListHistory implements GET /gmail/v1/users/{userId}/history
// (users.history.list). Like the real API it returns 404 when
// startHistoryId is too old (pruned from the capped log) or unknown; the
// connector must respond by falling back to a full sync.
func (s *Server) handleListHistory(w http.ResponseWriter, r *http.Request, email string) {
	q := r.URL.Query()
	rawStart := q.Get("startHistoryId")
	if rawStart == "" {
		s.writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalidArgument",
			"Missing required parameter: startHistoryId")
		return
	}
	startID, err := strconv.ParseUint(rawStart, 10, 64)
	if err != nil {
		s.writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalidArgument",
			fmt.Sprintf("Invalid startHistoryId %q", rawStart))
		return
	}
	maxResults, err := pageSize(q.Get("maxResults"))
	if err != nil {
		s.writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalidArgument", err.Error())
		return
	}
	var types map[string]bool
	if vals := q["historyTypes"]; len(vals) > 0 {
		types = make(map[string]bool, len(vals))
		for _, v := range vals {
			types[v] = true
		}
	}

	entries, current, next, err := s.store.listHistory(email, startID, types, maxResults, q.Get("pageToken"))
	switch {
	case errors.Is(err, errStaleHistoryID):
		s.writeGoogleError(w, http.StatusNotFound, "NOT_FOUND", "notFound",
			fmt.Sprintf("startHistoryId %d is too old or unknown; perform a full sync", startID))
		return
	case err != nil:
		s.writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalidArgument", err.Error())
		return
	}

	resp := wireListHistoryResponse{HistoryID: current, NextPageToken: next}
	for _, e := range entries {
		h := wireHistory{ID: e.id}
		for _, ref := range e.added {
			h.MessagesAdded = append(h.MessagesAdded, wireHistoryMessageChange{Message: ref})
			h.Messages = append(h.Messages, ref)
		}
		for _, ref := range e.deleted {
			h.MessagesDeleted = append(h.MessagesDeleted, wireHistoryMessageChange{Message: ref})
			h.Messages = append(h.Messages, ref)
		}
		resp.History = append(resp.History, h)
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// handleWatch implements POST /gmail/v1/users/{userId}/watch. Instead of
// publishing to Cloud Pub/Sub, the fake POSTs Pub/Sub-style push envelopes
// directly to the URL supplied in the X-Asker-Push-Url header on every
// subsequent mailbox change (ADR-008 dev shim).
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request, email string) {
	var req wireWatchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalidArgument",
			"Invalid JSON payload received")
		return
	}
	if req.TopicName == "" {
		s.writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalidArgument",
			"topicName required")
		return
	}
	pushURL := r.Header.Get(pushURLHeader)
	if pushURL == "" {
		s.writeGoogleError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalidArgument",
			"fake-gmail requires the "+pushURLHeader+" header on watch requests (dev Pub/Sub shim, ADR-008)")
		return
	}
	expiration := s.now().Add(watchTTL)
	historyID := s.store.setWatch(email, req.TopicName, pushURL, expiration)
	s.log.Info("watch registered", "email", email, "topic", req.TopicName, "push_url", pushURL)
	s.writeJSON(w, http.StatusOK, wireWatchResponse{
		HistoryID:  historyID,
		Expiration: expiration.UnixMilli(),
	})
}

// --- helpers ------------------------------------------------------------

func pageSize(raw string) (int, error) {
	if raw == "" {
		return defaultPageSize, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid maxResults %q", raw)
	}
	if n > maxPageSize {
		n = maxPageSize
	}
	return n, nil
}

func validEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	return at > 0 && at < len(s)-1 && !strings.ContainsAny(s, " \t\r\n")
}

func toWireMessage(m *storedMessage) wireMessage {
	body := []byte(m.body)
	return wireMessage{
		ID:           m.id,
		ThreadID:     m.threadID,
		LabelIDs:     m.labelIDs,
		Snippet:      snippet(m.body),
		HistoryID:    m.historyID,
		InternalDate: m.internalDate.UnixMilli(),
		SizeEstimate: int64(len(body)),
		Payload: &wirePart{
			PartID:   "",
			MimeType: "text/plain",
			Headers: []wireHeader{
				{Name: "From", Value: m.from},
				{Name: "To", Value: m.to},
				{Name: "Subject", Value: m.subject},
				{Name: "Date", Value: m.internalDate.UTC().Format(time.RFC1123Z)},
			},
			Body: &wirePartBody{
				Data: base64.RawURLEncoding.EncodeToString(body),
				Size: int64(len(body)),
			},
		},
	}
}

func snippet(body string) string {
	const max = 120
	if len(body) <= max {
		return body
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut]
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Error("encode response", "error", err)
	}
}

func (s *Server) writeGoogleError(w http.ResponseWriter, code int, status, reason, message string) {
	s.writeJSON(w, code, googleError{Error: googleErrorBody{
		Code:    code,
		Message: message,
		Errors:  []googleErrorItem{{Message: message, Reason: reason}},
		Status:  status,
	}})
}
