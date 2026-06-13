import { type FormEvent, useState } from "react";
import {
  buildConfig,
  type ConnectorType,
  oauthProviderLabel,
} from "./catalog";

export interface ConnectFormProps {
  type: ConnectorType;
  /**
   * Creates the connector instance from the form values and resolves with the
   * new instance id (the page wires this to createConnector, then — for the
   * dev paste-a-token path — putConnectorToken). Rejects with an Error.
   */
  onSubmit: (
    displayName: string,
    config: Record<string, unknown>,
    token: string,
  ) => Promise<string>;
  /**
   * Begins the real OAuth flow for a just-created instance: resolves with the
   * provider authorize URL the browser should navigate to. Only used for
   * connectors with an `oauthProvider`.
   */
  onStartOAuth?: (id: string) => Promise<string>;
  /** Navigates the top-level browser to the authorize URL. */
  onNavigate?: (url: string) => void;
  /** Closes the form (e.g. after a non-OAuth connect completes). */
  onDone: () => void;
  onCancel: () => void;
}

/** Top-level navigation; injectable so tests can assert without a real reload. */
function navigateTo(url: string): void {
  window.location.assign(url);
}

/**
 * Connect form for one catalog entry: a display name, the connector's config
 * fields (driven by the catalog field hints), and — for token connectors — a
 * dev "paste a token" field. Submitting creates the instance via onSubmit.
 *
 * For an `oauthProvider` connector the user can leave the token blank and,
 * after the instance is created, the form shows a "Connect with <Provider>"
 * button that fetches the provider authorize URL (onStartOAuth) and navigates
 * the browser there to complete the real OAuth sign-in. The manual paste-a-
 * token field stays as a dev fallback.
 */
export function ConnectForm({
  type,
  onSubmit,
  onStartOAuth,
  onNavigate,
  onDone,
  onCancel,
}: ConnectFormProps) {
  const [displayName, setDisplayName] = useState(type.displayName);
  const [values, setValues] = useState<Record<string, string>>({});
  const [token, setToken] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  // Set to the created instance id once an oauthProvider connector is created
  // with no manual token, so we can offer the "Connect with <Provider>" button.
  const [createdId, setCreatedId] = useState<string | null>(null);

  const navigate = onNavigate ?? navigateTo;
  const isOAuth = type.oauthProvider !== undefined;

  function setField(key: string, value: string) {
    setValues((prev) => ({ ...prev, [key]: value }));
  }

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    if (submitting) {
      return;
    }
    setError("");
    setSubmitting(true);
    const trimmedToken = token.trim();
    onSubmit(displayName.trim(), buildConfig(type, values), trimmedToken)
      .then((id) => {
        // OAuth connector with no manual token: offer the provider sign-in
        // step instead of closing. Otherwise the connect is complete.
        if (isOAuth && trimmedToken === "") {
          setCreatedId(id);
          return;
        }
        onDone();
      })
      .catch((err: unknown) => {
        setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => setSubmitting(false));
  }

  function handleStartOAuth() {
    if (submitting || createdId === null || onStartOAuth === undefined) {
      return;
    }
    setError("");
    setSubmitting(true);
    onStartOAuth(createdId)
      .then((url) => {
        // Top-level navigation to the provider; the gateway callback returns
        // the browser to /connectors?oauth=connected. No further state here.
        navigate(url);
      })
      .catch((err: unknown) => {
        setError(err instanceof Error ? err.message : String(err));
        setSubmitting(false);
      });
  }

  const titleId = `connect-${type.id}-title`;

  // Second step: instance created for an OAuth connector — offer provider sign-in.
  if (createdId !== null && type.oauthProvider !== undefined) {
    const providerLabel = oauthProviderLabel(type.oauthProvider);
    return (
      <div className="connect-form" aria-labelledby={titleId}>
        <h3 id={titleId} className="connect-form-title">
          Connect {type.displayName}
        </h3>
        <p className="connector-field-hint">
          {type.displayName} is ready. Sign in with {providerLabel} to authorize
          access.
        </p>

        {error !== "" && (
          <p className="connect-form-error" role="alert">
            {error}
          </p>
        )}

        <div className="connect-form-actions">
          <button
            type="button"
            className="search-button"
            onClick={handleStartOAuth}
            disabled={submitting}
          >
            {submitting
              ? "Redirecting…"
              : `Connect with ${providerLabel}`}
          </button>
          <button
            type="button"
            className="secondary-button"
            onClick={onDone}
            disabled={submitting}
          >
            Later
          </button>
        </div>
      </div>
    );
  }

  return (
    <form
      className="connect-form"
      onSubmit={handleSubmit}
      aria-labelledby={titleId}
    >
      <h3 id={titleId} className="connect-form-title">
        Connect {type.displayName}
      </h3>

      <label className="connector-field">
        Display name
        <input
          type="text"
          value={displayName}
          onChange={(e) => setDisplayName(e.target.value)}
          disabled={submitting}
          required
        />
      </label>

      {type.fields.map((field) => (
        <label key={field.key} className="connector-field">
          {field.label}
          {field.required === true && (
            <span className="connector-field-required" aria-hidden="true">
              {" "}
              *
            </span>
          )}
          <input
            type="text"
            value={values[field.key] ?? ""}
            placeholder={field.placeholder}
            onChange={(e) => setField(field.key, e.target.value)}
            disabled={submitting}
            required={field.required === true}
          />
        </label>
      ))}

      {type.needsToken && (
        <label className="connector-field">
          Access token
          {isOAuth && (
            <span className="connector-field-optional" aria-hidden="true">
              {" "}
              (optional)
            </span>
          )}
          <input
            type="password"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            disabled={submitting}
            autoComplete="off"
            required={!isOAuth}
          />
          <span className="connector-field-hint">
            {isOAuth
              ? "Leave blank to sign in with the provider after creating. Dev: paste a token to skip OAuth."
              : "Dev: paste a token. Real OAuth sign-in is coming soon."}
          </span>
        </label>
      )}

      {error !== "" && (
        <p className="connect-form-error" role="alert">
          {error}
        </p>
      )}

      <div className="connect-form-actions">
        <button type="submit" className="search-button" disabled={submitting}>
          {submitting ? "Connecting…" : "Connect"}
        </button>
        <button
          type="button"
          className="secondary-button"
          onClick={onCancel}
          disabled={submitting}
        >
          Cancel
        </button>
      </div>
    </form>
  );
}
