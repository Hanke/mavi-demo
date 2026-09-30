export const API_URL: string = import.meta.env.VITE_API_URL ?? "http://localhost:8080";

export type HealthResponse = {
  status: "ok" | "degraded";
  checks: Record<string, string>;
};

export async function fetchHealth(fetchImpl: typeof fetch = fetch): Promise<HealthResponse> {
  const res = await fetchImpl(`${API_URL}/health`);
  // The API returns 503 with a body when degraded; surface that rather than throwing.
  return (await res.json()) as HealthResponse;
}
