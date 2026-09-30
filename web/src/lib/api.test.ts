import { afterEach, describe, expect, it, vi } from "vitest";
import { converge, deploy } from "./api";

function respond(status: number, body: unknown) {
  const fetchMock = vi.fn(
    async (_input: RequestInfo | URL, _init?: RequestInit) =>
      new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      }),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("converge", () => {
  it("POSTs to the service's converge route with no body and no credential", async () => {
    const fetchMock = respond(202, { journalId: 41, joined: false });

    await converge("kami");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("/api/v1/services/kami/converge");
    expect(init?.method).toBe("POST");
    expect(init?.body).toBeUndefined();
    expect(init?.headers).toBeUndefined();
  });

  it("escapes the service name into a single path segment", async () => {
    const fetchMock = respond(202, { journalId: 1, joined: false });

    await converge("a/b");

    expect(fetchMock.mock.calls[0]![0]).toBe("/api/v1/services/a%2Fb/converge");
  });

  it("returns the run to follow on a fresh 202", async () => {
    respond(202, { journalId: 41, joined: false });

    await expect(converge("kami")).resolves.toEqual({
      journalId: 41,
      joined: false,
    });
  });

  it("reports a joined pending run", async () => {
    respond(202, { journalId: 40, joined: true });

    await expect(converge("kami")).resolves.toEqual({
      journalId: 40,
      joined: true,
    });
  });

  it("treats a daemon that omits joined as a fresh run", async () => {
    respond(202, { journalId: 41 });

    await expect(converge("kami")).resolves.toEqual({
      journalId: 41,
      joined: false,
    });
  });

  it("throws the daemon's own reason on a refusal", async () => {
    respond(400, { error: "kami: unknown service" });

    await expect(converge("kami")).rejects.toThrow("kami: unknown service");
  });

  it("throws the status text when the error body is not JSON", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(
        async () =>
          new Response("<html>bad gateway</html>", {
            status: 502,
            statusText: "Bad Gateway",
          }),
      ),
    );

    await expect(converge("kami")).rejects.toThrow("Bad Gateway");
  });
});

describe("deploy", () => {
  it("still sends the digest as a JSON body", async () => {
    const fetchMock = respond(202, { journalId: 9, joined: false });

    await deploy("web", "sha256:abc");

    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe("/api/v1/services/web/deploy");
    expect(init?.body).toBe(JSON.stringify({ digest: "sha256:abc" }));
    expect(init?.headers).toEqual({ "Content-Type": "application/json" });
  });
});
