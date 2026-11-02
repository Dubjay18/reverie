// Thin TypeScript client for the Reverie HTTP JSON API (/v1/*).

export interface Scope {
  tenant: string;
  agent: string;
  project: string;
  user: string;
}

export interface EventInput {
  id?: string;
  task_id: string;
  kind: string;
  origin?: string;
  at?: string;
  text?: string;
  payload?: Record<string, unknown>;
}

export interface Fact {
  ID: string;
  Entity: string;
  Relation: string;
  Value: string;
  Status: string;
  Source: string;
  Confidence: number;
  Pinned: boolean;
  EpisodeIDs?: string[];
}

export interface ContextPack {
  Facts?: Array<{ Text: string; Citation: string; Pinned?: boolean }>;
  Conflicts?: Array<{ Text: string; Citation: string }>;
  Warm?: Array<{ Text: string; Citation: string }>;
  Hot?: Array<{ Text: string; Citation: string }>;
  Tokens: number;
  Budget: number;
  Dropped: number;
}

export interface RecallResult {
  pack: ContextPack;
  rendered: string;
}

export interface DreamReport {
  run_id: string;
  trigger: string;
  status: string;
  episodes: number;
  learned: number;
  merged: number;
  superseded: number;
  contested: number;
  forgotten: number;
  rejected: number;
  tokens_spent: number;
  tokens_budget: number;
}

export class ReverieClient {
  private readonly baseURL: string;
  private readonly scope: Scope;

  constructor(baseURL: string, scope: Scope) {
    this.baseURL = baseURL.replace(/\/+$/, "");
    this.scope = scope;
  }

  private async post<T>(path: string, body: Record<string, unknown>): Promise<T> {
    const res = await fetch(`${this.baseURL}${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ scope: this.scope, ...body }),
    });
    const data = await res.json();
    if (!res.ok) {
      throw new Error(data.error || `HTTP ${res.status}`);
    }
    return data as T;
  }

  async record(events: EventInput[]): Promise<{ ids: string[] }> {
    return this.post<{ ids: string[] }>("/v1/record", { events });
  }

  async recall(task: string, budget = 1200): Promise<RecallResult> {
    return this.post<RecallResult>("/v1/recall", { task, budget });
  }

  async expand(ref: string): Promise<{ events: Array<Record<string, unknown>> }> {
    return this.post<{ events: Array<Record<string, unknown>> }>("/v1/expand", { ref });
  }

  async remember(input: {
    text?: string;
    entity?: string;
    relation?: string;
    value?: string;
    source?: string;
  }): Promise<{ fact: Fact }> {
    return this.post<{ fact: Fact }>("/v1/remember", input);
  }

  async forget(ref: string): Promise<{ run_id: string }> {
    return this.post<{ run_id: string }>("/v1/forget", { ref });
  }

  async dream(trigger = "manual", budget = 4000): Promise<DreamReport> {
    return this.post<DreamReport>("/v1/dream", { trigger, budget });
  }

  async revert(runID: string): Promise<{ reverted: number }> {
    return this.post<{ reverted: number }>("/v1/revert", { run_id: runID });
  }
}

// Optional CLI entrypoint used by cross-surface parity tests:
//   node --experimental-strip-types adapters/http/client.ts <baseURL>
const isMain =
  typeof process !== "undefined" &&
  Array.isArray(process.argv) &&
  process.argv[1]?.endsWith("client.ts") &&
  Boolean(process.argv[2]);

if (isMain) {
  (async () => {
    const baseURL = process.argv[2];
    const scope: Scope = {
      tenant: "local",
      agent: "ts-agent",
      project: "/workspace/demo",
      user: "jay",
    };
    const client = new ReverieClient(baseURL, scope);

    // Session 1: fail a migration task, remember a preference, and run a dream.
    await client.record([
      {
        task_id: "sess-1",
        kind: "message",
        origin: "user_input",
        text: "Run database migrations on staging",
      },
      {
        task_id: "sess-1",
        kind: "decision",
        origin: "agent_decision",
        text: "Always acquire pg_advisory_lock before running migrations",
      },
      {
        task_id: "sess-1",
        kind: "task.ended",
        origin: "system",
        payload: {
          outcome: "failure",
          summary: "Migration failed with lock timeout when concurrent worker ran",
        },
      },
    ]);
    const rem = await client.remember({
      entity: "jay",
      relation: "prefers",
      value: "PostgreSQL 16",
      text: "Jay prefers PostgreSQL 16",
    });
    const dreamRep = await client.dream("manual", 4000);

    // Session 2: recall memory for the next migration task and expand the episode ref.
    const recalled = await client.recall("Run database migrations for jay on staging", 1200);
    const expanded = await client.expand("ep:sess-1");

    console.log(
      JSON.stringify({
        fact_id: rem.fact.ID,
        dream_learned: dreamRep.learned,
        rendered: recalled.rendered,
        expanded_count: expanded.events.length,
      })
    );
  })().catch((err) => {
    console.error(err);
    process.exit(1);
  });
}
