import {
  createContext,
  useContext,
  useEffect,
  useState,
  type AnchorHTMLAttributes,
  type ReactNode,
} from "react";

type Location = { pathname: string; search: URLSearchParams };

const RouterCtx = createContext<Location>({
  pathname: "/",
  search: new URLSearchParams(),
});

function current(): Location {
  return {
    pathname: window.location.pathname,
    search: new URLSearchParams(window.location.search),
  };
}

export function navigate(to: string, replace = false) {
  if (replace) window.history.replaceState(null, "", to);
  else window.history.pushState(null, "", to);
  window.dispatchEvent(new PopStateEvent("popstate"));
}

export function RouterProvider({ children }: { children: ReactNode }) {
  const [loc, setLoc] = useState(current);
  useEffect(() => {
    const on = () => setLoc(current());
    window.addEventListener("popstate", on);
    return () => window.removeEventListener("popstate", on);
  }, []);
  return <RouterCtx.Provider value={loc}>{children}</RouterCtx.Provider>;
}

export function useLocation() {
  return useContext(RouterCtx);
}

/** Matches "/servers/:id" style patterns; returns params or null. */
export function match(
  pattern: string,
  pathname: string,
): Record<string, string> | null {
  const p = pattern.split("/").filter(Boolean);
  const s = pathname.split("/").filter(Boolean);
  if (p.length !== s.length) return null;
  const params: Record<string, string> = {};
  for (let i = 0; i < p.length; i++) {
    if (p[i].startsWith(":")) params[p[i].slice(1)] = decodeURIComponent(s[i]);
    else if (p[i] !== s[i]) return null;
  }
  return params;
}

export function Link({
  to,
  children,
  ...rest
}: { to: string } & AnchorHTMLAttributes<HTMLAnchorElement>) {
  return (
    <a
      href={to}
      onClick={(e) => {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
        e.preventDefault();
        navigate(to);
      }}
      {...rest}
    >
      {children}
    </a>
  );
}
