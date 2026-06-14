import { type ReactNode, useEffect, useMemo, useRef, useState } from "react";
import {
  AlertTriangle,
  ArrowLeft,
  Check,
  Loader2,
  Lock,
  LogOut,
  Plus,
  RotateCw,
  Trash2,
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
  deleteMyData,
  getMe,
  getSearchMode,
  type Me,
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
