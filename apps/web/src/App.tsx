import { useEffect, useState } from "react";
import { api } from "./api/client";

export default function App() {
  const [version, setVersion] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api.GET("/v1/version").then(({ data, error }) => {
      if (error) setError("control-plane unreachable");
      else setVersion(data.version);
    });
  }, []);

  return (
    <main className="flex min-h-screen items-center justify-center bg-slate-950 text-slate-100">
      <div className="rounded-lg border border-slate-800 bg-slate-900 p-8 text-center">
        <h1 className="text-2xl font-bold">p2pgames</h1>
        <p className="mt-2 text-slate-400">Peer-hosted game servers.</p>
        <p className="mt-4 font-mono text-sm">
          {version
            ? `control-plane ${version}`
            : (error ?? "connecting to control-plane…")}
        </p>
      </div>
    </main>
  );
}
