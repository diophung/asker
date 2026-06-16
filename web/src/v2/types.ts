// Data model for the Asker v2 search UI.
//
// Results are HETEROGENEOUS — an email, a file, a message, a calendar event, a
// photo, or a person. Every result shares a provenance trio (source / who /
// when) + title + snippet (Google's scannable rhythm); each type layers its own
// affordances on top. The shapes here are the contract the stub
// (`searchPersonalData`) fills and the UI renders — keep all data concerns
// behind the stub so this is trivially swappable for the real Asker API.

export type SourceFilter =
  | "all"
  | "email"
  | "files"
  | "messages"
  | "calendar"
  | "photos"
  | "people";

export type ResultType =
  | "email"
  | "file"
  | "message"
  | "calendar"
  | "photo"
  | "person";

/** The connector/source a result came from — drives the brand-colored dot. */
export type SourceName =
  | "Gmail"
  | "Drive"
  | "Slack"
  | "Calendar"
  | "Photos"
  | "Contacts";

/** Common provenance + body every result carries. */
interface ResultBase {
  id: string;
  type: ResultType;
  source: SourceName;
  /** Provenance "who/where": "from Sarah Chen", "/Projects/Q3", "#q3-planning". */
  who: string;
  /** Provenance "when": "3 days ago", "edited Tuesday". */
  when: string;
  title: string;
  /** Plain-text snippet; query terms are bolded at render time. */
  snippet: string;
  /** Lowercase blob the stub matches against (title+body+people+source). */
  haystack: string;
  /** Browser-openable link to the real item (set by the backend mapper). */
  url?: string;
  /** The connector instance this came from ("gmail", "slack", …); drives the
   * doc_type/connector_id sent with personalization feedback. */
  connectorId?: string;
  /** The DocType enum name (EMAIL, FILE, …) for feedback. */
  docType?: string;
  /** Participant addresses/handles for feedback (senders). */
  senders?: string[];
  /** Topic terms matched by the ranker, for feedback. */
  topics?: string[];
  /** The v3 "why this ranked" reason; "" / undefined when not personalized. */
  explanation?: string;
}

export interface EmailResult extends ResultBase {
  type: "email";
  sender: string;
  replyCount?: number;
  hasAttachment?: boolean;
}

export type FileKind = "pdf" | "doc" | "sheet" | "slides" | "image" | "other";

export interface FileResult extends ResultBase {
  type: "file";
  fileKind: FileKind;
  owner: string;
  /** Folder breadcrumb, e.g. "My Drive / Projects / Q3". */
  folder: string;
}

export interface MessageResult extends ResultBase {
  type: "message";
  sender: string;
  /** Channel ("#q3-planning") or DM name ("Sarah Chen"). */
  channel: string;
  reactions?: number;
}

export interface CalendarResult extends ResultBase {
  type: "calendar";
  /** Pre-formatted date/time block, e.g. "Thu, Aug 14 · 10:00 AM". */
  start: string;
  month: string; // "AUG"
  day: string; // "14"
  attendees: string[];
  location?: string;
}

export interface PhotoResult extends ResultBase {
  type: "photo";
  place?: string;
  /** Two CSS colors for the placeholder thumbnail gradient (no backend). */
  thumb: string[];
}

export interface PersonResult extends ResultBase {
  type: "person";
  name: string;
  role: string;
  org: string;
  lastContacted: string;
  sharedDocs: string[];
  upcomingMeeting?: string;
  email: string;
}

export type SearchResult =
  | EmailResult
  | FileResult
  | MessageResult
  | CalendarResult
  | PhotoResult
  | PersonResult;

/** A typed autocomplete row — diverges from Google's "popular queries". */
export type Suggestion =
  | { kind: "recent"; text: string }
  | { kind: "person"; text: string; sub: string }
  | { kind: "doc"; text: string; sub: string };

/** Knowledge panel — Google's entity card, rebuilt from the user's own graph. */
export interface PersonPanel {
  kind: "person";
  name: string;
  role: string;
  org: string;
  lastContacted: string;
  sharedDocs: string[];
  upcomingMeeting?: string;
  email: string;
}

export interface ProjectPanel {
  kind: "project";
  name: string;
  summary: string;
  people: string[];
  recentActivity: string[];
  files: string[];
}

export type Panel = PersonPanel | ProjectPanel;
