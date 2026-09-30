import { render, screen } from "@testing-library/react";
import { App } from "./App";
import type { HealthResponse } from "./api";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("App", () => {
  it("renders health checks from the API", async () => {
    const health: HealthResponse = { status: "ok", checks: { postgres: "ok", ai: "ok" } };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(health)));

    render(<App />);

    expect(await screen.findByText("ok", { selector: "[data-status]" })).toBeInTheDocument();
    expect(screen.getByText("postgres")).toBeInTheDocument();
    expect(screen.getByText("ai")).toBeInTheDocument();
  });

  it("shows a degraded status from a 503", async () => {
    const health: HealthResponse = { status: "degraded", checks: { postgres: "ok", ai: "error: boom" } };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(health, 503)));

    render(<App />);

    expect(await screen.findByText("degraded", { selector: "[data-status]" })).toBeInTheDocument();
    expect(screen.getByText("ai").closest("li")).toHaveTextContent("error: boom");
  });

  it("treats a non-JSON gateway error as unreachable rather than rendering it", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response("<html>502 Bad Gateway</html>", { status: 502, headers: { "Content-Type": "text/html" } }),
      ),
    );

    render(<App />);

    expect(await screen.findByRole("alert")).toHaveTextContent("HTTP 502");
  });

  it("shows an error when the API is unreachable", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("connection refused")));

    render(<App />);

    expect(await screen.findByRole("alert")).toHaveTextContent("connection refused");
  });
});
