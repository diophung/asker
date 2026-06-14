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
type OAuthResult = "connected" | "error" | null;

/**
 * Read the `oauth` result from the URL (the gateway callback redirects the
 * browser to /connectors?oauth=connected|error), then strip it so a reload or
 * back/forward doesn't re-show the banner. Returns null when absent/unknown.
 */
function consumeOAuthResult(): OAuthResult {
  if (typeof window === "undefined") {
    return null;
  }
  const params = new URLSearchParams(window.location.search);
  const value = params.get("oauth");
  if (value !== "connected" && value !== "error") {
    return null;
  }
  params.delete("oauth");
  const query = params.toString();
  const url =
    window.location.pathname + (query === "" ? "" : `?${query}`) +
    window.location.hash;
  window.history.replaceState(null, "", url);
  return value;
}

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
  // Banner for the result of a provider OAuth round-trip (?oauth=…).
  const [oauthResult, setOAuthResult] = useState<OAuthResult>(null);

  // The provider redirect lands us back here with ?oauth=…; read it once on
  // mount and clear the param so it doesn't survive a reload.
  useEffect(() => {
    setOAuthResult(consumeOAuthResult());
  }, []);

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

  // Create the instance and, for the dev paste-a-token path, store the token.
  // Returns the new instance id so the form can drive the OAuth sign-in step.
  async function handleConnect(
    type: ConnectorType,
    displayName: string,
    config: Record<string, unknown>,
    token: string,
  ): Promise<string> {
    const created = await client.createConnector(type.id, displayName, config);
    if (type.needsToken && token !== "") {
      await client.putConnectorToken(created.id, token);
    }
    await refresh();
    return created.id;
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
      {oauthResult === "connected" && (
        <div className="oauth-banner oauth-banner-success" role="status">
          <p className="state-detail">Account connected. Syncing will begin shortly.</p>
          <button
            type="button"
            className="link-button"
            onClick={() => setOAuthResult(null)}
          >
            Dismiss
          </button>
        </div>
      )}
      {oauthResult === "error" && (
        <div className="oauth-banner oauth-banner-error" role="alert">
          <p className="state-detail">
            Could not connect the account. Please try again.
          </p>
          <button
            type="button"
            className="link-button"
            onClick={() => setOAuthResult(null)}
          >
            Dismiss
          </button>
        </div>
      )}

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
          onStartOAuth={(id) => client.startOAuth(id)}
          onDone={() => setSelected(null)}
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
