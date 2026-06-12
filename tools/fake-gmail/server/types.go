package server

// Wire types mirroring the Gmail REST v1 JSON shapes consumed by
// google.golang.org/api/gmail/v1. Field tags matter: the generated client
// declares historyId / internalDate / expiration with the `,string` JSON
// option, so this fake MUST serialize those numbers as JSON strings or the
// client fails to decode the response.

// wireMessageRef is the abbreviated message resource returned by
// users.messages.list and inside history records: only id and threadId.
type wireMessageRef struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId,omitempty"`
}

// wireMessage is the users.messages.get (format=full) resource subset the
// Asker Gmail connector needs.
type wireMessage struct {
	ID           string    `json:"id"`
	ThreadID     string    `json:"threadId,omitempty"`
	LabelIDs     []string  `json:"labelIds,omitempty"`
	Snippet      string    `json:"snippet,omitempty"`
	HistoryID    uint64    `json:"historyId,omitempty,string"`
	InternalDate int64     `json:"internalDate,omitempty,string"` // epoch millis
	SizeEstimate int64     `json:"sizeEstimate,omitempty"`
	Payload      *wirePart `json:"payload,omitempty"`
}

type wirePart struct {
	PartID   string        `json:"partId,omitempty"`
	MimeType string        `json:"mimeType,omitempty"`
	Filename string        `json:"filename,omitempty"`
	Headers  []wireHeader  `json:"headers,omitempty"`
	Body     *wirePartBody `json:"body,omitempty"`
	Parts    []*wirePart   `json:"parts,omitempty"`
}

type wireHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type wirePartBody struct {
	// Data is base64url (RFC 4648 §5, unpadded) like the real API.
	Data string `json:"data,omitempty"`
	Size int64  `json:"size,omitempty"`
}

type wireListMessagesResponse struct {
	Messages           []wireMessageRef `json:"messages,omitempty"`
	NextPageToken      string           `json:"nextPageToken,omitempty"`
	ResultSizeEstimate int64            `json:"resultSizeEstimate"`
}

type wireHistoryMessageChange struct {
	Message wireMessageRef `json:"message"`
}

type wireHistory struct {
	ID uint64 `json:"id,omitempty,string"`
	// Messages is the union of all messages touched by this record (the real
	// API populates it alongside the change-type lists).
	Messages        []wireMessageRef           `json:"messages,omitempty"`
	MessagesAdded   []wireHistoryMessageChange `json:"messagesAdded,omitempty"`
	MessagesDeleted []wireHistoryMessageChange `json:"messagesDeleted,omitempty"`
}

type wireListHistoryResponse struct {
	History       []wireHistory `json:"history,omitempty"`
	HistoryID     uint64        `json:"historyId,omitempty,string"`
	NextPageToken string        `json:"nextPageToken,omitempty"`
}

type wireWatchRequest struct {
	TopicName string   `json:"topicName"`
	LabelIDs  []string `json:"labelIds,omitempty"`
}

type wireWatchResponse struct {
	HistoryID  uint64 `json:"historyId,omitempty,string"`
	Expiration int64  `json:"expiration,omitempty,string"` // epoch millis
}

// googleError matches googleapi.CheckResponse's expected error body so the
// generated client surfaces our failures as *googleapi.Error with the right
// HTTP code (e.g. the 404 on a stale startHistoryId that triggers the
// connector's full-sync fallback).
type googleError struct {
	Error googleErrorBody `json:"error"`
}

type googleErrorBody struct {
	Code    int               `json:"code"`
	Message string            `json:"message"`
	Errors  []googleErrorItem `json:"errors,omitempty"`
	Status  string            `json:"status,omitempty"`
}

type googleErrorItem struct {
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

// pushEnvelope is the Pub/Sub push delivery shape POSTed to the URL that a
// users.watch caller registered via the X-Asker-Push-Url header (ADR-008 dev
// shim — there is no real Cloud Pub/Sub in dev).
type pushEnvelope struct {
	Message      pushMessage `json:"message"`
	Subscription string      `json:"subscription"`
}

type pushMessage struct {
	// Data is standard base64 (how Pub/Sub JSON encodes bytes) of a
	// pushData JSON document, exactly like real Gmail notifications.
	Data        string `json:"data"`
	MessageID   string `json:"messageId"`
	PublishTime string `json:"publishTime"`
}

// pushData is the decoded notification payload.
type pushData struct {
	EmailAddress string `json:"emailAddress"`
	HistoryID    uint64 `json:"historyId"`
}
