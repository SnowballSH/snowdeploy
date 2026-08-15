/**
 * The reducer behind the UI. It is deliberately free of any framework: the
 * components hold one `$state` value and hand every server event through
 * `applyEvent`, so the progress rail and the history list can never disagree.
 */

/** Every state a deploy moves through, in lifecycle order. */
export const LIFECYCLE = [
  "detected",
  "pr-open",
  "checks",
  "merged",
  "reconciling",
  "probing",
  "healthy",
] as const;

/** The three states that end a deploy. Only `healthy` is a success. */
export const TERMINAL = ["healthy", "rolled-back", "failed"] as const;

export type Terminal = (typeof TERMINAL)[number];

export interface DeployEvent {
  service: string;
  action?: string;
  state: string;
  detail: string;
  journalId: number;
  at: string;
  prNumber?: number;
  prUrl?: string;
  mergeUrl?: string;
}

/** The commit an image digest was built from, when the registry knows it. */
export interface Revision {
  sha: string;
  url: string;
  subject: string;
}

export interface HistoryEntry {
  ID: number;
  Service: string;
  Action: string;
  Actor: string;
  OldDigest: string;
  NewDigest: string;
  PRNumber: number;
  MergeSHA: string;
  State: string;
  Detail: string;
  StartedAt: string;
  FinishedAt: string;
}

export interface ServiceStatus {
  name: string;
  repository: string;
  manifestDigest: string;
  runningDigest: string;
  latestAvailable: string;
  drifted: boolean;
  lastDeploy?: HistoryEntry;
  revisions?: Record<string, Revision>;
  /** The configuration repository's web address, for deriving PR and
   * commit links on rows that came from the journal rather than an event. */
  repoWebUrl?: string;
}

/** An in-flight deploy, as the progress rail renders it. */
export interface Progress {
  journalId: number;
  service: string;
  state: string;
  detail: string;
  at: string;
  prNumber?: number;
  prUrl?: string;
  mergeUrl?: string;
}

export interface StoreState {
  services: ServiceStatus[];
  /** At most one in-flight deploy per service. */
  active: Record<string, Progress>;
  /** Newest first, per service. */
  history: Record<string, HistoryEntry[]>;
  /** digest → commit, merged from every source that has answered. */
  revisions: Record<string, Revision>;
  /** The configuration repository's web address, once the catalog names it. */
  repoWebUrl: string;
  /** False until the first services fetch settles, so an empty catalog is
   * only claimed once it is actually known to be empty. */
  loaded: boolean;
  error: string | null;
}

export function initialState(): StoreState {
  return {
    services: [],
    active: {},
    history: {},
    revisions: {},
    repoWebUrl: "",
    loaded: false,
    error: null,
  };
}

/** prLink derives the pull-request URL for a journal-sourced row. */
export function prLink(
  repoWebUrl: string,
  prNumber: number | undefined,
): string | undefined {
  if (!repoWebUrl || !prNumber) return undefined;
  return `${repoWebUrl}/pull/${prNumber}`;
}

export function isTerminal(state: string): state is Terminal {
  return (TERMINAL as readonly string[]).includes(state);
}

/** How far along the rail a state sits; -1 for the two failure endings. */
export function lifecycleIndex(state: string): number {
  return (LIFECYCLE as readonly string[]).indexOf(state);
}

/**
 * applyEvent folds one server event into the store. A terminal event ends the
 * active deploy and prepends a receipt to that service's history, so the page
 * shows the outcome without waiting for a refetch.
 */
export function applyEvent(prev: StoreState, ev: DeployEvent): StoreState {
  const active = { ...prev.active };
  const history = { ...prev.history };

  if (isTerminal(ev.state)) {
    const started = prev.active[ev.service];
    delete active[ev.service];

    // A receipt for this run may already be in history from an earlier
    // refetch, frozen at whatever state it was fetched in. The terminal
    // event must UPDATE that row, not be dropped as a duplicate — a
    // finished deploy that keeps reading "detected" is a lie.
    const existing = history[ev.service] ?? [];
    if (existing.some((entry) => entry.ID === ev.journalId)) {
      history[ev.service] = existing.map((entry) =>
        entry.ID === ev.journalId
          ? { ...entry, State: ev.state, Detail: ev.detail, FinishedAt: ev.at }
          : entry,
      );
    } else {
      history[ev.service] = [
        {
          ID: ev.journalId,
          Service: ev.service,
          Action: ev.action || "deploy",
          Actor: "",
          OldDigest: "",
          NewDigest: "",
          PRNumber: ev.prNumber ?? 0,
          MergeSHA: "",
          State: ev.state,
          Detail: ev.detail,
          StartedAt: started?.at ?? ev.at,
          FinishedAt: ev.at,
        },
        ...existing,
      ];
    }
  } else {
    const before = prev.active[ev.service];
    active[ev.service] = {
      journalId: ev.journalId,
      service: ev.service,
      state: ev.state,
      detail: ev.detail,
      at: before?.at ?? ev.at,
      // Links are carried forward: the daemon sends them from the moment each
      // is known, but a dropped frame must not un-link the rail.
      prNumber: ev.prNumber ?? before?.prNumber,
      prUrl: ev.prUrl ?? before?.prUrl,
      mergeUrl: ev.mergeUrl ?? before?.mergeUrl,
    };
  }

  return { ...prev, active, history };
}

/**
 * setServices replaces the catalog and reconciles in-flight progress against
 * it. Events are the fast path but not a reliable one — the stream carries no
 * replay, so a dropped terminal frame would otherwise strand a rail forever,
 * and a page loaded mid-deploy would never learn one is running. The
 * journal's word (lastDeploy) is authoritative in both directions.
 */
export function setServices(
  prev: StoreState,
  services: ServiceStatus[],
): StoreState {
  const revisions = { ...prev.revisions };
  const active = { ...prev.active };
  const known = new Set(services.map((s) => s.name));
  const repoWebUrl =
    services.find((s) => s.repoWebUrl)?.repoWebUrl ?? prev.repoWebUrl;
  for (const name of Object.keys(active)) {
    if (!known.has(name)) delete active[name];
  }
  for (const service of services) {
    Object.assign(revisions, service.revisions);
    const last = service.lastDeploy;
    const current = active[service.name];
    if (!last) continue;
    if (current && isTerminal(last.State) && last.ID >= current.journalId) {
      delete active[service.name];
    } else if (!current && !isTerminal(last.State)) {
      active[service.name] = {
        journalId: last.ID,
        service: service.name,
        state: last.State,
        detail: last.Detail,
        at: last.StartedAt,
        prNumber: last.PRNumber || undefined,
        prUrl: prLink(repoWebUrl, last.PRNumber),
      };
    }
  }
  return {
    ...prev,
    services,
    revisions,
    active,
    repoWebUrl,
    loaded: true,
    error: null,
  };
}

/** setRevisions merges one lookup response into the digest → commit map. */
export function setRevisions(
  prev: StoreState,
  found: Record<string, Revision>,
): StoreState {
  return { ...prev, revisions: { ...prev.revisions, ...found } };
}

export function setHistory(
  prev: StoreState,
  service: string,
  entries: HistoryEntry[],
): StoreState {
  return { ...prev, history: { ...prev.history, [service]: entries } };
}

export function setError(prev: StoreState, error: string | null): StoreState {
  // An error settles the initial load too: the page must stop claiming it is
  // loading, and show what went wrong instead.
  return { ...prev, loaded: prev.loaded || error !== null, error };
}

/** shortDigest is what the tables show; the full value stays in the title. */
export function shortDigest(digest: string | undefined): string {
  if (!digest) return "—";
  return digest.replace(/^sha256:/, "").slice(0, 12);
}

/** updateAvailable is true when the registry is ahead of the merged manifest. */
export function updateAvailable(service: ServiceStatus): boolean {
  return (
    service.latestAvailable !== "" &&
    service.latestAvailable !== service.manifestDigest
  );
}
