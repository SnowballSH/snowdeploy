import { describe, expect, it } from "vitest";
import {
  applyEvent,
  initialState,
  isTerminal,
  lifecycleIndex,
  setHistory,
  setServices,
  shortDigest,
  updateAvailable,
  type DeployEvent,
  type HistoryEntry,
  type ServiceStatus,
} from "./state";

function event(partial: Partial<DeployEvent>): DeployEvent {
  return {
    service: "web",
    state: "pr-open",
    detail: "",
    journalId: 7,
    at: "2026-08-14T12:00:00Z",
    ...partial,
  };
}

function service(partial: Partial<ServiceStatus> = {}): ServiceStatus {
  return {
    name: "web",
    repository: "registry.example.com/acme/web",
    manifestDigest: "sha256:aaaaaaaaaaaaaaaaaaaa",
    runningDigest: "sha256:aaaaaaaaaaaaaaaaaaaa",
    latestAvailable: "sha256:aaaaaaaaaaaaaaaaaaaa",
    drifted: false,
    ...partial,
  };
}

describe("applyEvent", () => {
  it("tracks an in-flight deploy as it moves along the rail", () => {
    let s = initialState();
    s = applyEvent(s, event({ state: "pr-open", detail: "#12 opened" }));
    expect(s.active["web"]?.state).toBe("pr-open");

    s = applyEvent(s, event({ state: "probing", detail: "probing /healthz" }));
    expect(s.active["web"]?.state).toBe("probing");
    expect(s.active["web"]?.detail).toBe("probing /healthz");
    expect(s.history["web"]).toBeUndefined();
  });

  it("moves a healthy deploy to terminal and prepends it to history", () => {
    let s = initialState();
    s = applyEvent(s, event({ state: "pr-open", at: "2026-08-14T12:00:00Z" }));
    s = applyEvent(
      s,
      event({ state: "healthy", detail: "web is healthy", at: "2026-08-14T12:05:00Z" }),
    );

    expect(s.active["web"]).toBeUndefined();

    const entries = s.history["web"];
    expect(entries).toHaveLength(1);
    expect(entries?.[0]?.State).toBe("healthy");
    expect(entries?.[0]?.ID).toBe(7);
    expect(entries?.[0]?.Detail).toBe("web is healthy");
    expect(entries?.[0]?.StartedAt).toBe("2026-08-14T12:00:00Z");
    expect(entries?.[0]?.FinishedAt).toBe("2026-08-14T12:05:00Z");
  });

  it("prepends the newest receipt ahead of older ones", () => {
    let s = setHistory(initialState(), "web", [
      { ID: 1, State: "healthy" } as HistoryEntry,
    ]);
    s = applyEvent(s, event({ state: "healthy", journalId: 2 }));

    expect(s.history["web"]?.map((e) => e.ID)).toEqual([2, 1]);
  });

  it("does not duplicate a receipt already in history", () => {
    let s = setHistory(initialState(), "web", [
      { ID: 7, State: "healthy" } as HistoryEntry,
    ]);
    s = applyEvent(s, event({ state: "healthy", journalId: 7 }));

    expect(s.history["web"]).toHaveLength(1);
  });

  it("records a rollback as a terminal outcome too", () => {
    let s = applyEvent(initialState(), event({ state: "reconciling" }));
    s = applyEvent(s, event({ state: "rolled-back", detail: "probe failed" }));

    expect(s.active["web"]).toBeUndefined();
    expect(s.history["web"]?.[0]?.State).toBe("rolled-back");
  });

  it("keeps services separate", () => {
    let s = applyEvent(initialState(), event({ service: "web", state: "checks" }));
    s = applyEvent(s, event({ service: "api", state: "merged", journalId: 9 }));

    expect(s.active["web"]?.state).toBe("checks");
    expect(s.active["api"]?.state).toBe("merged");
  });

  it("does not mutate the previous state", () => {
    const before = applyEvent(initialState(), event({ state: "pr-open" }));
    const snapshot = JSON.stringify(before);
    applyEvent(before, event({ state: "healthy" }));

    expect(JSON.stringify(before)).toBe(snapshot);
  });
});

describe("setServices", () => {
  it("replaces the catalog without dropping in-flight progress", () => {
    let s = applyEvent(initialState(), event({ state: "merged" }));
    s = setServices(s, [service()]);

    expect(s.services).toHaveLength(1);
    expect(s.active["web"]?.state).toBe("merged");
  });
});

describe("helpers", () => {
  it("knows which states are terminal", () => {
    expect(isTerminal("healthy")).toBe(true);
    expect(isTerminal("rolled-back")).toBe(true);
    expect(isTerminal("failed")).toBe(true);
    expect(isTerminal("probing")).toBe(false);
  });

  it("orders the rail", () => {
    expect(lifecycleIndex("detected")).toBe(0);
    expect(lifecycleIndex("healthy")).toBe(6);
    expect(lifecycleIndex("rolled-back")).toBe(-1);
  });

  it("shortens digests and survives missing ones", () => {
    expect(shortDigest("sha256:0123456789abcdefff")).toBe("0123456789ab");
    expect(shortDigest(undefined)).toBe("—");
  });

  it("spots an available update only when the registry is ahead", () => {
    expect(updateAvailable(service())).toBe(false);
    expect(updateAvailable(service({ latestAvailable: "sha256:newer" }))).toBe(true);
    expect(updateAvailable(service({ latestAvailable: "" }))).toBe(false);
  });
});
