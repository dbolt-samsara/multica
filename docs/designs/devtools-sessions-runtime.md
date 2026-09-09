# DevTools Sessions runtime

## Status

Design loop complete. Two independent adversarial reviews returned **MEETS** after amendments. No implementation, Session creation, credential change, deployment, or production use is authorized by this document.

## Goal

Add one built-in Multica runtime named **DevTools Sessions** for issue work.

```text
Multica issue
  -> agent bound to DevTools Sessions
  -> devtools session dispatch
  -> Runtime Cloud Bubo session
  -> devtools session watch
  -> existing Multica task timeline and result
```

Existing Chat providers remain unchanged. A Sessions-bound Worker is issue-only: the server rejects creating or sending a Chat with an agent whose runtime provider is `sessions`, and agent pickers omit that agent from Chat. This design does not route by prompt text and does not make one agent switch between Chat and issue work.

## User-visible first increment

1. Install and authenticate the `devtools` CLI on the computer that runs the Multica daemon.
2. Multica detects one **DevTools Sessions** runtime.
3. Create one Worker agent bound to that runtime.
4. Assign one GitHub-backed issue to the Worker.
5. Multica creates one Runtime Cloud Bubo Session.
6. Bubo activity appears in the existing task timeline.
7. The final Session result becomes the existing Multica issue comment.
8. Cancelling the Multica run requests Session cancellation.

The existing runtime picker, task queue, timeline, and issue page remain the UI. A generic runtime icon is acceptable.

## Why use the CLI

`devtools session` already owns the unstable platform boundary:

- user authentication;
- Session creation;
- SSE reconnect and terminal-capture replay;
- normalized NDJSON events;
- Session reads; and
- cancellation.

Multica only owns the adapter from its task contract to CLI arguments and from CLI JSON back to its existing message/result contract. It does not add an HTTP Sessions client or an SSE parser.

`devtools session` is not compatible with Multica's existing custom-runtime protocol families. It cannot be configured as a Codex, Claude, or ACP custom command because Multica would append the wrong protocol arguments. It is a new built-in backend that happens to execute the `devtools` binary.

## Fixed prototype policy

The first increment has no user-configurable Sessions settings:

| Setting | Value |
| --- | --- |
| Runtime | `cloud` |
| Tool | `bubo` |
| Model | `devtools/standard` |
| Repositories | exactly one GitHub repository from the issue's project |
| Prompt | self-contained issue brief over stdin |
| Approvals | runner default deny; never park |
| Default skills | enabled |
| MCP scopes | exact fixed list below |
| Per-run spend | explicit positive ceiling; value remains a separate decision |
| Retry | none automatically |

No setting falls back to a local agent runtime.

### Fixed scope list

At Derek's explicit request, the worker supplies every MCP scope in the `bubo-runner` grantable ceiling inspected at devbox-client commit `33800056ea7b3bdc4fefce44d2dc8ed659deaad2`, in policy order:

```text
mcp:databricks
mcp:buildkite
mcp:glean
mcp:atlassian
mcp:github
mcp:github:write
mcp:slack
mcp:cloudzero
mcp:datadog
mcp:incident_io
mcp:pagerduty
mcp:sentry
mcp:mixpanel_us
mcp:mixpanel_eu
mcp:launchdarkly
mcp:granola
mcp:bitrise
```

`llm:inference` is not an MCP provider scope and is injected by the Sessions mint path after MCP policy resolution. It is not passed through `--mcp-scope`. `mcp:sessions` and `mcp:aws` are intentionally outside the dispatched `bubo-runner` ceiling.

This broad list is an owner-approved prototype policy, not a claim of least privilege or evidence that each provider is needed. It includes `mcp:github:write` and operational-data providers. Multica sends it verbatim and fails the dispatch if the deployed server rejects any value. Adding a future scope requires an explicit code change and review. A listed scope does not prove the user connected the corresponding provider.

## Required CLI prerequisite

The current CLI does not expose `--max-spend-usd`. Add this one direct passthrough flag to the shared `devtools session dispatch` command before enabling the Multica Worker. This is the only required change outside the Multica fork.

The spend flag must always contain the approved positive ceiling. An absent ceiling is not allowed.

Until the CLI contains the spend flag, no Worker dispatch is allowed. Every accepted Worker dispatch sends once, never retries an ambiguous response, and classifies a lost response under the existing failed task status with stable `failure_reason=dispatch_unknown`—not a new task state.

## CLI lifecycle

### Create

Multica sends the prompt on stdin, not in process arguments or a persistent prompt file:

```text
devtools session dispatch
  --runtime cloud
  --tool bubo
  --model devtools/standard
  --repo owner/repository@ref
  --thread multica:<task-id>
  --metadata multica_task_id=<task-id>
  --metadata multica_issue_id=<issue-id>
  --max-spend-usd <approved-ceiling>
  --mcp-scope <scope> ...
  --json
```

Parse stdout as the register response. Persist and emit its `session_id` before starting the watcher. Stderr is diagnostic only.

### Watch

```text
devtools session watch <session-id> --json --since 0
```

Each stdout line is one Bubo event JSON object. The backend keeps the highest observed event sequence in memory and skips repeated sequence numbers. It maps only:

- `assistant_delta` -> text;
- `tool_call_started` -> tool use;
- `tool_call_finished` -> tool result summary;
- `state_changed` -> status.

`cost_tick`, approvals, input events, limit warnings, and unknown event types are ignored with a bounded diagnostic. Final cost comes only from the terminal Session row. The prototype timeline is best-effort observability, not completion authority. Persisted cursor recovery and perfect transcript reconstruction are deferred.

Only the typed text and tool summary fields enter `agent.Message`. They pass through Multica's existing redaction and tool-output truncation. Raw event JSON, prompts, scope credentials, and full tool arguments are not logged or persisted as timeline content.

### Final read

After every watcher exit, including exit zero, run:

```text
devtools session get <session-id> --json
```

Only this Session row decides the Multica result:

| Session | Multica result |
| --- | --- |
| `succeeded` | completed with `result_summary` |
| `failed` | failed with the stable failure details available in the row |
| `cancelled` | cancelled |
| `queued` or `running` | watcher failure; task remains failed/blocked, never reported completed |

Worker prose and `pr_url` are not independent PR proof. This first increment only returns the Session result to the issue; it does not add PR verification or issue-status automation.

### Cancel

When the Multica task context is cancelled:

1. stop the local watcher process;
2. run `devtools session cancel <session-id> --json` with a short independent cleanup context; and
3. run `devtools session get <session-id> --json` once.

Cancellation is best effort. A nonterminal or unreadable final row is reported as an unknown remote obligation, not as proved cancellation.

In the server transaction that first changes a Sessions-bound Multica task to `cancelled`, immediately write `context.sessions_remote_cleanup={status:"unknown", remote_session_id:<pinned-id-if-present>}`. This happens before the daemon can acknowledge cancellation, so a daemon crash cannot leave a plain cancelled task while remote work may still spend.

Extend the existing daemon cancellation acknowledgement with `remote_session_id` and `remote_cleanup_status=confirmed|unknown`. A successful terminal readback conditionally refines the context to `confirmed`; an absent/failed acknowledgement never clears `unknown`. No migration or new task state is needed. Task detail exposes the warning.

## Self-contained prompt

The existing Multica issue prompt is local-only. It tells the agent to run `multica issue get`, while the full issue instructions live in a local runtime file. The Runtime Cloud worker has neither that file nor `MULTICA_TOKEN`.

For the Sessions Worker only, the daemon builds one prompt containing:

- issue title and description;
- current trigger and coalesced comments already present in the claimed task;
- agent instructions;
- workspace and project context;
- the canonical repository and ref; and
- a short final-result request.

The prompt does not tell Bubo to call Multica. The existing Multica daemon writes progress and the final issue comment. No Multica credential is sent to the Session.

The existing claim already carries the issue title as `ThreadName`; add only the missing issue description.

Before create, the daemon builds the complete CLI intent once in memory—prompt, repository/ref, model, tool, thread, metadata, spend ceiling, and ordered scopes—and executes it once. No live field is reread during that attempt. Because automatic create replay and restart adoption are out of scope, the prototype does not persist a second copy of the issue/comment prompt.

## Minimal Multica changes

### Runtime discovery

- Detect `devtools` through `MULTICA_SESSIONS_PATH` or `PATH`.
- Run the exact read-only preflight `devtools auth status`, then `devtools session list --json --limit 1`. Register the runtime online only when the executable, usable human login cache, and the default Agent Gateway Sessions door all succeed. Every dispatch/watch/get/cancel command also omits `-e` and therefore uses that same door. Neither preflight command creates a Session.
- Run under one declared daemon OS user and that user's persistent `HOME`/DevTools OAuth cache. The CLI resolves authorization per request; Multica never copies token text into its database, agent environment, prompt, or logs. Login, expiry, revocation, and reauthentication remain owned by `devtools auth login` outside a task.
- The prototype runtime and Worker are private. Server authorization requires the human initiating the issue run to equal the runtime owner. Other workspace members, agent-to-agent handoffs, Autopilots, quick-create, and squads cannot invoke it.
- Register it under provider `sessions` with display name **DevTools Sessions**.
- Keep the registered Multica `runtime_mode` as `local`: it describes the daemon-hosted control runtime, while the backend sends execution to Runtime Cloud.
- Do not add `sessions` to custom runtime protocol families.

Owning code: `server/internal/daemon/config.go`, `agents_probe.go`, and the existing generic runtime registration path.

### Backend

- In `runTask`, branch to `runSessionsTask` immediately after task identity/workspace validation and before skill resolution, local `execenv`, local MCP brokers, Multica-token injection, workdir reuse, model discovery, resume logic, and local idle/tool watchdogs.
- `runSessionsTask` uses the one in-memory intent and Sessions/Bubo limits as its liveness boundary. It does not create or report a local work directory.
- Add `sessionsBackend` under `server/pkg/agent/`.
- Add one explicit standalone `sessions` branch to `ResolveBackend` before the existing built-in-identity and protocol-family factories.
- Keep `agent.New`, `SupportedTypes`, the built-in protocol descriptor registry, custom runtime profiles, and their database CHECK unchanged. `sessions` is neither a custom protocol family nor an identity layered on an existing family.
- Use the existing command/process-tree helpers for every CLI process.
- Reuse `agent.Session`, `agent.Message`, and `agent.Result`; do not add another task state machine.
- Add a narrow synchronous `PersistSessionID` callback to the Sessions execution options. After dispatch returns, the backend must successfully call the existing task-session pin endpoint before starting `watch`. If the pin fails, it attempts cancellation and returns an unknown remote obligation; the pin failure is never only logged.

### Per-run data

- Add the missing issue description to the claim and one typed in-memory Sessions intent to the daemon task/`agent.ExecOptions` boundary.
- Build that intent once from the claimed task's existing project-narrowed repositories.
- Accept only one losslessly normalized GitHub `owner/repository@<full-commit-sha>` on the approved GitHub host. Reject a missing/empty ref, branch name, multiple repositories, local path, SSH URL, or unsupported host. The prototype never resolves a drifting default branch implicitly.
- The prototype prerequisite is one project GitHub resource already configured with a full commit SHA. Multica performs no branch/default-to-SHA resolution.
- Do not pass Multica custom arguments, local MCP configuration, skills, connected apps, or local work directories to Sessions.

### Existing behavior reused unchanged

- runtime and agent binding;
- issue task queue and claim;
- task cancellation watcher;
- task-message batching and timeline;
- terminal task reporting; and
- synthesized final issue comment.

### Focused existing-surface changes

- Chat create/send handlers reject a Sessions-bound agent; Chat agent pickers omit it. Existing non-Sessions Chat execution is unchanged.
- The daemon and frontend display-name maps label provider `sessions` as **DevTools Sessions**; the generic icon remains.
- The server cancellation transaction initializes `sessions_remote_cleanup=unknown`; the daemon acknowledgement only refines it to confirmed, and task detail shows the warning.
- Provider-aware failure classification prevents automatic retries for Sessions-bound tasks.

No new database table, task state, queue, runtime pool, UI page, logo, or configuration form is required.

## Retry and crash boundary

Multica must not apply its existing automatic task retry policy to a task whose runtime provider is `sessions`. A daemon/runtime failure can leave remote work running, and a retry child has a new Multica task ID. The task remains failed/blocked until the pinned Session is terminal or an operator reconciles the unique task-scoped thread. A human may then create a new run deliberately.

The one in-memory intent is executed once. The prototype performs no automatic create replay and does not authorize a new retry task.

If the daemon exits after create but before the synchronous Session-ID pin, the outcome is `dispatch_unknown`. The approved spend ceiling bounds the remote exposure; Multica never creates another Session automatically. The unique thread `multica:<task-id>` is for operator discovery and cleanup only, not client-side find-then-create adoption.

Graceful cancellation follows the cancel/readback contract below. An abrupt daemon exit remains an explicitly unsupported prototype case, but it cannot trigger duplicate automatic work.

## Failure policy

| Failure | Result |
| --- | --- |
| `devtools` missing or auth preflight fails | runtime is unavailable; no task dispatch |
| invalid/multiple repository | fail before CLI create |
| explicit scope rejected | fail; never remove the scope silently |
| create transport failure | `dispatch_unknown`; no automatic replay |
| create response remains unknown | `dispatch_unknown`; no new key or list-then-create |
| watcher disconnect | CLI owns bounded reconnect; final GET still required |
| watcher exits without terminal row | failed/blocked, not completed |
| Multica cancellation before Session ID | stop create; if outcome is unknown, `dispatch_unknown` |
| Multica cancellation after Session ID | cancel then read back |
| cancel/readback remains nonterminal or unreadable | persist `context.sessions_remote_cleanup.status=unknown` with Session ID and show it in task detail |
| daemon exits | Session may continue; restart adoption is deferred and this limitation is visible |
| task fails for any reason | Sessions-bound tasks are not automatically retried |

## Explicit non-goals

- Chat execution changes for existing providers or prompt-based Chat/Work routing. The only Chat change is the Sessions-Worker eligibility rejection.
- Multiple repositories.
- Follow-up Session resume or transcript lineage.
- Steering, approvals, or parked runs.
- User-selectable runtime, tool, model, scopes, shape, or image.
- Multica skills, connected apps, custom MCP, custom environment, or custom arguments in Runtime Cloud.
- Raw Sessions HTTP, raw SSE, webhook sinks, or a second auth implementation.
- Persisted SSE cursors, restart adoption, or perfect timeline reconstruction.
- Multi-user/service identity.
- Runtime pools, Autopilots, quick-create, squads, or generic remote-provider abstractions.
- PR verification, automatic issue status changes, deployment, or production rollout.
- Refactoring Multica's existing local runtime pipeline.

These can be separate designs only after the first increment produces useful evidence.

## Implementation sequence

1. **CLI prerequisite:** expose and test `--max-spend-usd`.
2. **Hidden adapter contract:** add the standalone factory route, early remote branch, one-shot create, synchronous Session-ID pin, watch-to-terminal without projecting events, final GET, cancel/readback, and no-auto-retry rule. The runtime is not registered or assignable yet.
3. **Runtime registration:** add discovery/auth preflight and issue-only invocation gates. Keep the runtime unavailable unless the complete hidden adapter contract is present.
4. **Live timeline:** map the four approved event types into existing messages.
5. **Disposable proof:** one issue, one Session, one result, then explicit cleanup.

Each step is independently reviewable and leaves the existing Chat and local providers unchanged.

## Acceptance criteria

- A separate Worker agent bound to **DevTools Sessions** handles one GitHub-backed issue.
- The Worker is rejected from Chat, and only its human runtime owner can invoke it from an issue.
- One immutable CLI intent is built once for the task before create. A successful path creates exactly one Session for that intent; an unknown path never automatically creates another.
- The request contains the fixed runtime, tool, model, spend ceiling, immutable repository SHA, metadata, and all 17 fixed MCP scopes exactly once in the pinned order.
- Bubo runs on Runtime Cloud; no local coding agent handles the issue.
- The existing Multica timeline shows best-effort assistant/tool progress.
- The final Multica result comes from `devtools session get`, not watcher exit or worker prose.
- Cancellation attempts Session cancellation and reports only the observed readback.
- Unknown remote cleanup remains visible and durable on the cancelled Multica task.
- Focused cancellation tests cover cancel before Session-ID pin, cancel after pin, lost daemon acknowledgement, failed readback, and confirmed terminal readback.
- Sessions-bound task failures never enter Multica's automatic retry path.
- Existing Chat and non-Sessions runtime behavior is byte-for-byte unchanged by focused regression tests.
- No item in the non-goals is implemented as part of this increment.

## Evidence and open decisions

### Adversarial review

Two independent reviewers separately challenged scope/incrementalism and technical correctness. Their first reviews rejected the draft. The incorporated corrections:

- make the Sessions Worker issue-only and server-reject it from Chat;
- use one standalone backend branch without widening custom runtime families;
- record the broad 17-scope list as Derek's explicit policy and remove injected `llm:inference` from CLI input;
- require one full-SHA project resource and no ref resolver;
- use one-shot dispatch, a synchronous Session-ID pin, no automatic task retry, and durable unknown cancellation cleanup;
- branch before local workdir, skill, MCP, token, resume, and watchdog setup;
- use the same Agent Gateway Sessions door for preflight and every lifecycle command;
- remove live-cost, persisted-cursor, restart-adoption, runtime-pool, and other adjacent work; and
- land the complete hidden adapter before the runtime becomes assignable.

Both final re-reviews returned **MEETS** with no remaining findings and no implementation authority.

Inspected Multica baseline: `9fab6da917953c6f1a967a17beef045aff3d0ce5`.

Inspected installed CLI: `/Users/derek.bolt/.local/bin/devtools`, `devtools version v0.1.1267-1-ge5860e96f`; its help and one read-only authenticated list call support dispatch, JSON watch, get, cancel, explicit scopes, Runtime Cloud, Bubo, repositories, thread, metadata, and model selection. No inference call or Session create was performed.

Inspected devbox-client source for the missing CLI spend flag at `33800056ea7b3bdc4fefce44d2dc8ed659deaad2`. The fixed MCP ceiling comes from that commit's policy blob `2cd298bc207fa1b0a2fe65b7544787e0904e7983`. Deployed version parity remains a preflight; a deployed policy that rejects any pinned scope blocks dispatch rather than shrinking the list.

Before implementation, decide only:

1. the positive per-run spend ceiling; and
2. whether the CLI spend flag will land before the Multica runtime work begins or as its explicit blocking dependency.

Everything else is fixed by this design or deferred.
