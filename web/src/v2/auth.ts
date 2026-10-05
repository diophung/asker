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
let generation = 0;
let refreshing: { owner: Session; promise: Promise<void> } | null = null;
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

async function grant(body: URLSearchParams): Promise<Session> {
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
  return {
    token: json.access_token,
    refresh: json.refresh_token,
    expiresAt: Date.now() + json.expires_in * 1000,
    user: userOf(json.access_token),
  };
}

export async function signIn(username: string, password: string): Promise<void> {
  const attempt = ++generation;
  const next = await grant(
    new URLSearchParams({
      grant_type: "password",
      client_id: CLIENT_ID,
      username,
      password,
    }),
  );
  if (attempt !== generation) throw new Error("Sign-in was canceled.");
  session = next;
  persistSession();
  notify();
}

export function signOut(): void {
  ++generation;
  session = null;
  persistSession();
  notify();
}

/** A rejected old request cannot invalidate a newer token/account. */
export function invalidateToken(token: string): void {
  if (session?.token === token) signOut();
}

/** Fresh access token, refreshing within 30s of expiry. Throws when signed out. */
export async function getToken(): Promise<string> {
  if (!session) {
    throw new Error("not signed in");
  }
  if (Date.now() > session.expiresAt - 30_000) {
    const owner = session;
    if (!refreshing || refreshing.owner !== owner) {
      const promise = grant(
        new URLSearchParams({
          grant_type: "refresh_token",
          client_id: CLIENT_ID,
          refresh_token: owner.refresh,
        }),
      ).then((next) => {
        if (session !== owner) throw new Error("Session changed.");
        session = next;
        persistSession();
      }).catch(() => {
        // A late failed refresh must never sign out a newer account.
        if (session === owner) {
          session = null;
          persistSession();
          notify();
        }
        throw new Error("Session expired — please sign in again.");
      });
      refreshing = { owner, promise };
      void promise.finally(() => {
        if (refreshing?.promise === promise) refreshing = null;
      }).catch(() => {});
    }
    await refreshing.promise;
  }
  if (!session) throw new Error("not signed in");
  return session.token;
}
