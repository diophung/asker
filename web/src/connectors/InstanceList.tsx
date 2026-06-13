import { useState } from "react";
import type { ConnectorInstanceStatus } from "../api";
import { connectorTypeLabel } from "./catalog";
import { relativeTime } from "./relativeTime";

export interface InstanceListProps {
  instances: ConnectorInstanceStatus[];
  /** Resolves when the delete completes; the page refreshes the list. */
  onDelete: (id: string) => Promise<void>;
}

/** Map a free-form status/phase string to a stable badge modifier class. */
function badgeClass(value: string): string {
  const v = value.toLowerCase();
  if (v.includes("error") || v.includes("fail")) {
    return "status-error";
  }
  if (v.includes("sync") || v.includes("run") || v.includes("active")) {
    return "status-active";
  }
  if (v.includes("idle") || v.includes("ready") || v.includes("done")) {
    return "status-idle";
  }
  return "status-neutral";
}

/** List of the tenant's connector instances with live sync status. */
export function InstanceList({ instances, onDelete }: InstanceListProps) {
  if (instances.length === 0) {
    return (
      <div className="state-box state-idle">
        <p className="state-detail">
          No connectors yet. Connect a source above to start indexing.
        </p>
      </div>
    );
  }

  return (
    <ul className="instance-list" aria-label="Connected sources">
      {instances.map((row) => (
        <InstanceRow key={row.instance.id} row={row} onDelete={onDelete} />
      ))}
    </ul>
  );
}

function InstanceRow({
  row,
  onDelete,
}: {
  row: ConnectorInstanceStatus;
  onDelete: (id: string) => Promise<void>;
}) {
  const { instance, sync } = row;
  const [confirming, setConfirming] = useState(false);
  const [deleting, setDeleting] = useState(false);

  function handleDelete() {
    setDeleting(true);
    onDelete(instance.id).finally(() => {
      setDeleting(false);
      setConfirming(false);
    });
  }

  const completed = relativeTime(sync.last_sync_completed);

  return (
    <li className="instance-card">
      <div className="instance-main">
        <div className="instance-titles">
          <span className="instance-name">{instance.display_name}</span>
          <span className="badge badge-connector">
            {connectorTypeLabel(instance.connector_id)}
          </span>
        </div>
        <div className="instance-status-row">
          <span className={`status-badge ${badgeClass(instance.status)}`}>
            {instance.status}
          </span>
          {sync.phase !== "" && (
            <span className="instance-meta">
              phase: <span className={badgeClass(sync.phase)}>{sync.phase}</span>
            </span>
          )}
          <span className="instance-meta">
            {sync.docs_emitted.toLocaleString()}{" "}
            {sync.docs_emitted === 1 ? "doc" : "docs"}
          </span>
          {completed !== "" && (
            <span className="instance-meta">synced {completed}</span>
          )}
        </div>
        {sync.last_error !== "" && (
          <p className="instance-error" role="alert">
            {sync.last_error}
          </p>
        )}
      </div>

      <div className="instance-actions">
        {confirming ? (
          <>
            <span className="instance-confirm-text">Delete?</span>
            <button
              type="button"
              className="danger-button"
              onClick={handleDelete}
              disabled={deleting}
            >
              {deleting ? "Deleting…" : "Confirm"}
            </button>
            <button
              type="button"
              className="secondary-button"
              onClick={() => setConfirming(false)}
              disabled={deleting}
            >
              Cancel
            </button>
          </>
        ) : (
          <button
            type="button"
            className="secondary-button"
            onClick={() => setConfirming(true)}
          >
            Delete
          </button>
        )}
      </div>
    </li>
  );
}
