// Keycloak authentication for the Asker web UI.
//
// Flow: on load we run a silent check-sso (hidden iframe -> silent-check-sso.html)
// to pick up an existing Keycloak session without a redirect. If there is no
// session the app renders a sign-in screen; clicking it starts the standard
// Authorization Code flow with PKCE (S256) — required because asker-web is a
// public client. Tokens never touch our backend directly: the gateway verifies
// the JWT on every request.
import Keycloak from "keycloak-js";

export const keycloakUrl: string =
  import.meta.env.VITE_KEYCLOAK_URL ?? "http://localhost:8081";

export const apiUrl: string =
  import.meta.env.VITE_API_URL ?? "http://localhost:8080";

const keycloak = new Keycloak({
  url: keycloakUrl,
  realm: "asker",
  clientId: "asker-web",
});

let initPromise: Promise<boolean> | null = null;

/**
 * Initialize Keycloak exactly once (React StrictMode mounts effects twice in
 * dev; keycloak-js refuses double init). Resolves true when a session exists.
 */
export function initAuth(): Promise<boolean> {
  initPromise ??= keycloak.init({
    onLoad: "check-sso",
    silentCheckSsoRedirectUri: `${window.location.origin}/silent-check-sso.html`,
    pkceMethod: "S256",
  });
  return initPromise;
}

/** Redirect to the Keycloak login page and come back to the current URL. */
export function login(): Promise<void> {
  return keycloak.login({ redirectUri: window.location.href });
}

/** End the Keycloak session and return to the app root. */
export function logout(): Promise<void> {
  return keycloak.logout({ redirectUri: window.location.origin });
}

/**
 * Return a fresh access token, refreshing it first when it expires within
 * 30 seconds. Call this before every API request. Throws when the session is
 * gone (e.g. refresh token expired) — callers should treat that as signed-out.
 */
export async function getToken(): Promise<string> {
  try {
    await keycloak.updateToken(30);
  } catch {
    throw new Error("session expired — please sign in again");
  }
  if (!keycloak.token) {
    throw new Error("not authenticated");
  }
  return keycloak.token;
}

/** Display name of the signed-in user, if any. */
export function username(): string {
  const parsed = keycloak.tokenParsed as
    | { preferred_username?: string; sub?: string }
    | undefined;
  return parsed?.preferred_username ?? parsed?.sub ?? "";
}
