<script lang="ts">
  import { Callout, PageShell, ThemeToggle } from "foundationui/svelte";
  import { deploy, fetchHistory, fetchServices, rollback, subscribe } from "./lib/api";
  import { parse, type Route } from "./lib/router";
  import Service from "./routes/Service.svelte";
  import Services from "./routes/Services.svelte";
  import {
    applyEvent,
    initialState,
    setError,
    setHistory,
    setServices,
  } from "./lib/state";

  let store = $state(initialState());
  let route = $state<Route>(parse(location.pathname));

  async function refresh() {
    try {
      store = setServices(store, await fetchServices());
    } catch (err) {
      store = setError(store, err instanceof Error ? err.message : String(err));
    }
  }

  async function loadHistory(service: string) {
    try {
      store = setHistory(store, service, await fetchHistory(service));
    } catch (err) {
      store = setError(store, err instanceof Error ? err.message : String(err));
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
      {onRollback}
    />
  {:else}
    <Services services={store.services} active={store.active} {onDeploy} />
  {/if}
</PageShell>
