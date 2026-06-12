package server

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// maxHistoryEntries caps the per-user history log, mimicking Gmail's limited
// history retention. A startHistoryId older than the oldest retained entry
// gets a 404, which is the real API's signal that the client must fall back
// to a full sync.
const maxHistoryEntries = 10000

var (
	errMessageNotFound = errors.New("message not found")
	errStaleHistoryID  = errors.New("startHistoryId is stale or unknown")
)

// storedMessage is the mutable in-memory representation of one email.
type storedMessage struct {
	id           string
	threadID     string
	historyID    uint64 // history record that last touched this message
	internalDate time.Time
	from         string
	to           string
	subject      string
	body         string
	labelIDs     []string
}

// historyEntry is one mailbox change. Exactly one of added/deleted is set:
// every entry models a single change type, so consumers that process entries
// in id order always converge to the right state regardless of how they
// order the change lists within a record.
type historyEntry struct {
	id      uint64
	added   []wireMessageRef
	deleted []wireMessageRef
}

// watchTarget is a users.watch registration plus our X-Asker-Push-Url shim.
type watchTarget struct {
	topicName  string
	pushURL    string
	expiration time.Time
}

// mailbox is one user's state. All access goes through store.mu.
type mailbox struct {
	email     string
	messages  map[string]*storedMessage
	historyID uint64 // monotonically increasing, starts at 0 (first entry is 1)
	history   []historyEntry
	// prunedThrough is the highest history id evicted from the log; a
	// startHistoryId below it cannot be replayed and yields 404.
	prunedThrough uint64
	watch         *watchTarget
}

type store struct {
	mu    sync.Mutex
	users map[string]*mailbox
	now   func() time.Time
}

func newStore(now func() time.Time) *store {
	return &store{users: make(map[string]*mailbox), now: now}
}

// mailboxLocked returns (creating if needed) the mailbox for email.
// Caller must hold s.mu.
func (s *store) mailboxLocked(email string) *mailbox {
	mb, ok := s.users[email]
	if !ok {
		mb = &mailbox{email: email, messages: make(map[string]*storedMessage)}
		s.users[email] = mb
	}
	return mb
}

// appendHistoryLocked records one change, bumps the mailbox historyId and
// trims the log to maxHistoryEntries. Caller must hold s.mu.
func (mb *mailbox) appendHistoryLocked(added, deleted []wireMessageRef) uint64 {
	mb.historyID++
	mb.history = append(mb.history, historyEntry{id: mb.historyID, added: added, deleted: deleted})
	if n := len(mb.history) - maxHistoryEntries; n > 0 {
		mb.prunedThrough = mb.history[n-1].id
		mb.history = append([]historyEntry(nil), mb.history[n:]...)
	}
	return mb.historyID
}

// messageInput is the content of a message to create or update.
type messageInput struct {
	id           string // empty => assign one
	threadID     string // empty => same as id
	internalDate time.Time
	from, to     string
	subject      string
	body         string
	labelIDs     []string
}

// upsertResult reports what a mutation did, for admin responses and watcher
// notification.
type upsertResult struct {
	msg       *storedMessage
	historyID uint64 // mailbox historyId after the mutation
	watch     *watchTarget
}

// addMessage inserts a new message (history: messageAdded). If a message
// with the same id already exists the call degrades to editMessage so that
// re-seeding with the same seed is safe and is observed as source edits.
func (s *store) addMessage(email string, in messageInput) upsertResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.mailboxLocked(email)
	if in.id != "" {
		if _, exists := mb.messages[in.id]; exists {
			return s.editLocked(mb, mb.messages[in.id], in.subject, in.body)
		}
	}
	if in.id == "" {
		// Synthesize a Gmail-looking hex id that is unique in this mailbox.
		in.id = fmt.Sprintf("%016x", uint64(s.now().UnixNano()))
		for i := 0; ; i++ {
			if _, exists := mb.messages[in.id]; !exists {
				break
			}
			in.id = fmt.Sprintf("%015x%01x", uint64(s.now().UnixNano()), i%16)
		}
	}
	if in.threadID == "" {
		in.threadID = in.id
	}
	if in.internalDate.IsZero() {
		in.internalDate = s.now()
	}
	if len(in.labelIDs) == 0 {
		in.labelIDs = []string{"INBOX"}
	}
	msg := &storedMessage{
		id:           in.id,
		threadID:     in.threadID,
		internalDate: in.internalDate,
		from:         in.from,
		to:           in.to,
		subject:      in.subject,
		body:         in.body,
		labelIDs:     in.labelIDs,
	}
	ref := wireMessageRef{ID: msg.id, ThreadID: msg.threadID}
	msg.historyID = mb.appendHistoryLocked([]wireMessageRef{ref}, nil)
	mb.messages[msg.id] = msg
	return upsertResult{msg: msg, historyID: mb.historyID, watch: mb.watch}
}

// editMessage models an EDIT AT THE SOURCE (the real Gmail API has no body
// edits; ADR-008's fake needs them to exercise the connector's re-ingest
// path). The history log records a messageDeleted entry followed by a
// messageAdded entry for the SAME message id, in that order, each with its
// own history id. A consumer replaying history in id order therefore sees
// delete(id) then add(id) and ends up re-fetching the message — same doc_id,
// new content, new version (the message's historyId is bumped to the add
// entry's id, so it works as a version_etag).
func (s *store) editMessage(email, id, subject, body string) (upsertResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.mailboxLocked(email)
	msg, ok := mb.messages[id]
	if !ok {
		return upsertResult{}, errMessageNotFound
	}
	res := s.editLocked(mb, msg, subject, body)
	return res, nil
}

func (s *store) editLocked(mb *mailbox, msg *storedMessage, subject, body string) upsertResult {
	if subject != "" {
		msg.subject = subject
	}
	if body != "" {
		msg.body = body
	}
	ref := wireMessageRef{ID: msg.id, ThreadID: msg.threadID}
	mb.appendHistoryLocked(nil, []wireMessageRef{ref})                 // delete ...
	msg.historyID = mb.appendHistoryLocked([]wireMessageRef{ref}, nil) // ... then re-add
	return upsertResult{msg: msg, historyID: mb.historyID, watch: mb.watch}
}

// deleteMessage removes the message (history: messageDeleted). This is the
// tombstone path: the connector must emit a Tombstone for the doc.
func (s *store) deleteMessage(email, id string) (upsertResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.mailboxLocked(email)
	msg, ok := mb.messages[id]
	if !ok {
		return upsertResult{}, errMessageNotFound
	}
	delete(mb.messages, id)
	ref := wireMessageRef{ID: msg.id, ThreadID: msg.threadID}
	mb.appendHistoryLocked(nil, []wireMessageRef{ref})
	return upsertResult{msg: msg, historyID: mb.historyID, watch: mb.watch}, nil
}

// seedMessages bulk-upserts deterministic synthetic messages (one history
// entry per add, two per overwrite) under a single lock acquisition and
// returns the final historyId plus the watch to notify (one notification
// for the whole batch).
func (s *store) seedMessages(email string, inputs []messageInput) upsertResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.mailboxLocked(email)
	for _, in := range inputs {
		if existing, ok := mb.messages[in.id]; ok {
			s.editLocked(mb, existing, in.subject, in.body)
			continue
		}
		msg := &storedMessage{
			id:           in.id,
			threadID:     in.threadID,
			internalDate: in.internalDate,
			from:         in.from,
			to:           in.to,
			subject:      in.subject,
			body:         in.body,
			labelIDs:     in.labelIDs,
		}
		ref := wireMessageRef{ID: msg.id, ThreadID: msg.threadID}
		msg.historyID = mb.appendHistoryLocked([]wireMessageRef{ref}, nil)
		mb.messages[msg.id] = msg
	}
	return upsertResult{historyID: mb.historyID, watch: mb.watch}
}

// profileInfo is the users.getProfile snapshot of a mailbox.
type profileInfo struct {
	messagesTotal int64
	threadsTotal  int64
	historyID     uint64
}

// profile returns the mailbox's message/thread totals and current historyId.
func (s *store) profile(email string) profileInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.mailboxLocked(email)
	threads := make(map[string]struct{}, len(mb.messages))
	for _, m := range mb.messages {
		threads[m.threadID] = struct{}{}
	}
	return profileInfo{
		messagesTotal: int64(len(mb.messages)),
		threadsTotal:  int64(len(threads)),
		historyID:     mb.historyID,
	}
}

func (s *store) getMessage(email, id string) (*storedMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.mailboxLocked(email)
	msg, ok := mb.messages[id]
	if !ok {
		return nil, errMessageNotFound
	}
	cp := *msg
	return &cp, nil
}

// listMessages returns one page of message refs ordered newest-first by
// internalDate (id as tiebreaker), like the real API. The page token is a
// plain offset into the current snapshot — good enough for a dev fake;
// concurrent mutation between pages may shift results slightly.
func (s *store) listMessages(email string, maxResults int, pageToken string) (refs []wireMessageRef, next string, total int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.mailboxLocked(email)

	offset := 0
	if pageToken != "" {
		offset, err = strconv.Atoi(pageToken)
		if err != nil || offset < 0 {
			return nil, "", 0, fmt.Errorf("invalid pageToken %q", pageToken)
		}
	}

	all := make([]*storedMessage, 0, len(mb.messages))
	for _, m := range mb.messages {
		all = append(all, m)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].internalDate.Equal(all[j].internalDate) {
			return all[i].internalDate.After(all[j].internalDate)
		}
		return all[i].id > all[j].id
	})

	total = len(all)
	if offset > total {
		offset = total
	}
	end := offset + maxResults
	if end > total {
		end = total
	}
	for _, m := range all[offset:end] {
		refs = append(refs, wireMessageRef{ID: m.id, ThreadID: m.threadID})
	}
	if end < total {
		next = strconv.Itoa(end)
	}
	return refs, next, total, nil
}

// listHistory returns history entries with id > startID (and > the page
// token's id), filtered to the requested change types. It returns
// errStaleHistoryID when startID predates the retained log or is beyond the
// current historyId — both are 404 in the real API and must push the
// connector to a full sync.
func (s *store) listHistory(email string, startID uint64, types map[string]bool, maxResults int, pageToken string) (entries []historyEntry, current uint64, next string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.mailboxLocked(email)

	if startID > mb.historyID || startID < mb.prunedThrough {
		return nil, 0, "", errStaleHistoryID
	}
	after := startID
	if pageToken != "" {
		tok, perr := strconv.ParseUint(pageToken, 10, 64)
		if perr != nil {
			return nil, 0, "", fmt.Errorf("invalid pageToken %q", pageToken)
		}
		if tok > after {
			after = tok
		}
	}

	for _, e := range mb.history {
		if e.id <= after {
			continue
		}
		fe := e.filtered(types)
		if fe == nil {
			continue
		}
		if len(entries) == maxResults {
			next = strconv.FormatUint(entries[len(entries)-1].id, 10)
			break
		}
		entries = append(entries, *fe)
	}
	return entries, mb.historyID, next, nil
}

// filtered returns a copy of e keeping only the requested change types
// (nil/empty types means all), or nil if nothing relevant remains.
func (e historyEntry) filtered(types map[string]bool) *historyEntry {
	out := historyEntry{id: e.id}
	if len(types) == 0 || types["messageAdded"] {
		out.added = e.added
	}
	if len(types) == 0 || types["messageDeleted"] {
		out.deleted = e.deleted
	}
	if len(out.added) == 0 && len(out.deleted) == 0 {
		return nil
	}
	return &out
}

// setWatch registers (or replaces) the user's push target and returns the
// current historyId.
func (s *store) setWatch(email, topicName, pushURL string, expiration time.Time) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	mb := s.mailboxLocked(email)
	mb.watch = &watchTarget{topicName: topicName, pushURL: pushURL, expiration: expiration}
	return mb.historyID
}
