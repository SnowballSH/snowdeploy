<script lang="ts">
  import { Callout, PageShell, ThemeToggle } from "foundationui/svelte";
  import {
    deploy,
    fetchHistory,
    fetchRevisions,
    fetchServices,
    rollback,
    subscribe,
  } from "./lib/api";
  import { parse, type Route } from "./lib/router";
  import Service from "./routes/Service.svelte";
  import Services from "./routes/Services.svelte";
  import {
    applyEvent,
    initialState,
    setError,
    setHistory,
    setRevisions,
    setServices,
  } from "./lib/state";

  let store = $state(initialState());
  let route = $state<Route>(parse(location.pathname));

  // Both loaders read `store` only AFTER their await. Reading it before would
  // be a synchronous read inside whichever $effect called them, making that
  // effect depend on `store`: every assignment then re-runs the effect, which
  // closes and reopens the EventSource in a tight loop — the UI flashes and no
  // deploy event ever survives long enough to arrive.
  async function refresh() {
    try {
      const services = await fetchServices();
      store = setServices(store, services);
    } catch (err) {
      store = setError(store, err instanceof Error ? err.message : String(err));
    }
  }

  async function loadHistory(service: string) {
    try {
      const entries = await fetchHistory(service);
      store = setHistory(store, service, entries);
      await loadRevisions(
        service,
        entries.map((entry) => entry.NewDigest),
      );
    } catch (err) {
      store = setError(store, err instanceof Error ? err.message : String(err));
    }
  }

  // Best-effort: a digest whose commit the daemon cannot name still renders,
  // just without the commit line, so a lookup failure is not a page error.
  async function loadRevisions(service: string, digests: string[]) {
    const wanted = [
      ...new Set(digests.filter((d) => d && !store.revisions[d])),
    ];
    if (wanted.length === 0) return;
    try {
      const found = await fetchRevisions(service, wanted);
      store = setRevisions(store, found);
    } catch {
      // Absence of commit metadata is a degraded label, never an error state.
    }
  }

  async function act(
    run: () => Promise<number>,
    service: string,
  ): Promise<void> {
    try {
      await run();
      store = setError(store, null);
    } catch (err) {
      store = setError(store, err instanceof Error ? err.message : String(err));
      return;
    }
    await refresh();
    if (route.name === "service") await loadHistory(service);
  }

  const onDeploy = (service: string, digest: string) =>
    act(() => deploy(service, digest), service);

  const onRollback = (service: string, digest: string) =>
    act(() => rollback(service, digest), service);

  $effect(() => {
    const onPop = () => {
      route = parse(location.pathname);
    };
    window.addEventListener("popstate", onPop);

    const onClick = (e: MouseEvent) => {
      const anchor = (e.target as HTMLElement | null)?.closest("a");
      const target = anchor?.getAttribute("href");
      if (!anchor || !target?.startsWith("/") || e.metaKey || e.ctrlKey) return;
      e.preventDefault();
      history.pushState({}, "", target);
      route = parse(location.pathname);
    };
    document.addEventListener("click", onClick);

    const unsubscribe = subscribe((ev) => {
      store = applyEvent(store, ev);
    });

    void refresh();
    const timer = setInterval(refresh, 30_000);

    return () => {
      window.removeEventListener("popstate", onPop);
      document.removeEventListener("click", onClick);
      unsubscribe();
      clearInterval(timer);
    };
  });

  // Reload a service's receipts whenever its page is opened.
  $effect(() => {
    if (route.name === "service") void loadHistory(route.service);
  });

  const current = $derived.by(() => {
    if (route.name !== "service") return undefined;
    const wanted = route.service;
    return store.services.find((s) => s.name === wanted);
  });
</script>

<PageShell width="site">
  <header class="flex items-center justify-between py-6">
    <div>
      <h1 class="text-2xl font-medium text-ink">snowdeploy</h1>
      <p class="text-sm text-ink-secondary">
        Deploy, roll back, and watch managed services.
      </p>
    </div>
    <ThemeToggle />
  </header>

  {#if store.error}
    <div class="mb-4">
      <Callout tone="warn">{store.error}</Callout>
    </div>
  {/if}

  {#if route.name === "service"}
    <Service
      name={route.service}
      service={current}
      progress={store.active[route.service]}
      history={store.history[route.service] ?? []}
      revisions={store.revisions}
      {onRollback}
    />
  {:else}
    <Services
      services={store.services}
      active={store.active}
      revisions={store.revisions}
      loaded={store.loaded}
      {onDeploy}
    />
  {/if}
</PageShell>
