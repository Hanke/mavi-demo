import { useEffect, useState } from "react";
import { API_URL, fetchHealth, type HealthResponse } from "./api";

type State =
  | { kind: "loading" }
  | { kind: "ok"; health: HealthResponse }
  | { kind: "error"; message: string };

export function App() {
  const [state, setState] = useState<State>({ kind: "loading" });

  useEffect(() => {
    fetchHealth()
      .then((health) => setState({ kind: "ok", health }))
      .catch((err: unknown) =>
        setState({ kind: "error", message: err instanceof Error ? err.message : String(err) }),
      );
  }, []);

  return (
    <main>
      <h1>Mavi</h1>
      <p className="muted">
        API: <code>{API_URL}</code>
      </p>
      <HealthPanel state={state} />
    </main>
  );
}

function HealthPanel({ state }: { state: State }) {
  if (state.kind === "loading") return <p>Checking API health…</p>;
  if (state.kind === "error") return <p role="alert">API unreachable: {state.message}</p>;

  const { health } = state;
  return (
    <section aria-label="health">
      <h2>
        Status: <span data-status={health.status}>{health.status}</span>
      </h2>
      <ul>
        {Object.entries(health.checks).map(([name, value]) => (
          <li key={name}>
            <strong>{name}</strong>: {value}
          </li>
        ))}
      </ul>
    </section>
  );
}
