import { type FormEvent, useState } from "react";
import { buildConfig, type ConnectorType } from "./catalog";

export interface ConnectFormProps {
  type: ConnectorType;
  /** Resolves once create + token are persisted; rejects with an Error. */
  onSubmit: (
    displayName: string,
    config: Record<string, unknown>,
    token: string,
  ) => Promise<void>;
  onCancel: () => void;
}

/**
 * Connect form for one catalog entry: a display name, the connector's config
 * fields (driven by the catalog field hints), and — for token connectors — a
 * dev "paste a token" field. Submitting calls onSubmit, which the page wires to
 * createConnector then putConnectorToken.
 */
export function ConnectForm({ type, onSubmit, onCancel }: ConnectFormProps) {
  const [displayName, setDisplayName] = useState(type.displayName);
  const [values, setValues] = useState<Record<string, string>>({});
  const [token, setToken] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");

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
    onSubmit(displayName.trim(), buildConfig(type, values), token.trim())
      .catch((err: unknown) => {
        setError(err instanceof Error ? err.message : String(err));
      })
      .finally(() => setSubmitting(false));
  }

  const titleId = `connect-${type.id}-title`;

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
          <input
            type="password"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            disabled={submitting}
            autoComplete="off"
            required
          />
          <span className="connector-field-hint">
            Dev: paste a token. Real OAuth sign-in is coming soon.
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
