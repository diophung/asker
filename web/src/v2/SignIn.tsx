import { type FormEvent, useState } from "react";
import { Lock } from "lucide-react";
import { DEV_USER, DEV_PASS, signIn } from "./auth";

/** Google-homage wordmark (matches SearchApp's logo). */
function Wordmark() {
  const letters: [string, string][] = [
    ["A", "#4285f4"],
    ["s", "#ea4335"],
    ["k", "#fbbc04"],
    ["e", "#4285f4"],
    ["r", "#34a853"],
  ];
  return (
    <span className="text-[44px] font-semibold leading-none tracking-tight select-none">
      {letters.map(([ch, color], i) => (
        <span key={i} style={{ color }} aria-hidden="true">
          {ch}
        </span>
      ))}
    </span>
  );
}

/**
 * Dev sign-in gate. Shown only in backend mode (the real gateway needs a token).
 * Prefilled with the dev-stack credentials; signing in runs the password grant
 * (see auth.ts) and reveals the search UI.
 */
export function SignIn({ onSignedIn }: { onSignedIn: () => void }) {
  const [user, setUser] = useState(DEV_USER);
  const [pass, setPass] = useState(DEV_PASS);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  function handleSubmit(e: FormEvent) {
    e.preventDefault();
    if (busy) {
      return;
    }
    setError("");
    setBusy(true);
    signIn(user.trim(), pass)
      .then(onSignedIn)
      .catch((err: unknown) => {
        setError(err instanceof Error ? err.message : "Sign-in failed.");
        setBusy(false);
      });
  }

  return (
    <main className="flex min-h-screen items-center justify-center px-4">
      <div className="w-full max-w-[400px] rounded-3xl border border-gline px-8 py-10 text-center">
        <Wordmark />
        <h1 className="mt-5 text-[22px] font-normal text-gink">Sign in</h1>
        <p className="mt-1 text-[14px] text-gmuted">to search your own data</p>

        <form onSubmit={handleSubmit} className="mt-7 space-y-3 text-left">
          <Field
            label="Username"
            type="text"
            value={user}
            onChange={setUser}
            autoFocus
          />
          <Field
            label="Password"
            type="password"
            value={pass}
            onChange={setPass}
          />

          {error !== "" && (
            <p className="text-[13px] text-[#d93025]" role="alert">
              {error}
            </p>
          )}

          <button
            type="submit"
            disabled={busy || user.trim() === ""}
            className="mt-2 w-full rounded-full bg-gblue py-2.5 text-[15px] font-medium text-white hover:brightness-95 disabled:opacity-50"
          >
            {busy ? "Signing in…" : "Sign in"}
          </button>
        </form>

        <p className="mt-6 flex items-center justify-center gap-1.5 text-[12px] text-gmuted">
          <Lock className="size-3" />
          Dev sign-in to your local Asker stack · prefilled with the dev account
        </p>
      </div>
    </main>
  );
}

function Field({
  label,
  type,
  value,
  onChange,
  autoFocus,
}: {
  label: string;
  type: string;
  value: string;
  onChange: (v: string) => void;
  autoFocus?: boolean;
}) {
  return (
    <label className="block">
      <span className="mb-1 block text-[12px] font-medium text-gmuted">
        {label}
      </span>
      <input
        type={type}
        value={value}
        autoFocus={autoFocus}
        autoComplete="off"
        onChange={(e) => onChange(e.target.value)}
        className="w-full rounded-lg border border-gline px-3 py-2 text-[15px] text-gink outline-none focus:border-gblue"
      />
    </label>
  );
}
