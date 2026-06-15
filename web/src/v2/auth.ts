// DEV-ONLY auth for the v2 UI. The v2 dev port is not a registered Keycloak
// redirect URI, so the production OIDC redirect flow (src/auth.ts) can't run
// here. Instead we sign in with the resource-owner password grant through the
// Vite dev proxy (/kc -> Keycloak). NEVER ship this — in production the seam
// swaps back to the real OIDC token.
//
// SESSION PERSISTENCE (DEV-ONLY relaxation): the source tabs do a full page
// load per tab, which would drop an in-memory session and sign the user out on
// every tab switch. So the dev session is mirrored into sessionStorage: it
// survives reloads/navigations within the tab and is cleared when the tab
// closes. This intentionally relaxes the v2 spec's "no localStorage/
// sessionStorage" rule (written for the no-backend mock); the real OIDC seam
// holds tokens differently. The key is namespaced and removed on sign-out.

const env = import.meta.env;
const TOKEN_PATH = "/kc/realms/asker/protocol/openid-connect/token";
const CLIENT_ID = "asker-web";
const STORAGE_KEY = "asker.dev.session";

/** Prefill for the dev sign-in form. */
export const DEV_USER = env.VITE_DEV_USER ?? "alice";
export const DEV_PASS = env.VITE_DEV_PASS ?? "password123";

interface Session {
  token: string;
  refresh: string;
  expiresAt: number;
  user: string;
}

function loadStoredSession(): Session | null {
  try {
    const raw = sessionStorage.getItem(STORAGE_KEY);
    if (!raw) {
      return null;
    }
    const s = JSON.parse(raw) as Partial<Session>;
    if (typeof s.token === "string" && typeof s.refresh === "string" && typeof s.expiresAt === "number") {
      return { token: s.token, refresh: s.refresh, expiresAt: s.expiresAt, user: s.user ?? "you" };
    }
  } catch {
    // Unparseable / unavailable storage: fall back to a fresh (signed-out) state.
  }
  return null;
}

function persistSession(): void {
  try {
    if (session) {
      sessionStorage.setItem(STORAGE_KEY, JSON.stringify(session));
    } else {
      sessionStorage.removeItem(STORAGE_KEY);
    }
  } catch {
    // sessionStorage unavailable (private mode etc.): degrade to in-memory only.
  }
}

let session: Session | null = loadStoredSession();
const subscribers = new Set<() => void>();

function notify(): void {
  for (const fn of subscribers) {
    fn();
  }
}

/** Subscribe to sign-in/sign-out; returns an unsubscribe. */
export function subscribe(fn: () => void): () => void {
  subscribers.add(fn);
  return () => subscribers.delete(fn);
}

export function isSignedIn(): boolean {
  return session !== null;
}

export function currentUser(): string {
  return session?.user ?? "";
}

function userOf(jwt: string): string {
  try {
    const payload = JSON.parse(
      atob(jwt.split(".")[1].replace(/-/g, "+").replace(/_/g, "/")),
    ) as { email?: string; preferred_username?: string; sub?: string };
    return payload.email ?? payload.preferred_username ?? payload.sub ?? "you";
  } catch {
    return "you";
  }
}

async function grant(body: URLSearchParams): Promise<void> {
  const res = await fetch(TOKEN_PATH, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body,
  });
  if (!res.ok) {
    throw new Error(
      res.status === 401
        ? "Wrong username or password."
        : `Sign-in failed (HTTP ${res.status}). Is the dev stack running?`,
    );
  }
  const json = (await res.json()) as {
    access_token: string;
    refresh_token: string;
    expires_in: number;
  };
  session = {
    token: json.access_token,
    refresh: json.refresh_token,
    expiresAt: Date.now() + json.expires_in * 1000,
    user: userOf(json.access_token),
  };
  persistSession();
}

export async function signIn(username: string, password: string): Promise<void> {
  await grant(
    new URLSearchParams({
      grant_type: "password",
      client_id: CLIENT_ID,
      username,
      password,
    }),
  );
  notify();
}

export function signOut(): void {
  session = null;
  persistSession();
  notify();
}

/** Fresh access token, refreshing within 30s of expiry. Throws when signed out. */
export async function getToken(): Promise<string> {
  if (!session) {
    throw new Error("not signed in");
  }
  if (Date.now() > session.expiresAt - 30_000) {
    try {
      await grant(
        new URLSearchParams({
          grant_type: "refresh_token",
          client_id: CLIENT_ID,
          refresh_token: session.refresh,
        }),
      );
    } catch {
      session = null;
      persistSession();
      notify();
      throw new Error("session expired — please sign in again");
    }
  }
  return session.token;
}
