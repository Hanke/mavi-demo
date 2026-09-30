import { render, screen } from "@testing-library/react";
import { App } from "./App";

describe("App", () => {
  it("renders health checks from the API", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        json: async () => ({ status: "ok", checks: { postgres: "ok", ai: "ok" } }),
      }),
    );

    render(<App />);

    expect(await screen.findByText("ok", { selector: "[data-status]" })).toBeInTheDocument();
    expect(screen.getByText("postgres")).toBeInTheDocument();
    expect(screen.getByText("ai")).toBeInTheDocument();
  });

  it("shows an error when the API is unreachable", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("connection refused")));

    render(<App />);

    expect(await screen.findByRole("alert")).toHaveTextContent("connection refused");
  });
});
