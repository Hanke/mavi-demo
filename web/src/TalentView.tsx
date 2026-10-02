import { useEffect, useMemo, useState, type FormEvent } from "react";
import {
  createApiClient,
  type ApiClient,
  type ApiError,
  type Candidate,
  type Job,
  type WorkAvailability,
  type WorkAvailabilityInput,
} from "./api";

// The talent side of intake: say who you are, upload a resume, then answer
// the four things a resume cannot (time zone, working hours, hours a week,
// start date). Until those are answered the candidate is not matched.

/** Where the browser remembers which candidate this is; there is no real sign-in. */
const STORAGE_KEY = "mavi.candidate";
const JOB_POLL_MS = 1500;

function storedCandidate(): string | null {
  try {
    return localStorage.getItem(STORAGE_KEY);
  } catch {
    return null;
  }
}

function errorMessage(error: ApiError | undefined, status: number): string {
  return error?.error ?? `HTTP ${status}`;
}

export function TalentView() {
  const [candidateId, setCandidateId] = useState<string | null>(storedCandidate);

  function remember(id: string | null) {
    try {
      if (id === null) localStorage.removeItem(STORAGE_KEY);
      else localStorage.setItem(STORAGE_KEY, id);
    } catch {
      // Without storage the id lasts until the page is reloaded.
    }
    setCandidateId(id);
  }

  return (
    <section aria-label="talent">
      <h2>Talent</h2>
      {candidateId === null ? (
        <SignUp onCreated={remember} />
      ) : (
        <Intake key={candidateId} candidateId={candidateId} onForget={() => remember(null)} />
      )}
    </section>
  );
}

function SignUp({ onCreated }: { onCreated: (id: string) => void }) {
  const [fullName, setFullName] = useState("");
  const [email, setEmail] = useState("");
  const [problem, setProblem] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setProblem(null);
    try {
      const { data, error, response } = await createApiClient({ role: "talent" }).POST("/candidates", {
        body: { full_name: fullName, email: email.trim() === "" ? null : email },
      });
      if (data) onCreated(data.id);
      else setProblem(fieldMessages(error) ?? errorMessage(error, response.status));
    } catch (err) {
      setProblem(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit}>
      <p className="muted">Start by telling us who you are.</p>
      <label>
        Full name
        <input value={fullName} onChange={(e) => setFullName(e.target.value)} required />
      </label>
      <label>
        Email
        <input type="email" value={email} onChange={(e) => setEmail(e.target.value)} />
      </label>
      <button type="submit" disabled={busy}>
        Continue
      </button>
      {problem && <p role="alert">{problem}</p>}
    </form>
  );
}

function fieldMessages(error: ApiError | undefined): string | null {
  if (!error?.fields) return null;
  return Object.entries(error.fields)
    .map(([field, message]) => `${field}: ${message}`)
    .join("; ");
}

type Loaded =
  | { kind: "loading" }
  | { kind: "ready"; candidate: Candidate; availability: WorkAvailability | null }
  | { kind: "gone" }
  | { kind: "error"; message: string };

function Intake({ candidateId, onForget }: { candidateId: string; onForget: () => void }) {
  const client = useMemo(() => createApiClient({ role: "talent", actor: candidateId }), [candidateId]);
  const [state, setState] = useState<Loaded>({ kind: "loading" });
  const [job, setJob] = useState<Job | null>(null);

  useEffect(() => {
    let current = true;
    const path = { params: { path: { id: candidateId } } };
    Promise.all([
      client.GET("/candidates/{id}", path),
      client.GET("/candidates/{id}/availability", path),
      client.GET("/candidates/{id}/resume/job", path),
    ])
      .then(([candidate, availability, resumeJob]) => {
        if (!current) return;
        if (candidate.response.status === 404) {
          setState({ kind: "gone" });
        } else if (!candidate.data) {
          setState({ kind: "error", message: errorMessage(candidate.error, candidate.response.status) });
        } else if (!availability.data && availability.response.status !== 404) {
          // Only a 404 means nothing was supplied. On any other failure an
          // empty form would invite the candidate to overwrite their answers.
          setState({ kind: "error", message: errorMessage(availability.error, availability.response.status) });
        } else {
          setState({ kind: "ready", candidate: candidate.data, availability: availability.data ?? null });
          // A parse still running from before a reload is picked up again.
          if (resumeJob.data) setJob((newer) => newer ?? resumeJob.data);
        }
      })
      .catch((err: unknown) => {
        if (current) setState({ kind: "error", message: err instanceof Error ? err.message : String(err) });
      });
    return () => {
      current = false;
    };
  }, [client, candidateId]);

  if (state.kind === "loading") return <p>Loading your details…</p>;
  if (state.kind === "error") return <p role="alert">Could not load your details: {state.message}</p>;
  if (state.kind === "gone") {
    return (
      <div>
        <p role="alert">We no longer have a record for this browser.</p>
        <button type="button" onClick={onForget}>
          Start again
        </button>
      </div>
    );
  }

  const { candidate, availability } = state;
  // The form follows the upload. Someone coming back to edit already has a
  // resume or an earlier answer, so they get it straight away.
  const showAvailability = job !== null || candidate.resume_text !== "" || availability !== null;
  return (
    <div>
      <p>
        Signed in as <strong>{candidate.full_name}</strong>.{" "}
        <button type="button" className="link" onClick={onForget}>
          Not you?
        </button>
      </p>
      <ResumeUpload client={client} candidateId={candidateId} job={job} onJob={setJob} />
      {showAvailability && <AvailabilityForm client={client} candidateId={candidateId} initial={availability} />}
    </div>
  );
}

function ResumeUpload(props: { client: ApiClient; candidateId: string; job: Job | null; onJob: (job: Job) => void }) {
  const { client, candidateId, job, onJob } = props;
  const [file, setFile] = useState<File | null>(null);
  const [problem, setProblem] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const pending = job !== null && (job.status === "queued" || job.status === "running");
  useEffect(() => {
    if (!pending) return;
    const timer = setInterval(() => {
      client
        .GET("/candidates/{id}/resume/job", { params: { path: { id: candidateId } } })
        .then(({ data }) => {
          if (data) onJob(data);
        })
        .catch(() => {
          // A failed poll is retried on the next tick.
        });
    }, JOB_POLL_MS);
    return () => clearInterval(timer);
  }, [client, candidateId, pending, onJob]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (file === null) return;
    setBusy(true);
    setProblem(null);
    try {
      // The body is the file itself, not JSON and not multipart.
      const { data, error, response } = await client.POST("/candidates/{id}/resume", {
        params: { path: { id: candidateId } },
        body: file as unknown as string,
        bodySerializer: (body) => body as unknown as BodyInit,
        headers: { "Content-Type": "application/octet-stream" },
      });
      if (data) onJob(data);
      else setProblem(errorMessage(error, response.status));
    } catch (err) {
      setProblem(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit}>
      <h3>Your resume</h3>
      <label>
        Resume (PDF or DOCX)
        <input
          type="file"
          accept=".pdf,.docx,application/pdf"
          onChange={(e) => setFile(e.target.files?.[0] ?? null)}
        />
      </label>
      <button type="submit" disabled={busy || file === null}>
        Upload
      </button>
      {problem && <p role="alert">{problem}</p>}
      {job && <p data-job={job.status}>{jobMessage(job)}</p>}
    </form>
  );
}

function jobMessage(job: Job): string {
  switch (job.status) {
    case "succeeded":
      return "Resume read.";
    case "failed":
      return `We could not read that resume: ${job.last_error ?? "unknown error"}`;
    default:
      return "Resume received; reading it now.";
  }
}

function browserTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

function timeZones(selected: string): string[] {
  let zones: string[] = [];
  try {
    zones = Intl.supportedValuesOf("timeZone");
  } catch {
    // An older browser: the selected zone is the only option.
  }
  // The list leaves out UTC and the old names some browsers still report.
  return zones.includes(selected) ? zones : [selected, ...zones];
}

function today(): string {
  const now = new Date();
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`;
}

type Saved = { kind: "idle" } | { kind: "saving" } | { kind: "saved" } | { kind: "failed"; message: string };

function AvailabilityForm(props: { client: ApiClient; candidateId: string; initial: WorkAvailability | null }) {
  const { client, candidateId, initial } = props;
  const [timezone, setTimezone] = useState(initial?.timezone ?? browserTimeZone);
  const [workStart, setWorkStart] = useState(initial?.work_start ?? "09:00");
  const [workEnd, setWorkEnd] = useState(initial?.work_end ?? "17:00");
  const [hoursPerWeek, setHoursPerWeek] = useState(String(initial?.hours_per_week ?? 40));
  const [availableFrom, setAvailableFrom] = useState(initial?.available_from ?? today);
  const [answered, setAnswered] = useState(initial !== null);
  const [saved, setSaved] = useState<Saved>({ kind: "idle" });
  const [fields, setFields] = useState<Record<string, string>>({});
  const zones = useMemo(() => timeZones(timezone), [timezone]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setSaved({ kind: "saving" });
    setFields({});
    const body: WorkAvailabilityInput = {
      timezone,
      work_start: workStart,
      work_end: workEnd,
      hours_per_week: Number(hoursPerWeek),
      available_from: availableFrom,
    };
    try {
      const { data, error, response } = await client.PUT("/candidates/{id}/availability", {
        params: { path: { id: candidateId } },
        body,
      });
      if (data) {
        setAnswered(true);
        setSaved({ kind: "saved" });
      } else {
        setFields(error?.fields ?? {});
        setSaved({ kind: "failed", message: errorMessage(error, response.status) });
      }
    } catch (err) {
      setSaved({ kind: "failed", message: err instanceof Error ? err.message : String(err) });
    }
  }

  const problem = (field: string) => fields[field] && <span className="field-error">{fields[field]}</span>;
  return (
    <form onSubmit={submit} aria-label="availability">
      <h3>When and where you can work</h3>
      {!answered && (
        <p className="muted">A resume cannot tell us these. You will not be matched to roles until you answer them.</p>
      )}
      <label>
        Time zone
        <select value={timezone} onChange={(e) => setTimezone(e.target.value)}>
          {zones.map((zone) => (
            <option key={zone} value={zone}>
              {zone}
            </option>
          ))}
        </select>
        {problem("timezone")}
      </label>
      <label>
        Working hours start
        <input type="time" value={workStart} onChange={(e) => setWorkStart(e.target.value)} required />
        {problem("work_start")}
      </label>
      <label>
        Working hours end
        <input type="time" value={workEnd} onChange={(e) => setWorkEnd(e.target.value)} required />
        {problem("work_end")}
      </label>
      <label>
        Hours per week
        <input
          type="number"
          min={1}
          max={80}
          step={1}
          value={hoursPerWeek}
          onChange={(e) => setHoursPerWeek(e.target.value)}
          required
        />
        {problem("hours_per_week")}
      </label>
      <label>
        Earliest start date
        <input type="date" value={availableFrom} onChange={(e) => setAvailableFrom(e.target.value)} required />
        {problem("available_from")}
      </label>
      <button type="submit" disabled={saved.kind === "saving"}>
        {answered ? "Update availability" : "Save availability"}
      </button>
      {saved.kind === "saved" && <p role="status">Saved. You can change these at any time.</p>}
      {saved.kind === "failed" && <p role="alert">Not saved: {saved.message}</p>}
    </form>
  );
}
