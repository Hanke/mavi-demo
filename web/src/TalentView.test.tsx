import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { TalentView } from "./TalentView";
import type { Candidate, Job, WorkAvailability } from "./api";

const ID = "11111111-0000-0000-0000-000000000001";

const candidate: Candidate = {
  id: ID,
  full_name: "Ada Okafor",
  email: null,
  phone: null,
  location: null,
  resume_text: "",
  source: "self",
  status: "active",
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-01T00:00:00Z",
};

const job: Job = {
  id: 7,
  kind: "parse_resume",
  payload: {},
  status: "succeeded",
  priority: 0,
  run_at: "2026-10-01T00:00:00Z",
  attempts: 1,
  max_attempts: 3,
  last_error: null,
  worker: null,
  started_at: null,
  finished_at: null,
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-01T00:00:00Z",
};

const stored: WorkAvailability = {
  candidate_id: ID,
  timezone: "Europe/London",
  work_start: "08:00",
  work_end: "16:30",
  hours_per_week: 30,
  available_from: "2026-11-02",
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-01T00:00:00Z",
};

type Route = (request: Request) => Response | Promise<Response>;

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

/** Stubs fetch with one handler per "METHOD /path"; anything else is a 404. Returns the requests seen. */
function api(routes: Record<string, Route>): Request[] {
  const seen: Request[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (request: Request) => {
      seen.push(request.clone());
      const route = routes[`${request.method} ${new URL(request.url).pathname}`];
      return route ? route(request) : json({ error: "not found" }, 404);
    }),
  );
  return seen;
}

function field(form: HTMLElement, label: string): HTMLInputElement {
  return within(form).getByLabelText(label);
}

beforeEach(() => {
  localStorage.clear();
});

describe("TalentView", () => {
  it("asks who the candidate is first, and calls nothing until they say", async () => {
    const seen = api({
      "POST /candidates": () => json(candidate, 201),
      [`GET /candidates/${ID}`]: () => json(candidate),
    });

    render(<TalentView />);
    expect(seen).toHaveLength(0);
    expect(screen.queryByRole("form", { name: "availability" })).not.toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Full name"), { target: { value: "Ada Okafor" } });
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));

    expect(await screen.findByText("Ada Okafor")).toBeInTheDocument();
    expect(seen[0].headers.get("X-Role")).toBe("talent");
    expect(localStorage.getItem("mavi.candidate")).toBe(ID);
  });

  it("shows the availability form after the upload and saves what the candidate enters", async () => {
    localStorage.setItem("mavi.candidate", ID);
    const seen = api({
      [`GET /candidates/${ID}`]: () => json(candidate),
      [`POST /candidates/${ID}/resume`]: () => json(job, 202),
      [`PUT /candidates/${ID}/availability`]: async (request) =>
        json({ ...stored, ...((await request.json()) as object) }, 201),
    });

    render(<TalentView />);
    const upload = await screen.findByLabelText("Resume (PDF or DOCX)");
    // No resume and no earlier answer: the form waits for the upload.
    expect(screen.queryByRole("form", { name: "availability" })).not.toBeInTheDocument();

    // Bytes stand in for the File: the stubbed fetch receives Node's Request, which cannot read jsdom's File.
    const file = new TextEncoder().encode("%PDF-1.4");
    fireEvent.change(upload, { target: { files: [file] } });
    fireEvent.click(screen.getByRole("button", { name: "Upload" }));

    const form = await screen.findByRole("form", { name: "availability" });
    // The upload is the file's own bytes, not JSON.
    const post = seen.find((r) => r.method === "POST")!;
    expect(post.headers.get("Content-Type")).toBe("application/octet-stream");
    expect(await post.text()).toBe("%PDF-1.4");
    expect(within(form).getByText(/will not be matched/)).toBeInTheDocument();
    fireEvent.change(field(form, "Time zone"), { target: { value: "America/Chicago" } });
    fireEvent.change(field(form, "Working hours start"), { target: { value: "08:30" } });
    fireEvent.change(field(form, "Working hours end"), { target: { value: "17:00" } });
    fireEvent.change(field(form, "Hours per week"), { target: { value: "32" } });
    fireEvent.change(field(form, "Earliest start date"), { target: { value: "2026-11-16" } });
    fireEvent.click(within(form).getByRole("button", { name: "Save availability" }));

    expect(await within(form).findByRole("status")).toHaveTextContent("Saved");
    const put = seen.find((r) => r.method === "PUT")!;
    expect(put.headers.get("X-Role")).toBe("talent");
    expect(put.headers.get("X-Actor")).toBe(ID);
    expect(await put.json()).toEqual({
      timezone: "America/Chicago",
      work_start: "08:30",
      work_end: "17:00",
      hours_per_week: 32,
      available_from: "2026-11-16",
    });
    // Once answered, the warning goes and the same form edits.
    expect(within(form).queryByText(/will not be matched/)).not.toBeInTheDocument();
    expect(within(form).getByRole("button", { name: "Update availability" })).toBeInTheDocument();
  });

  it("loads an earlier answer for editing", async () => {
    localStorage.setItem("mavi.candidate", ID);
    const seen = api({
      [`GET /candidates/${ID}`]: () => json({ ...candidate, resume_text: "Ada Okafor, CPA" }),
      [`GET /candidates/${ID}/availability`]: () => json(stored),
      [`PUT /candidates/${ID}/availability`]: async (request) => json({ ...stored, ...((await request.json()) as object) }),
    });

    render(<TalentView />);
    const form = await screen.findByRole("form", { name: "availability" });
    expect(field(form, "Time zone")).toHaveValue("Europe/London");
    expect(field(form, "Working hours start")).toHaveValue("08:00");
    expect(field(form, "Working hours end")).toHaveValue("16:30");
    expect(field(form, "Hours per week")).toHaveValue(30);
    expect(field(form, "Earliest start date")).toHaveValue("2026-11-02");

    fireEvent.change(field(form, "Hours per week"), { target: { value: "20" } });
    fireEvent.click(within(form).getByRole("button", { name: "Update availability" }));

    await within(form).findByRole("status");
    expect(await seen.find((r) => r.method === "PUT")!.json()).toMatchObject({ timezone: "Europe/London", hours_per_week: 20 });
  });

  it("shows the API's message beside the field it refuses", async () => {
    localStorage.setItem("mavi.candidate", ID);
    api({
      [`GET /candidates/${ID}`]: () => json({ ...candidate, resume_text: "Ada Okafor, CPA" }),
      [`PUT /candidates/${ID}/availability`]: () =>
        json({ error: "validation failed", fields: { work_end: "must differ from work_start" } }, 422),
    });

    render(<TalentView />);
    const form = await screen.findByRole("form", { name: "availability" });
    fireEvent.click(within(form).getByRole("button", { name: "Save availability" }));

    expect(await within(form).findByRole("alert")).toHaveTextContent("validation failed");
    expect(within(form).getByText("must differ from work_start")).toBeInTheDocument();
    expect(within(form).getByRole("button", { name: "Save availability" })).toBeInTheDocument();
  });

  it("does not offer an empty form when the stored answers could not be loaded", async () => {
    localStorage.setItem("mavi.candidate", ID);
    api({
      [`GET /candidates/${ID}`]: () => json({ ...candidate, resume_text: "Ada Okafor, CPA" }),
      [`GET /candidates/${ID}/availability`]: () => json({ error: "internal error" }, 500),
    });

    render(<TalentView />);

    expect(await screen.findByRole("alert")).toHaveTextContent("internal error");
    expect(screen.queryByRole("form", { name: "availability" })).not.toBeInTheDocument();
  });

  it("follows a parse that is still running until it finishes", async () => {
    localStorage.setItem("mavi.candidate", ID);
    let polls = 0;
    api({
      [`GET /candidates/${ID}`]: () => json({ ...candidate, resume_text: "Ada Okafor, CPA" }),
      [`GET /candidates/${ID}/resume/job`]: () => json({ ...job, status: polls++ === 0 ? "running" : "succeeded" }),
    });

    render(<TalentView />);

    expect(await screen.findByText("Resume received; reading it now.")).toBeInTheDocument();
    expect(await screen.findByText("Resume read.", undefined, { timeout: 4000 })).toBeInTheDocument();
  });

  it("says why when the resume could not be read", async () => {
    localStorage.setItem("mavi.candidate", ID);
    api({
      [`GET /candidates/${ID}`]: () => json(candidate),
      [`GET /candidates/${ID}/resume/job`]: () => json({ ...job, status: "failed", last_error: "no text in the file" }),
    });

    render(<TalentView />);

    expect(await screen.findByText(/could not read that resume: no text in the file/)).toBeInTheDocument();
  });

  it("lets the candidate try again, or leave, when their details could not be loaded", async () => {
    localStorage.setItem("mavi.candidate", ID);
    let down = true;
    const seen = api({
      [`GET /candidates/${ID}`]: () => (down ? json({ error: "internal error" }, 500) : json(candidate)),
    });

    render(<TalentView />);
    expect(await screen.findByRole("alert")).toHaveTextContent("internal error");

    down = false;
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("Ada Okafor")).toBeInTheDocument();
    // Every request is made as this candidate.
    expect(seen.every((request) => request.headers.get("X-Actor") === ID)).toBe(true);

    down = true;
    cleanup();
    render(<TalentView />);
    fireEvent.click(await screen.findByRole("button", { name: "Not you?" }));
    await waitFor(() => expect(screen.getByLabelText("Full name")).toBeInTheDocument());
    expect(localStorage.getItem("mavi.candidate")).toBeNull();
  });

  it("does not let a late answer about an earlier upload replace a newer one", async () => {
    localStorage.setItem("mavi.candidate", ID);
    let polls = 0;
    let answerPoll: (response: Response) => void = () => {};
    api({
      [`GET /candidates/${ID}`]: () => json({ ...candidate, resume_text: "Ada Okafor, CPA" }),
      // The page loads with job 7 running; the first poll for it is slow to answer.
      [`GET /candidates/${ID}/resume/job`]: () =>
        polls++ === 0 ? json({ ...job, id: 7, status: "running" }) : new Promise<Response>((resolve) => (answerPoll = resolve)),
      [`POST /candidates/${ID}/resume`]: () =>
        json({ ...job, id: 8, status: "failed", last_error: "no text in the file" }, 202),
    });

    render(<TalentView />);
    expect(await screen.findByText("Resume received; reading it now.")).toBeInTheDocument();
    await waitFor(() => expect(polls).toBe(2), { timeout: 4000 });

    // A second upload is answered while that poll is still out.
    fireEvent.change(screen.getByLabelText("Resume (PDF or DOCX)"), {
      target: { files: [new TextEncoder().encode("%PDF-1.4")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Upload" }));
    expect(await screen.findByText(/could not read that resume: no text in the file/)).toBeInTheDocument();

    // The poll comes back about job 7. It is older news.
    await act(async () => {
      answerPoll(json({ ...job, id: 7, status: "succeeded" }));
      await Promise.resolve();
    });
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(screen.getByText(/could not read that resume: no text in the file/)).toBeInTheDocument();
    expect(screen.queryByText("Resume read.")).not.toBeInTheDocument();
  });

  it("starts again when the stored candidate no longer exists", async () => {
    localStorage.setItem("mavi.candidate", ID);
    api({});

    render(<TalentView />);
    fireEvent.click(await screen.findByRole("button", { name: "Start again" }));

    await waitFor(() => expect(screen.getByLabelText("Full name")).toBeInTheDocument());
    expect(localStorage.getItem("mavi.candidate")).toBeNull();
  });
});
