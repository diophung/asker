import { useEffect, useMemo, useState } from "react";
import { apiUrl, getToken, initAuth, login, logout, username } from "./auth";
import { SearchClient } from "./api";
import { SearchPage } from "./SearchPage";

type AuthState = "initializing" | "anonymous" | "authenticated" | "error";

export function App() {
  const [auth, setAuth] = useState<AuthState>("initializing");

  useEffect(() => {
    let cancelled = false;
    initAuth()
      .then((ok) => {
        if (!cancelled) {
          setAuth(ok ? "authenticated" : "anonymous");
        }
      })
      .catch(() => {
        if (!cancelled) {
          setAuth("error");
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const client = useMemo(
    () => new SearchClient({ baseUrl: apiUrl, getToken }),
    [],
  );

  return (
    <div className="app">
      <header className="app-header">
        <span className="brand">Asker</span>
        {auth === "authenticated" && (
          <div className="header-user">
            <span className="header-username">{username()}</span>
            <button
              type="button"
              className="link-button"
              onClick={() => void logout()}
            >
              Sign out
            </button>
          </div>
        )}
      </header>

      {auth === "initializing" && (
        <div className="auth-screen">
          <p className="state-detail">Checking session…</p>
        </div>
      )}

      {auth === "anonymous" && (
        <div className="auth-screen">
          <h1>Search your everything.</h1>
          <p className="state-detail">
            Sign in to search across your connected accounts.
          </p>
          <button
            type="button"
            className="search-button"
            onClick={() => void login()}
          >
            Sign in
          </button>
        </div>
      )}

      {auth === "error" && (
        <div className="auth-screen" role="alert">
          <h1>Sign-in unavailable</h1>
          <p className="state-detail">
            Could not reach the identity provider. Is the dev stack running?
          </p>
          <button
            type="button"
            className="search-button"
            onClick={() => window.location.reload()}
          >
            Retry
          </button>
        </div>
      )}

      {auth === "authenticated" && <SearchPage client={client} />}
    </div>
  );
}
