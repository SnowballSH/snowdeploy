/**
 * The browser half of the API contract. The page holds no credential: the
 * session rides the fronting proxy's forward-auth, which is why every call is
 * a same-origin fetch with no Authorization header of its own.
 */
import type {
  Accepted,
  DeployEvent,
  HistoryEntry,
  Revision,
  ServiceStatus,
} from "./state";

async function json<T>(response: Response): Promise<T> {
  if (!response.ok) {
    let detail = response.statusText;
    try {
      const body = (await response.json()) as { error?: string };
      if (body.error) detail = body.error;
    } catch {
      // A non-JSON error body is still an error; the status carries it.
    }
    throw new Error(detail);
  }
  return (await response.json()) as T;
}

export async function fetchServices(): Promise<ServiceStatus[]> {
  return json<ServiceStatus[]>(await fetch("/api/v1/services"));
}

export async function fetchHistory(
  service: string,
  n = 20,
): Promise<HistoryEntry[]> {
  const path = `/api/v1/services/${encodeURIComponent(service)}/history?n=${n}`;
  return json<HistoryEntry[]>(await fetch(path));
}

export async function fetchRevisions(
  service: string,
  digests: string[],
): Promise<Record<string, Revision>> {
  if (digests.length === 0) return {};
  const path =
    `/api/v1/services/${encodeURIComponent(service)}/revisions?digests=` +
    encodeURIComponent(digests.join(","));
  return json<Record<string, Revision>>(await fetch(path));
}

async function post(
  service: string,
  action: "deploy" | "rollback" | "converge",
  body?: unknown,
): Promise<Accepted> {
  const init: RequestInit =
    body === undefined
      ? { method: "POST" }
      : {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        };
  const response = await fetch(
    `/api/v1/services/${encodeURIComponent(service)}/${action}`,
    init,
  );
  const accepted = await json<Partial<Accepted>>(response);
  return { journalId: accepted.journalId ?? 0, joined: accepted.joined === true };
}

export function deploy(service: string, digest: string): Promise<Accepted> {
  return post(service, "deploy", { digest });
}

export function rollback(service: string, digest: string): Promise<Accepted> {
  return post(service, "rollback", { digest });
}

/**
 * converge sends no body: the daemon applies the manifest main holds at the
 * front of the queue, so there is no digest for the page to choose.
 */
export function converge(service: string): Promise<Accepted> {
  return post(service, "converge");
}

/**
 * subscribe streams state transitions. The browser's EventSource reconnects on
 * its own, so a daemon restart heals the page without a reload.
 */
export function subscribe(onEvent: (ev: DeployEvent) => void): () => void {
  const source = new EventSource("/api/v1/events");
  const handler = (message: MessageEvent<string>) => {
    try {
      onEvent(JSON.parse(message.data) as DeployEvent);
    } catch {
      // A frame we cannot parse is not worth breaking the stream over.
    }
  };
  source.addEventListener("state", handler);
  return () => {
    source.removeEventListener("state", handler);
    source.close();
  };
}
