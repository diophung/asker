import { useCallback, useEffect, useState } from "react";
import {
  type ConnectorClient,
  type ConnectorInstanceStatus,
  isAbortError,
} from "../api";
import { catalogByCategory, type ConnectorType } from "./catalog";
import { ConnectForm } from "./ConnectForm";
import { InstanceList } from "./InstanceList";

/** Poll interval for live sync-progress updates. */
const POLL_INTERVAL_MS = 15000;

type LoadStatus = "loading" | "ready" | "error";

/**
 * Connectors management view: a catalog of connectable source types, a Connect
 * form, and the tenant's existing instances with live sync status. The instance
 * list polls listConnectors every ~15s so sync progress updates without a
 * manual refresh.
 */
export function ConnectorsPage({ client }: { client: ConnectorClient }) {
  const [instances, setInstances] = useState<ConnectorInstanceStatus[]>([]);
  const [status, setStatus] = useState<LoadStatus>("loading");
  const [errorMsg, setErrorMsg] = useState("");
  const [selected, setSelected] = useState<ConnectorType | null>(null);

  // Stable so the polling effect doesn't re-subscribe on every render.
  const refresh = useCallback(
    async (signal?: AbortSignal): Promise<void> => {
      try {
        const rows = await client.listConnectors(signal);
        setInstances(rows);
        setStatus("ready");
      } catch (err) {
        if (isAbortError(err)) {
          return;
        }
        setErrorMsg(err instanceof Error ? err.message : String(err));
        setStatus("error");
      }
    },
    [client],
  );

  useEffect(() => {
    const controller = new AbortController();
    void refresh(controller.signal);
    const timer = setInterval(() => void refresh(controller.signal), POLL_INTERVAL_MS);
    return () => {
      controller.abort();
      clearInterval(timer);
    };
  }, [refresh]);

  async function handleConnect(
    type: ConnectorType,
    displayName: string,
    config: Record<string, unknown>,
    token: string,
  ): Promise<void> {
    const created = await client.createConnector(type.id, displayName, config);
    if (type.needsToken && token !== "") {
      await client.putConnectorToken(created.id, token);
    }
    setSelected(null);
    await refresh();
  }

  async function handleDelete(id: string): Promise<void> {
    try {
      await client.deleteConnector(id);
    } catch (err) {
      if (!isAbortError(err)) {
        setErrorMsg(err instanceof Error ? err.message : String(err));
        setStatus("error");
      }
    }
    await refresh();
  }

  return (
    <div className="connectors-page">
      <section className="connectors-catalog" aria-label="Available connectors">
        <h2 className="section-title">Add a source</h2>
        {catalogByCategory().map((group) => (
          <div key={group.category} className="catalog-group">
            <h3 className="catalog-category">{group.category}</h3>
            <div className="catalog-grid">
              {group.types.map((type) => (
                <button
                  key={type.id}
                  type="button"
                  className="catalog-item"
                  onClick={() => setSelected(type)}
                >
                  <span className="catalog-item-name">{type.displayName}</span>
                  <span className="catalog-item-connect">Connect</span>
                </button>
              ))}
            </div>
          </div>
        ))}
      </section>

      {selected !== null && (
        <ConnectForm
          type={selected}
          onSubmit={(displayName, config, token) =>
            handleConnect(selected, displayName, config, token)
          }
          onCancel={() => setSelected(null)}
        />
      )}

      <section className="connectors-instances" aria-label="Your connectors">
        <h2 className="section-title">Your connectors</h2>
        {status === "loading" && (
          <div className="state-box" aria-busy="true">
            <p className="state-detail">Loading connectors…</p>
          </div>
        )}
        {status === "error" && (
          <div className="state-box state-error" role="alert">
            <p className="state-title">Could not load connectors</p>
            <p className="state-detail">{errorMsg}</p>
            <button type="button" onClick={() => void refresh()}>
              Retry
            </button>
          </div>
        )}
        {status === "ready" && (
          <InstanceList instances={instances} onDelete={handleDelete} />
        )}
      </section>
    </div>
  );
}
