import type { ReactNode } from "react";
import {
  CalendarClock,
  FileText,
  Lock,
  Mail,
  MessageSquare,
  Users,
} from "lucide-react";
import type { Panel } from "./types";
import { Avatar } from "./ui";

/**
 * The clearest "this is MY graph, not the web's" moment — Google's knowledge
 * panel, built entirely from the user's own data. Quiet card, not a dashboard.
 */
export function KnowledgePanel({ panel }: { panel: Panel }) {
  return (
    <aside
      aria-label={`${panel.kind === "person" ? "Person" : "Project"} details`}
      className="rounded-2xl border border-gline p-5"
    >
      {panel.kind === "person" ? (
        <PersonCard panel={panel} />
      ) : (
        <ProjectCard panel={panel} />
      )}
      <div className="mt-4 flex items-center gap-1.5 border-t border-gline pt-3 text-[12px] text-gmuted">
        <Lock className="size-3" />
        From your data · only you can see this
      </div>
    </aside>
  );
}

function PersonCard({ panel }: { panel: Extract<Panel, { kind: "person" }> }) {
  return (
    <>
      <div className="flex items-center gap-3">
        <Avatar name={panel.name} size={56} />
        <div className="min-w-0">
          <h2 className="truncate text-[20px] font-normal text-gink">
            {panel.name}
          </h2>
          <p className="truncate text-[14px] text-gmuted">
            {panel.role} · {panel.org}
          </p>
        </div>
      </div>

      <div className="mt-4 flex gap-2">
        <ContactAction icon={Mail} label="Email" />
        <ContactAction icon={MessageSquare} label="Message" />
      </div>

      <Row label="Last contacted" value={panel.lastContacted} />
      {panel.upcomingMeeting && (
        <Row
          icon={CalendarClock}
          label="Upcoming"
          value={panel.upcomingMeeting}
        />
      )}

      <Section icon={FileText} title="Shared documents">
        <ul className="space-y-1">
          {panel.sharedDocs.map((d) => (
            <li key={d}>
              <a
                href={`#${d}`}
                onClick={(e) => e.preventDefault()}
                className="text-[14px] text-gtitle hover:underline"
              >
                {d}
              </a>
            </li>
          ))}
        </ul>
      </Section>
    </>
  );
}

function ProjectCard({ panel }: { panel: Extract<Panel, { kind: "project" }> }) {
  return (
    <>
      <h2 className="text-[20px] font-normal text-gink">{panel.name}</h2>
      <p className="mt-1 text-[14px] leading-[1.5] text-gsnippet">
        {panel.summary}
      </p>

      <Section icon={Users} title="Key people">
        <div className="flex flex-wrap gap-2">
          {panel.people.map((p) => (
            <span
              key={p}
              className="inline-flex items-center gap-1.5 rounded-full bg-gbg-soft px-2 py-1 text-[13px] text-gink"
            >
              <Avatar name={p} size={20} /> {p}
            </span>
          ))}
        </div>
      </Section>

      <Section icon={CalendarClock} title="Recent activity">
        <ul className="space-y-1 text-[13.5px] text-gsnippet">
          {panel.recentActivity.map((a) => (
            <li key={a}>{a}</li>
          ))}
        </ul>
      </Section>

      <Section icon={FileText} title="Related files">
        <ul className="space-y-1">
          {panel.files.map((f) => (
            <li key={f}>
              <a
                href={`#${f}`}
                onClick={(e) => e.preventDefault()}
                className="text-[14px] text-gtitle hover:underline"
              >
                {f}
              </a>
            </li>
          ))}
        </ul>
      </Section>
    </>
  );
}

function ContactAction({
  icon: Icon,
  label,
}: {
  icon: typeof Mail;
  label: string;
}) {
  return (
    <button
      type="button"
      className="inline-flex flex-1 items-center justify-center gap-1.5 rounded-full border border-gline py-1.5 text-[13px] text-gblue hover:bg-gbg-soft"
    >
      <Icon className="size-4" /> {label}
    </button>
  );
}

function Row({
  icon: Icon,
  label,
  value,
}: {
  icon?: typeof Mail;
  label: string;
  value: string;
}) {
  return (
    <div className="mt-3 flex items-baseline gap-2 text-[14px]">
      <span className="flex w-28 shrink-0 items-center gap-1.5 text-gmuted">
        {Icon && <Icon className="size-3.5" />}
        {label}
      </span>
      <span className="text-gink">{value}</span>
    </div>
  );
}

function Section({
  icon: Icon,
  title,
  children,
}: {
  icon: typeof Mail;
  title: string;
  children: ReactNode;
}) {
  return (
    <div className="mt-4">
      <h3 className="mb-1.5 flex items-center gap-1.5 text-[12px] font-semibold uppercase tracking-wide text-gmuted">
        <Icon className="size-3.5" />
        {title}
      </h3>
      {children}
    </div>
  );
}
