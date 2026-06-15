import { type ReactNode, useEffect, useMemo, useRef, useState } from "react";
import {
  AlertTriangle,
  ArrowLeft,
  Check,
  Download,
  Loader2,
  Lock,
  LogOut,
  Plus,
  RotateCw,
  Trash2,
  X,
} from "lucide-react";
import { ConnectorClient, type ConnectorInstanceStatus } from "../api";
import {
  buildConfig,
  CONNECTOR_CATALOG,
  type ConnectorType,
  oauthProviderLabel,
} from "../connectors/catalog";
import { currentUser, getToken } from "./auth";
import {
  type DocType,
  defaultProfile,
  deleteMyData,
  exportPersonalization,
  getMe,
  getPreferences,
  getSearchMode,
  type Me,
  type Profile,
  resetLearning,
  savePreferences,
  type SearchMode,
  setSearchMode,
} from "./backend";
import { Avatar, SourceIcon } from "./ui";
import type { SourceName } from "./types";

const CONNECTOR_LABEL: Record<string, { name: string; source: SourceName }> = {
  gmail: { name: "Gmail", source: "Gmail" },
  "outlook-mail": { name: "Outlook Mail", source: "Gmail" },
  gdrive: { name: "Google Drive", source: "Drive" },
  gcal: { name: "Google Calendar", source: "Calendar" },
  "outlook-cal": { name: "Outlook Calendar", source: "Calendar" },
  slack: { name: "Slack", source: "Slack" },
  upload: { name: "Uploads", source: "Photos" },
};

function prettyDate(iso: string): string {
  if (!iso) return "never";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "never";
  const mins = Math.floor((Date.now() - t) / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins} min ago`;
  if (mins < 1440) return `${Math.floor(mins / 60)} h ago`;
  return new Date(t).toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

const MODES: { id: SearchMode; label: string; desc: string }[] = [
  { id: "hybrid", label: "Hybrid", desc: "Keyword + meaning. The best default." },
  { id: "keyword", label: "Keyword", desc: "Exact words only — fast and literal." },
  { id: "vector", label: "Meaning", desc: "Semantic only — finds related ideas." },
];

export function Settings({
  onBack,
  onSignOut,
}: {
  onBack: () => void;
  onSignOut: () => void;
}) {
  const [me, setMe] = useState<Me | null>(null);
  const [connectors, setConnectors] = useState<ConnectorInstanceStatus[] | null>(
    null,
  );
  const [connError, setConnError] = useState("");
  const [mode, setMode] = useState<SearchMode>(getSearchMode());

  const client = useMemo(
    () => new ConnectorClient({ baseUrl: "", getToken }),
    [],
  );

  useEffect(() => {
    let live = true;
    getMe()
      .then((m) => live && setMe(m))
      .catch(() => undefined);
    void refreshConnectors(live);
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function refreshConnectors(live = true) {
    try {
      const rows = await client.listConnectors();
      if (live) {
        setConnectors(rows);
        setConnError("");
      }
    } catch {
      if (live) {
        setConnectors([]);
        setConnError("Couldn't load your connected sources.");
      }
    }
  }

  function pickMode(m: SearchMode) {
    setMode(m);
    setSearchMode(m);
  }

  async function disconnect(id: string) {
    await client.deleteConnector(id).catch(() => undefined);
    await refreshConnectors();
  }

  return (
    <main className="min-h-screen">
      <header className="sticky top-0 z-10 flex items-center gap-3 border-b border-gline bg-white px-4 py-3">
        <button
          type="button"
          onClick={onBack}
          aria-label="Back to search"
          className="rounded-full p-1.5 text-gmuted hover:bg-gbg-soft"
        >
          <ArrowLeft className="size-5" />
        </button>
        <h1 className="text-[18px] font-medium text-gink">Settings</h1>
      </header>

      <div className="mx-auto max-w-[720px] space-y-6 px-4 py-7">
        {/* Account */}
        <Section title="Account">
          <div className="flex items-center gap-3">
            <Avatar name={me?.email ?? currentUser()} size={44} />
            <div className="min-w-0 flex-1">
              <p className="truncate text-[15px] text-gink">
                {me?.email ?? currentUser()}
              </p>
              {me && (
                <p className="truncate text-[12.5px] text-gmuted">
                  Tenant {me.tenantId.slice(0, 8)} · isolated to you
                </p>
              )}
            </div>
            <button
              type="button"
              onClick={onSignOut}
              className="inline-flex shrink-0 items-center gap-1.5 rounded-full border border-gline px-3 py-1.5 text-[13px] text-gblue hover:bg-gbg-soft"
            >
              <LogOut className="size-3.5" /> Sign out
            </button>
          </div>
        </Section>

        {/* Connected sources */}
        <Section title="Connected sources">
          {connectors === null ? (
            <div className="flex items-center gap-2 text-[14px] text-gmuted">
              <Loader2 className="size-4 animate-spin" /> Loading…
            </div>
          ) : connectors.length === 0 ? (
            <p className="text-[14px] text-gmuted">
              {connError || "No sources connected yet."}
            </p>
          ) : (
            <ul className="divide-y divide-gline">
              {connectors.map(({ instance, sync }) => {
                const meta = CONNECTOR_LABEL[instance.connector_id] ?? {
                  name: instance.display_name || instance.connector_id,
                  source: "Drive" as SourceName,
                };
                return (
                  <li
                    key={instance.id}
                    className="flex items-center gap-3 py-3 first:pt-0 last:pb-0"
                  >
                    <SourceIcon
                      source={meta.source}
                      connectorId={instance.connector_id}
                      size={22}
                    />
                    <div className="min-w-0 flex-1">
                      <p className="text-[14.5px] text-gink">{meta.name}</p>
                      <p className="text-[12.5px] text-gmuted">
                        {sync.docs_emitted.toLocaleString()} items · synced{" "}
                        {prettyDate(sync.last_sync_completed)}
                        {sync.last_error ? " · error" : ""}
                      </p>
                    </div>
                    <StatusPill status={instance.status} error={sync.last_error} />
                    <button
                      type="button"
                      onClick={() => void disconnect(instance.id)}
                      className="shrink-0 rounded-full px-2.5 py-1 text-[12.5px] text-gmuted hover:bg-gbg-soft hover:text-[#d93025]"
                    >
                      Disconnect
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
          <div className="mt-4 flex flex-wrap items-center gap-2">
            <AddSource
              client={client}
              onChanged={() => void refreshConnectors()}
            />
            <button
              type="button"
              onClick={() => void refreshConnectors()}
              className="inline-flex items-center gap-1.5 rounded-full px-3 py-1.5 text-[13px] text-gmuted hover:bg-gbg-soft"
            >
              <RotateCw className="size-3.5" /> Refresh
            </button>
          </div>
        </Section>

        {/* Search */}
        <Section title="Search">
          <p className="mb-2 text-[13px] text-gmuted">Default search mode</p>
          <div className="space-y-1.5">
            {MODES.map((m) => (
              <button
                key={m.id}
                type="button"
                onClick={() => pickMode(m.id)}
                className={[
                  "flex w-full items-center gap-3 rounded-xl border px-3 py-2.5 text-left",
                  mode === m.id
                    ? "border-gblue bg-gblue/5"
                    : "border-gline hover:bg-gbg-soft",
                ].join(" ")}
              >
                <span
                  className={[
                    "mt-0.5 flex size-4 shrink-0 items-center justify-center rounded-full border",
                    mode === m.id ? "border-gblue" : "border-gmuted",
                  ].join(" ")}
                >
                  {mode === m.id && (
                    <span className="size-2 rounded-full bg-gblue" />
                  )}
                </span>
                <span className="min-w-0">
                  <span className="block text-[14.5px] text-gink">{m.label}</span>
                  <span className="block text-[12.5px] text-gmuted">{m.desc}</span>
                </span>
              </button>
            ))}
          </div>
        </Section>

        {/* Personalization */}
        <Personalization />

        {/* Privacy & data */}
        <Section title="Privacy & data">
          <p className="flex items-center gap-2 text-[14px] text-gink">
            <Lock className="size-4 text-gprov" />
            Your data is private — only you can see it.
          </p>
          <p className="mt-1 text-[13px] text-gmuted">
            Every result is attributed to a source you control, isolated to your
            tenant, and encrypted per-tenant at rest.
          </p>
          <DangerZone />
        </Section>
      </div>
    </main>
  );
}

function StatusPill({ status, error }: { status: string; error: string }) {
  const bad = error !== "" || status.toLowerCase().includes("error");
  const syncing = status.toLowerCase().includes("sync");
  const label = bad ? "Error" : syncing ? "Syncing" : status || "Active";
  const color = bad
    ? "bg-[#fce8e6] text-[#c5221f]"
    : syncing
      ? "bg-[#fef7e0] text-[#b06000]"
      : "bg-[#e6f4ea] text-[#137333]";
  return (
    <span className={`shrink-0 rounded-full px-2 py-0.5 text-[11.5px] ${color}`}>
      {label}
    </span>
  );
}

/** The GDPR per-tenant erase — type-to-confirm because it is irreversible. */
function DangerZone() {
  const [open, setOpen] = useState(false);
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  const [error, setError] = useState("");
  const closeRef = useRef<HTMLButtonElement>(null);

  function run() {
    setBusy(true);
    setError("");
    deleteMyData()
      .then(() => {
        setBusy(false);
        setDone(true);
      })
      .catch((e: unknown) => {
        setBusy(false);
        setError(e instanceof Error ? e.message : "Delete failed.");
      });
  }

  return (
    <div className="mt-5 rounded-xl border border-[#f3c9c5] p-4">
      <p className="flex items-center gap-2 text-[14px] font-medium text-[#c5221f]">
        <AlertTriangle className="size-4" /> Delete all my data
      </p>
      <p className="mt-1 text-[13px] text-gmuted">
        Permanently erases everything indexed for your tenant and crypto-shreds
        your encryption key. This cannot be undone.
      </p>
      <button
        type="button"
        onClick={() => {
          setOpen(true);
          setText("");
          setDone(false);
          setError("");
        }}
        className="mt-3 inline-flex items-center gap-1.5 rounded-full border border-[#f3c9c5] px-3 py-1.5 text-[13px] text-[#c5221f] hover:bg-[#fce8e6]"
      >
        <Trash2 className="size-3.5" /> Delete everything
      </button>

      {open && (
        <div
          className="fixed inset-0 z-40 flex items-center justify-center bg-black/30 p-4"
          role="dialog"
          aria-modal="true"
          aria-label="Confirm delete all data"
        >
          <div className="w-full max-w-[420px] rounded-2xl bg-white p-6 shadow-xl">
            {done ? (
              <>
                <p className="flex items-center gap-2 text-[16px] font-medium text-gink">
                  <Check className="size-5 text-gprov" /> Your data was erased.
                </p>
                <p className="mt-2 text-[13px] text-gmuted">
                  Everything indexed for your tenant is gone and your key was
                  crypto-shredded.
                </p>
                <button
                  ref={closeRef}
                  type="button"
                  onClick={() => setOpen(false)}
                  className="mt-4 w-full rounded-full bg-gblue py-2 text-[14px] font-medium text-white"
                >
                  Done
                </button>
              </>
            ) : (
              <>
                <p className="text-[16px] font-medium text-gink">
                  Delete all your data?
                </p>
                <p className="mt-2 text-[13px] text-gmuted">
                  This erases all indexed content and disconnects every source.
                  Type <strong className="text-gink">DELETE</strong> to confirm.
                </p>
                <input
                  type="text"
                  value={text}
                  onChange={(e) => setText(e.target.value)}
                  placeholder="DELETE"
                  className="mt-3 w-full rounded-lg border border-gline px-3 py-2 text-[14px] outline-none focus:border-[#c5221f]"
                />
                {error !== "" && (
                  <p className="mt-2 text-[13px] text-[#c5221f]" role="alert">
                    {error}
                  </p>
                )}
                <div className="mt-4 flex gap-2">
                  <button
                    type="button"
                    onClick={() => setOpen(false)}
                    className="flex-1 rounded-full border border-gline py-2 text-[14px] text-gink hover:bg-gbg-soft"
                  >
                    Cancel
                  </button>
                  <button
                    type="button"
                    disabled={text !== "DELETE" || busy}
                    onClick={run}
                    className="flex-1 rounded-full bg-[#c5221f] py-2 text-[14px] font-medium text-white hover:brightness-95 disabled:opacity-40"
                  >
                    {busy ? "Deleting…" : "Delete everything"}
                  </button>
                </div>
              </>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

function requiredFilled(
  type: ConnectorType,
  values: Record<string, string>,
): boolean {
  return type.fields
    .filter((f) => f.required)
    .every((f) => (values[f.key] ?? "").trim() !== "");
}

/** Connect a new source: pick a type, fill required config, create the instance,
 * and (for OAuth connectors) finish in a new tab — the gateway returns the OAuth
 * flow to the main web app, and a pre-opened tab dodges popup blockers. */
function AddSource({
  client,
  onChanged,
}: {
  client: ConnectorClient;
  onChanged: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [type, setType] = useState<ConnectorType | null>(null);
  const [values, setValues] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [info, setInfo] = useState("");

  function close() {
    setOpen(false);
    setType(null);
    setValues({});
    setError("");
    setInfo("");
  }

  function connect() {
    if (!type) {
      return;
    }
    setBusy(true);
    setError("");
    setInfo("");
    // Open the OAuth tab synchronously (in the click) so it isn't popup-blocked.
    const tab = type.oauthProvider ? window.open("", "_blank") : null;
    client
      .createConnector(type.id, type.displayName, buildConfig(type, values))
      .then(async (inst) => {
        onChanged(); // the new instance appears immediately
        if (type.oauthProvider) {
          const url = await client.startOAuth(inst.id);
          if (tab) {
            tab.location.href = url;
          } else {
            window.open(url, "_blank", "noopener");
          }
          setInfo(
            `Finish connecting with ${oauthProviderLabel(type.oauthProvider)} in the new tab, then Refresh.`,
          );
        } else {
          setInfo(`${type.displayName} connected.`);
        }
        setType(null);
        setValues({});
      })
      .catch((e: unknown) => {
        tab?.close();
        setError(e instanceof Error ? e.message : "Couldn't connect.");
      })
      .finally(() => setBusy(false));
  }

  if (!open) {
    return (
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="inline-flex items-center gap-1.5 rounded-full bg-gblue px-3 py-1.5 text-[13px] font-medium text-white hover:brightness-95"
      >
        <Plus className="size-3.5" /> Connect a source
      </button>
    );
  }

  return (
    <div className="w-full rounded-xl border border-gline p-4">
      {type === null ? (
        <>
          <div className="mb-3 flex items-center justify-between">
            <span className="text-[13px] font-medium text-gink">
              Choose a source
            </span>
            <button
              type="button"
              onClick={close}
              className="text-[13px] text-gmuted hover:underline"
            >
              Cancel
            </button>
          </div>
          <div className="grid grid-cols-2 gap-2 sm:grid-cols-3">
            {CONNECTOR_CATALOG.map((t) => (
              <button
                key={t.id}
                type="button"
                onClick={() => {
                  setType(t);
                  setValues({});
                  setError("");
                }}
                className="flex items-center gap-2 rounded-lg border border-gline px-3 py-2 text-left text-[13.5px] text-gink hover:bg-gbg-soft"
              >
                <SourceIcon
                  source={CONNECTOR_LABEL[t.id]?.source ?? "Drive"}
                  connectorId={t.id}
                  size={18}
                />
                <span className="truncate">{t.displayName}</span>
              </button>
            ))}
          </div>
        </>
      ) : (
        <>
          <div className="mb-3 flex items-center gap-2">
            <button
              type="button"
              onClick={() => setType(null)}
              aria-label="Back"
              className="rounded-full p-1 text-gmuted hover:bg-gbg-soft"
            >
              <ArrowLeft className="size-4" />
            </button>
            <SourceIcon
              source={CONNECTOR_LABEL[type.id]?.source ?? "Drive"}
              connectorId={type.id}
              size={18}
            />
            <span className="text-[14.5px] font-medium text-gink">
              {type.displayName}
            </span>
          </div>
          {type.fields.length > 0 && (
            <div className="space-y-2.5">
              {type.fields.map((f) => (
                <label key={f.key} className="block">
                  <span className="mb-1 block text-[12px] text-gmuted">
                    {f.label}
                    {f.required && <span className="text-[#d93025]"> *</span>}
                  </span>
                  <input
                    type="text"
                    value={values[f.key] ?? ""}
                    placeholder={f.placeholder}
                    autoComplete="off"
                    onChange={(e) =>
                      setValues((v) => ({ ...v, [f.key]: e.target.value }))
                    }
                    className="w-full rounded-lg border border-gline px-3 py-2 text-[14px] text-gink outline-none focus:border-gblue"
                  />
                </label>
              ))}
            </div>
          )}
          {error !== "" && (
            <p className="mt-2 text-[13px] text-[#c5221f]" role="alert">
              {error}
            </p>
          )}
          <div className="mt-3 flex gap-2">
            <button
              type="button"
              onClick={close}
              className="rounded-full border border-gline px-4 py-1.5 text-[13px] text-gink hover:bg-gbg-soft"
            >
              Cancel
            </button>
            <button
              type="button"
              disabled={busy || !requiredFilled(type, values)}
              onClick={connect}
              className="rounded-full bg-gblue px-4 py-1.5 text-[13px] font-medium text-white hover:brightness-95 disabled:opacity-40"
            >
              {busy
                ? "Connecting…"
                : type.oauthProvider
                  ? `Connect with ${oauthProviderLabel(type.oauthProvider)}`
                  : "Connect"}
            </button>
          </div>
        </>
      )}
      {info !== "" && (
        <p className="mt-3 flex items-center gap-1.5 text-[13px] text-gprov">
          <Check className="size-3.5" /> {info}
        </p>
      )}
    </div>
  );
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="rounded-2xl border border-gline p-5">
      <h2 className="mb-4 text-[13px] font-semibold uppercase tracking-wide text-gmuted">
        {title}
      </h2>
      {children}
    </section>
  );
}

// --- Personalization (v3.2): one control per preference field, plus the
// mandatory transparency controls (pause, reset, export). Loads the resolved
// profile on mount and persists every change via savePreferences. -------------

/** The user-facing priority sources, mapped to their DocType source-weight key.
 * A "priority" toggle nudges the weight above 1.0; off resets it to the 1.0
 * default (the gateway treats a missing key as 1.0). */
const PRIORITY_SOURCES: { key: DocType; label: string; source: SourceName }[] = [
  { key: "EMAIL", label: "Email", source: "Gmail" },
  { key: "CHAT_MESSAGE", label: "Messages", source: "Slack" },
  { key: "FILE", label: "Files", source: "Drive" },
  { key: "CALENDAR_EVENT", label: "Calendar", source: "Calendar" },
];
const PRIORITY_WEIGHT = 2.0;

/** A short, curated IANA timezone list (the backend validates any value). */
const TIMEZONES = [
  "",
  "America/Los_Angeles",
  "America/Denver",
  "America/Chicago",
  "America/New_York",
  "UTC",
  "Europe/London",
  "Europe/Berlin",
  "Asia/Kolkata",
  "Asia/Singapore",
  "Asia/Tokyo",
  "Australia/Sydney",
];

function Personalization() {
  const [profile, setProfile] = useState<Profile | null>(null);
  const [sampleCount, setSampleCount] = useState(0);
  const [loadError, setLoadError] = useState("");
  const [saveError, setSaveError] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    let live = true;
    getPreferences()
      .then((r) => {
        if (live) {
          setProfile(r.profile);
          setSampleCount(r.sampleCount);
        }
      })
      .catch(() => {
        if (live) {
          setProfile(defaultProfile());
          setLoadError("Couldn't load your preferences — showing defaults.");
        }
      });
    return () => {
      live = false;
    };
  }, []);

  // Apply a change and persist it. Optimistic: the control reflects the edit
  // immediately; a failed save surfaces an inline message but keeps the edit.
  function update(mutate: (p: Profile) => Profile) {
    setProfile((prev) => {
      if (!prev) {
        return prev;
      }
      const next = mutate(prev);
      setSaving(true);
      setSaveError("");
      savePreferences(next)
        .then((version) => {
          setProfile((cur) => (cur ? { ...cur, version } : cur));
        })
        .catch((e: unknown) => {
          setSaveError(e instanceof Error ? e.message : "Couldn't save changes.");
        })
        .finally(() => setSaving(false));
      return next;
    });
  }

  if (profile === null) {
    return (
      <Section title="Personalization">
        <div className="flex items-center gap-2 text-[14px] text-gmuted">
          <Loader2 className="size-4 animate-spin" /> Loading your preferences…
        </div>
      </Section>
    );
  }

  return (
    <Section title="Personalization">
      <p className="-mt-1 mb-4 text-[13px] text-gmuted">
        Tune how results are ranked for you. Every control feeds the ranker —
        nothing here is cosmetic.{" "}
        {saving && (
          <span className="inline-flex items-center gap-1 text-gmuted">
            <Loader2 className="size-3 animate-spin" /> Saving…
          </span>
        )}
      </p>
      {loadError !== "" && (
        <p className="mb-3 text-[13px] text-[#b06000]" role="status">
          {loadError}
        </p>
      )}
      {saveError !== "" && (
        <p className="mb-3 text-[13px] text-[#c5221f]" role="alert">
          {saveError}
        </p>
      )}

      <div className="space-y-6">
        {/* Priority sources */}
        <Field
          label="Priority sources"
          hint="Boost results from the sources that matter most to you."
        >
          <div className="flex flex-wrap gap-2">
            {PRIORITY_SOURCES.map((s) => {
              const on = (profile.source_weights[s.key] ?? 1) > 1;
              return (
                <button
                  key={s.key}
                  type="button"
                  aria-pressed={on}
                  onClick={() =>
                    update((p) => {
                      const weights = { ...p.source_weights };
                      if (on) {
                        delete weights[s.key];
                      } else {
                        weights[s.key] = PRIORITY_WEIGHT;
                      }
                      return { ...p, source_weights: weights };
                    })
                  }
                  className={[
                    "inline-flex items-center gap-1.5 rounded-full border px-3 py-1.5 text-[13px]",
                    on
                      ? "border-gblue bg-gblue/5 text-gblue"
                      : "border-gline text-gink hover:bg-gbg-soft",
                  ].join(" ")}
                >
                  <SourceIcon source={s.source} size={15} /> {s.label}
                </button>
              );
            })}
          </div>
        </Field>

        {/* Important people */}
        <Field
          label="Important people"
          hint="Results involving these people are boosted. Use an email or name."
        >
          <TagList
            values={profile.important_people}
            placeholder="alice@example.com"
            ariaLabel="Add an important person"
            onChange={(important_people) =>
              update((p) => ({ ...p, important_people }))
            }
          />
        </Field>

        {/* Topics */}
        <Field
          label="Topics & projects"
          hint="Keywords, projects, or interests to favor in ranking."
        >
          <TagList
            values={profile.topics}
            placeholder="q3 planning"
            ariaLabel="Add a topic"
            onChange={(topics) => update((p) => ({ ...p, topics }))}
          />
        </Field>

        {/* Mute */}
        <Field
          label="Muted people"
          hint="Down-rank or hide results involving these people."
        >
          <TagList
            values={profile.mute.people}
            placeholder="noreply@example.com"
            ariaLabel="Mute a person"
            onChange={(people) =>
              update((p) => ({ ...p, mute: { ...p.mute, people } }))
            }
          />
        </Field>
        <Field label="Muted topics" hint="Hide results matching these keywords.">
          <TagList
            values={profile.mute.topics}
            placeholder="newsletter"
            ariaLabel="Mute a topic"
            onChange={(topics) =>
              update((p) => ({ ...p, mute: { ...p.mute, topics } }))
            }
          />
        </Field>
        <Field label="Muted sources" hint="Turn off a whole source in ranking.">
          <div className="flex flex-wrap gap-2">
            {PRIORITY_SOURCES.map((s) => {
              const muted = profile.mute.sources.includes(s.key);
              return (
                <button
                  key={s.key}
                  type="button"
                  aria-pressed={muted}
                  onClick={() =>
                    update((p) => {
                      const sources = muted
                        ? p.mute.sources.filter((x) => x !== s.key)
                        : [...p.mute.sources, s.key];
                      return { ...p, mute: { ...p.mute, sources } };
                    })
                  }
                  className={[
                    "inline-flex items-center gap-1.5 rounded-full border px-3 py-1.5 text-[13px]",
                    muted
                      ? "border-[#f3c9c5] bg-[#fce8e6] text-[#c5221f]"
                      : "border-gline text-gink hover:bg-gbg-soft",
                  ].join(" ")}
                >
                  <SourceIcon source={s.source} size={15} /> {s.label}
                </button>
              );
            })}
          </div>
        </Field>

        {/* Working hours + timezone */}
        <Field
          label="Working hours"
          hint="Grounds “this week” and what counts as urgent."
        >
          <div className="flex flex-wrap items-center gap-2 text-[14px] text-gink">
            <HourSelect
              ariaLabel="Working hours start"
              value={profile.working_hours.start_hour}
              max={23}
              onChange={(start_hour) =>
                update((p) => ({
                  ...p,
                  working_hours: { ...p.working_hours, start_hour },
                }))
              }
            />
            <span className="text-gmuted">to</span>
            <HourSelect
              ariaLabel="Working hours end"
              value={profile.working_hours.end_hour}
              max={24}
              onChange={(end_hour) =>
                update((p) => ({
                  ...p,
                  working_hours: { ...p.working_hours, end_hour },
                }))
              }
            />
            <select
              aria-label="Timezone"
              value={profile.timezone}
              onChange={(e) =>
                update((p) => ({ ...p, timezone: e.target.value }))
              }
              className="rounded-lg border border-gline px-2.5 py-1.5 text-[13.5px] text-gink outline-none focus:border-gblue"
            >
              {TIMEZONES.map((tz) => (
                <option key={tz || "default"} value={tz}>
                  {tz === "" ? "Default (UTC)" : tz}
                </option>
              ))}
            </select>
          </div>
        </Field>

        {/* Sliders */}
        <Slider
          label="Attention sensitivity"
          lo="Show me everything"
          hi="Only the critical few"
          value={profile.attention_sensitivity}
          onChange={(attention_sensitivity) =>
            update((p) => ({ ...p, attention_sensitivity }))
          }
        />
        <Slider
          label="Recency vs. importance"
          lo="Importance"
          hi="Recency"
          value={profile.recency_vs_importance}
          onChange={(recency_vs_importance) =>
            update((p) => ({ ...p, recency_vs_importance }))
          }
        />
        <Slider
          label="Novelty vs. familiarity"
          lo="Familiar"
          hi="Novel"
          value={profile.novelty_vs_familiarity}
          onChange={(novelty_vs_familiarity) =>
            update((p) => ({ ...p, novelty_vs_familiarity }))
          }
        />
      </div>

      {/* Transparency controls */}
      <div className="mt-6 border-t border-gline pt-5">
        <label className="flex items-center justify-between gap-3">
          <span className="min-w-0">
            <span className="block text-[14px] text-gink">Pause learning</span>
            <span className="block text-[12.5px] text-gmuted">
              Freeze the model — your feedback won’t change ranking while paused.
            </span>
          </span>
          <Toggle
            checked={profile.learning_paused}
            ariaLabel="Pause learning"
            onChange={(learning_paused) =>
              update((p) => ({ ...p, learning_paused }))
            }
          />
        </label>

        <p className="mt-4 text-[13px] text-gmuted" data-testid="sample-count">
          Learned from {sampleCount.toLocaleString()} interaction
          {sampleCount === 1 ? "" : "s"}.
        </p>

        <div className="mt-3 flex flex-wrap gap-2">
          <ExportButton />
          <ResetLearning onReset={() => setSampleCount(0)} />
        </div>
      </div>
    </Section>
  );
}

function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: ReactNode;
}) {
  return (
    <div>
      <p className="text-[14px] font-medium text-gink">{label}</p>
      {hint && <p className="mb-2 mt-0.5 text-[12.5px] text-gmuted">{hint}</p>}
      <div className={hint ? "" : "mt-2"}>{children}</div>
    </div>
  );
}

/** An add/remove chip list — Enter or the + button adds; × removes. */
function TagList({
  values,
  placeholder,
  ariaLabel,
  onChange,
}: {
  values: string[];
  placeholder: string;
  ariaLabel: string;
  onChange: (next: string[]) => void;
}) {
  const [draft, setDraft] = useState("");

  function add() {
    const v = draft.trim();
    if (v === "") {
      return;
    }
    if (!values.some((x) => x.toLowerCase() === v.toLowerCase())) {
      onChange([...values, v]);
    }
    setDraft("");
  }

  return (
    <div>
      {values.length > 0 && (
        <ul className="mb-2 flex flex-wrap gap-1.5">
          {values.map((v) => (
            <li
              key={v}
              className="inline-flex items-center gap-1 rounded-full bg-gbg-soft px-2.5 py-1 text-[13px] text-gink"
            >
              <span className="truncate">{v}</span>
              <button
                type="button"
                aria-label={`Remove ${v}`}
                onClick={() => onChange(values.filter((x) => x !== v))}
                className="rounded-full text-gmuted hover:text-[#c5221f]"
              >
                <X className="size-3.5" />
              </button>
            </li>
          ))}
        </ul>
      )}
      <div className="flex gap-2">
        <input
          type="text"
          value={draft}
          placeholder={placeholder}
          aria-label={ariaLabel}
          autoComplete="off"
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              add();
            }
          }}
          className="min-w-0 flex-1 rounded-lg border border-gline px-3 py-2 text-[14px] text-gink outline-none focus:border-gblue"
        />
        <button
          type="button"
          onClick={add}
          disabled={draft.trim() === ""}
          className="inline-flex shrink-0 items-center gap-1 rounded-lg border border-gline px-3 py-2 text-[13px] text-gblue hover:bg-gbg-soft disabled:opacity-40"
        >
          <Plus className="size-3.5" /> Add
        </button>
      </div>
    </div>
  );
}

function HourSelect({
  value,
  max,
  ariaLabel,
  onChange,
}: {
  value: number;
  max: number;
  ariaLabel: string;
  onChange: (h: number) => void;
}) {
  return (
    <select
      aria-label={ariaLabel}
      value={value}
      onChange={(e) => onChange(Number(e.target.value))}
      className="rounded-lg border border-gline px-2.5 py-1.5 text-[13.5px] text-gink outline-none focus:border-gblue"
    >
      {Array.from({ length: max + 1 }, (_, h) => (
        <option key={h} value={h}>
          {h.toString().padStart(2, "0")}:00
        </option>
      ))}
    </select>
  );
}

function Slider({
  label,
  lo,
  hi,
  value,
  onChange,
}: {
  label: string;
  lo: string;
  hi: string;
  value: number;
  onChange: (v: number) => void;
}) {
  return (
    <div>
      <p className="text-[14px] font-medium text-gink">{label}</p>
      <input
        type="range"
        min={0}
        max={1}
        step={0.05}
        value={value}
        aria-label={label}
        onChange={(e) => onChange(Number(e.target.value))}
        className="mt-2 w-full accent-gblue"
      />
      <div className="mt-0.5 flex justify-between text-[12px] text-gmuted">
        <span>{lo}</span>
        <span>{hi}</span>
      </div>
    </div>
  );
}

function Toggle({
  checked,
  ariaLabel,
  onChange,
}: {
  checked: boolean;
  ariaLabel: string;
  onChange: (next: boolean) => void;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={ariaLabel}
      onClick={() => onChange(!checked)}
      className={[
        "relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition-colors",
        checked ? "bg-gblue" : "bg-gline",
      ].join(" ")}
    >
      <span
        className={[
          "inline-block size-5 transform rounded-full bg-white shadow transition-transform",
          checked ? "translate-x-5" : "translate-x-0.5",
        ].join(" ")}
      />
    </button>
  );
}

/** Export everything the engine has personalized for you (data rights). */
function ExportButton() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  function run() {
    setBusy(true);
    setError("");
    exportPersonalization()
      .then((data) => {
        const blob = new Blob([JSON.stringify(data, null, 2)], {
          type: "application/json",
        });
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = "asker-personalization.json";
        a.click();
        URL.revokeObjectURL(url);
      })
      .catch((e: unknown) => {
        setError(e instanceof Error ? e.message : "Export failed.");
      })
      .finally(() => setBusy(false));
  }

  return (
    <span className="inline-flex flex-col">
      <button
        type="button"
        onClick={run}
        disabled={busy}
        className="inline-flex items-center gap-1.5 rounded-full border border-gline px-3 py-1.5 text-[13px] text-gink hover:bg-gbg-soft disabled:opacity-50"
      >
        <Download className="size-3.5" /> {busy ? "Exporting…" : "Export my data"}
      </button>
      {error !== "" && (
        <span className="mt-1 text-[12.5px] text-[#c5221f]" role="alert">
          {error}
        </span>
      )}
    </span>
  );
}

/** Reset what we've learned — confirm modal (it erases feedback history). */
function ResetLearning({ onReset }: { onReset: () => void }) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [doneCount, setDoneCount] = useState<number | null>(null);

  function run() {
    setBusy(true);
    setError("");
    resetLearning()
      .then((deleted) => {
        setDoneCount(deleted);
        onReset();
      })
      .catch((e: unknown) => {
        setError(e instanceof Error ? e.message : "Reset failed.");
      })
      .finally(() => setBusy(false));
  }

  return (
    <>
      <button
        type="button"
        onClick={() => {
          setOpen(true);
          setError("");
          setDoneCount(null);
        }}
        className="inline-flex items-center gap-1.5 rounded-full border border-[#f3c9c5] px-3 py-1.5 text-[13px] text-[#c5221f] hover:bg-[#fce8e6]"
      >
        <RotateCw className="size-3.5" /> Reset what you’ve learned about me
      </button>

      {open && (
        <div
          className="fixed inset-0 z-40 flex items-center justify-center bg-black/30 p-4"
          role="dialog"
          aria-modal="true"
          aria-label="Confirm reset learning"
        >
          <div className="w-full max-w-[420px] rounded-2xl bg-white p-6 shadow-xl">
            {doneCount !== null ? (
              <>
                <p className="flex items-center gap-2 text-[16px] font-medium text-gink">
                  <Check className="size-5 text-gprov" /> Learning reset.
                </p>
                <p className="mt-2 text-[13px] text-gmuted">
                  Cleared {doneCount.toLocaleString()} feedback signal
                  {doneCount === 1 ? "" : "s"}. Ranking is back to your explicit
                  preferences and the defaults.
                </p>
                <button
                  type="button"
                  onClick={() => setOpen(false)}
                  className="mt-4 w-full rounded-full bg-gblue py-2 text-[14px] font-medium text-white"
                >
                  Done
                </button>
              </>
            ) : (
              <>
                <p className="text-[16px] font-medium text-gink">
                  Reset what you’ve learned?
                </p>
                <p className="mt-2 text-[13px] text-gmuted">
                  This erases the behavioral model built from your clicks and
                  feedback. Your explicit preferences above are kept. This can’t
                  be undone.
                </p>
                {error !== "" && (
                  <p className="mt-2 text-[13px] text-[#c5221f]" role="alert">
                    {error}
                  </p>
                )}
                <div className="mt-4 flex gap-2">
                  <button
                    type="button"
                    onClick={() => setOpen(false)}
                    className="flex-1 rounded-full border border-gline py-2 text-[14px] text-gink hover:bg-gbg-soft"
                  >
                    Cancel
                  </button>
                  <button
                    type="button"
                    disabled={busy}
                    onClick={run}
                    className="flex-1 rounded-full bg-[#c5221f] py-2 text-[14px] font-medium text-white hover:brightness-95 disabled:opacity-40"
                  >
                    {busy ? "Resetting…" : "Reset learning"}
                  </button>
                </div>
              </>
            )}
          </div>
        </div>
      )}
    </>
  );
}
