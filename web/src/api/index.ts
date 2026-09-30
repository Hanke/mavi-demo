// The web app's only door to the Go API. Every path, body and response is
// typed from src/api/schema.d.ts, which openapi-typescript generates from
// api/openapi.yaml (`npm run generate`, or `make generate` at the repo root).
// Nothing in here re-declares a shape the API already defines.
import createClient, { type Middleware } from "openapi-fetch";
import type { paths } from "./schema";
import type { HealthResponse, Persona } from "./types";

export * from "./types";

export const API_URL: string = import.meta.env.VITE_API_URL ?? "http://localhost:8080";

export type ClientOptions = {
  /** Sent as `X-Role`; omit for `/health` only. */
  role?: Persona;
  /** Sent as `X-Actor`: the candidate id for talent, an email for ops. */
  actor?: string;
  /** Injected in tests. When omitted, the global fetch is looked up on every call, not captured here. */
  fetch?: typeof fetch;
  baseUrl?: string;
};

/** A typed client for the API: `client.GET("/roles", { params: { query: { status: "open" } } })`. */
export function createApiClient(options: ClientOptions = {}) {
  const client = createClient<paths>({
    baseUrl: options.baseUrl ?? API_URL,
    fetch: (input) => (options.fetch ?? globalThis.fetch)(input),
  });
  if (options.role !== undefined || options.actor !== undefined) {
    const persona: Middleware = {
      onRequest({ request }) {
        if (options.role !== undefined) request.headers.set("X-Role", options.role);
        if (options.actor !== undefined) request.headers.set("X-Actor", options.actor);
        return request;
      },
    };
    client.use(persona);
  }
  return client;
}

export type ApiClient = ReturnType<typeof createApiClient>;

/**
 * The API's health. A 503 carries the same body as a 200 (with the failing
 * check named), so both are returned rather than thrown. Anything else (a
 * transport failure, a proxy's HTML error page, an unexpected status) rejects,
 * so the caller never renders a body that is not a HealthResponse.
 */
export async function fetchHealth(fetchImpl?: typeof fetch): Promise<HealthResponse> {
  const { data, error, response } = await createApiClient({ fetch: fetchImpl }).GET("/health");
  const body: unknown = data ?? error;
  if ((response.status === 200 || response.status === 503) && isHealthResponse(body)) {
    return body;
  }
  throw new Error(`unexpected /health response: HTTP ${response.status}`);
}

function isHealthResponse(body: unknown): body is HealthResponse {
  if (typeof body !== "object" || body === null) return false;
  const b = body as Record<string, unknown>;
  return (b.status === "ok" || b.status === "degraded") && typeof b.checks === "object" && b.checks !== null;
}
