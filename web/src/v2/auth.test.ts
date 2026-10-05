import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const jwt = (user: string) => `e30.${btoa(JSON.stringify({ preferred_username: user }))}.signature`;
const response = (user: string, expires = 1) => new Response(JSON.stringify({
  access_token: jwt(user), refresh_token: `${user}-refresh`, expires_in: expires,
}), { status: 200 });

beforeEach(() => { sessionStorage.clear(); vi.resetModules(); });
afterEach(() => vi.unstubAllGlobals());

describe("session refresh ownership", () => {
  it("invalidates only the token rejected by the gateway", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(response("alice", 300))
      .mockResolvedValueOnce(response("bob", 300)));
    const auth = await import("./auth");
    await auth.signIn("alice", "test");
    const old = await auth.getToken();
    await auth.signIn("bob", "test");
    auth.invalidateToken(old);
    expect(auth.currentUser()).toBe("bob");
    auth.invalidateToken(await auth.getToken());
    expect(auth.isSignedIn()).toBe(false);
  });
  it("shares one refresh across simultaneous search and source requests", async () => {
    let finish!: (response: Response) => void;
    const fetch = vi.fn().mockResolvedValueOnce(response("alice"))
      .mockImplementationOnce(() => new Promise<Response>((resolve) => { finish = resolve; }));
    vi.stubGlobal("fetch", fetch);
    const auth = await import("./auth");
    await auth.signIn("alice", "test");
    const tokens = [auth.getToken(), auth.getToken(), auth.getToken()];
    expect(fetch).toHaveBeenCalledTimes(2);
    finish(response("alice", 300));
    expect(await Promise.all(tokens)).toEqual([jwt("alice"), jwt("alice"), jwt("alice")]);
  });

  it("clears an expired session and notifies the UI once", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(response("alice"))
      .mockResolvedValueOnce(new Response("expired", { status: 400 })));
    const auth = await import("./auth");
    await auth.signIn("alice", "test");
    const subscriber = vi.fn();
    auth.subscribe(subscriber);
    const outcomes = await Promise.allSettled([auth.getToken(), auth.getToken()]);
    expect(outcomes.every((outcome) => outcome.status === "rejected")).toBe(true);
    expect(auth.isSignedIn()).toBe(false);
    expect(sessionStorage.getItem("asker.dev.session")).toBeNull();
    expect(subscriber).toHaveBeenCalledTimes(1);
  });

  it("cannot restore an account after sign-out while refresh is pending", async () => {
    let finish!: (response: Response) => void;
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(response("alice"))
      .mockImplementationOnce(() => new Promise<Response>((resolve) => { finish = resolve; })));
    const auth = await import("./auth");
    await auth.signIn("alice", "test");
    const token = auth.getToken();
    auth.signOut();
    finish(response("alice", 300));
    await expect(token).rejects.toThrow();
    expect(auth.isSignedIn()).toBe(false);
  });

  it("a late failed refresh cannot sign out a newly signed-in account", async () => {
    let finish!: (response: Response) => void;
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(response("alice"))
      .mockImplementationOnce(() => new Promise<Response>((resolve) => { finish = resolve; }))
      .mockResolvedValueOnce(response("bob", 300)));
    const auth = await import("./auth");
    await auth.signIn("alice", "test");
    const old = auth.getToken();
    await auth.signIn("bob", "test");
    finish(new Response("expired", { status: 400 }));
    await expect(old).rejects.toThrow();
    expect(auth.currentUser()).toBe("bob");
    expect(await auth.getToken()).toBe(jwt("bob"));
  });
});
