/**
 * A router small enough to read in one sitting. Two routes exist: the service
 * list and one service's detail page.
 */
export type Route =
  | { name: "services" }
  | { name: "service"; service: string };

export function parse(pathname: string): Route {
  const match = /^\/service\/([^/]+)\/?$/.exec(pathname);
  if (match?.[1]) {
    return { name: "service", service: decodeURIComponent(match[1]) };
  }
  return { name: "services" };
}

export function href(route: Route): string {
  return route.name === "service"
    ? `/service/${encodeURIComponent(route.service)}`
    : "/";
}

export function navigate(route: Route): void {
  history.pushState({}, "", href(route));
  window.dispatchEvent(new PopStateEvent("popstate"));
}
