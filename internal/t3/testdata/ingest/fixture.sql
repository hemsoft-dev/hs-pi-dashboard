-- Offline T3 V2 database for the pmd ingest parity test. Table definitions are
-- copied from a real statev2.sqlite (T3 0.1.x, October 2026); rows are invented.
CREATE TABLE orchestration_v2_projection_threads (
      thread_id TEXT PRIMARY KEY,
      project_id TEXT NOT NULL,
      title TEXT NOT NULL,
      default_provider TEXT NOT NULL,
      runtime_mode TEXT NOT NULL,
      interaction_mode TEXT NOT NULL,
      active_provider_thread_id TEXT,
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL,
      archived_at TEXT,
      deleted_at TEXT,
      payload_json TEXT NOT NULL
    , provider_instance_id TEXT);
CREATE TABLE projection_projects (
      project_id TEXT PRIMARY KEY,
      title TEXT NOT NULL,
      workspace_root TEXT NOT NULL,
      scripts_json TEXT NOT NULL,
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL,
      deleted_at TEXT
    , default_model_selection_json TEXT, default_thread_env_mode TEXT, favicon_path TEXT, auto_pull INTEGER NOT NULL DEFAULT 0, project_icon_json TEXT);
CREATE TABLE orchestration_v2_projection_runs (
      run_id TEXT PRIMARY KEY,
      thread_id TEXT NOT NULL,
      ordinal INTEGER NOT NULL,
      provider TEXT NOT NULL,
      provider_thread_id TEXT,
      status TEXT NOT NULL,
      requested_at TEXT NOT NULL,
      completed_at TEXT,
      payload_json TEXT NOT NULL
    , provider_instance_id TEXT);
CREATE TABLE orchestration_v2_projection_runtime_requests (
      runtime_request_id TEXT PRIMARY KEY,
      thread_id TEXT NOT NULL,
      node_id TEXT NOT NULL,
      provider_turn_id TEXT,
      kind TEXT NOT NULL,
      status TEXT NOT NULL,
      created_at TEXT NOT NULL,
      resolved_at TEXT,
      payload_json TEXT NOT NULL
    );
CREATE TABLE projection_thread_pull_requests (
      thread_id TEXT NOT NULL,
      host TEXT NOT NULL,
      repository TEXT NOT NULL,
      number INTEGER NOT NULL,
      url TEXT NOT NULL,
      source TEXT NOT NULL,
      linked_at TEXT NOT NULL,
      snapshot_json TEXT,
      stack_json TEXT,
      PRIMARY KEY (thread_id, host, repository, number)
    );

INSERT INTO projection_projects VALUES
 ('p-ghx','gh-x','/w/gh-x','[]','2026-10-01T00:00:00.000Z','2026-10-01T00:00:00.000Z',NULL,NULL,NULL,NULL,0,NULL),
 ('p-chief','chief','/w/chief','[]','2026-10-01T00:00:00.000Z','2026-10-01T00:00:00.000Z',NULL,NULL,NULL,NULL,0,NULL);

-- Threads: live, queued behind completed, queued behind failed, stale queued,
-- settled, settled before its latest run, settled without runs, PR watch,
-- a /root/ subagent with lineage, an unknown project, archived and deleted.
INSERT INTO orchestration_v2_projection_threads VALUES
 ('t-live','p-ghx','Live run ✓ «quoted»','codex','full','default',NULL,'2026-10-26T10:00:00.000Z','2026-10-26T12:00:00.000Z',NULL,NULL,
  '{"branch":"feat/live","creationSource":"user","modelSelection":{"model":"gpt-6.1-sol","options":{"effort":"high"}},"lineage":{},"pullRequests":[{"repository":"hemsoft-dev/gh-x","number":12,"snapshot":{"state":"open"},"watch":{"since":"2026-10-26T11:00:00.000Z"}},{"repository":"hemsoft-dev/gh-x","number":13,"snapshot":null,"watch":0}],"linkedPullRequest":{"repository":"hemsoft-dev/gh-x","number":12,"snapshot":{"state":"open"},"watch":true},"branchPullRequest":"not-an-object"}',NULL),
 ('t-queued-completed','p-ghx','Queued behind completed','codex','full','default',NULL,'2026-10-26T09:00:00.000Z','2026-10-26T11:30:00.000Z',NULL,NULL,
  '{"branch":null,"modelSelection":{"model":"claude-opus-5-5"}}',NULL),
 ('t-queued-failed','p-chief','Queued behind failed','claudeAgent','full','default',NULL,'2026-10-26T09:00:00.000Z','2026-10-26T11:31:00.000Z',NULL,NULL,
  '{}',NULL),
 ('t-stale-queued','p-chief','Stale queued run','claudeAgent','full','default',NULL,'2026-10-26T08:00:00.000Z','2026-10-26T08:30:00.000Z',NULL,NULL,
  '{"creationSource":"mcp"}',NULL),
 ('t-settled','p-chief','Settled','codex','full','default',NULL,'2026-10-25T08:00:00.000Z','2026-10-25T09:00:00.000Z',NULL,NULL,
  '{"settledOverride":"settled","settledAt":"2026-10-25T09:00:00.000Z"}',NULL),
 ('t-settled-early','p-chief','Settled before the latest run','codex','full','default',NULL,'2026-10-25T08:00:00.000Z','2026-10-25T10:00:00.000Z',NULL,NULL,
  '{"settledOverride":"settled","settledAt":"2026-10-25T08:30:00.000Z"}',NULL),
 ('t-settled-norun','p-chief','Settled without runs','codex','full','default',NULL,'2026-10-25T08:00:00.000Z','2026-10-25T08:00:00.000Z',NULL,NULL,
  '{"settledOverride":"settled","settledAt":"2026-10-25T08:30:00.000Z","pullRequests":[]}',NULL),
 ('t-watch','p-ghx','PR watch','codex','full','default',NULL,'2026-10-26T07:00:00.000Z','2026-10-26T07:10:00.000Z',NULL,NULL,
  '{"pullRequests":[{"repository":"hemsoft-dev/gh-x","number":20,"snapshot":{"state":"merged"},"watch":{"armed":true}}],"branchPullRequest":{"repository":"hemsoft-dev/gh-x","number":21,"watch":""}}',NULL),
 ('t-live/root/agent-1','p-ghx','/root/ reviewer','codex','full','default',NULL,'2026-10-26T10:05:00.000Z','2026-10-26T10:06:00.000Z',NULL,NULL,
  '{"lineage":{"rootThreadId":"t-live","parentThreadId":"t-live","relationshipToParent":"subagent"},"modelSelection":{"model":"gpt-6.1-sol"}}',NULL),
 ('t-no-project','p-gone','Project removed','codex','full','default',NULL,'2026-10-26T06:00:00.000Z','2026-10-26T06:00:00.000Z',NULL,NULL,
  '{"lineage":null,"modelSelection":null}',NULL),
 ('t-archived','p-ghx','Archived','codex','full','default',NULL,'2026-10-20T06:00:00.000Z','2026-10-20T06:00:00.000Z','2026-10-21T06:00:00.000Z',NULL,'{}',NULL),
 ('t-deleted','p-ghx','Deleted','codex','full','default',NULL,'2026-10-20T06:00:00.000Z','2026-10-20T06:00:00.000Z',NULL,'2026-10-21T06:00:00.000Z','{}',NULL);

INSERT INTO orchestration_v2_projection_runs VALUES
 ('r-live-0','t-live',0,'codex',NULL,'completed','2026-10-26T10:00:00.000Z','2026-10-26T10:30:00.000Z','{}',NULL),
 ('r-live-1','t-live',1,'codex',NULL,'running','2026-10-26T11:59:00.000Z',NULL,'{}',NULL),
 ('r-qc-0','t-queued-completed',0,'codex',NULL,'completed','2026-10-26T09:00:00.000Z','2026-10-26T09:10:00.000Z','{}',NULL),
 ('r-qc-1','t-queued-completed',1,'codex',NULL,'queued','2026-10-26T11:30:00.000Z',NULL,'{}',NULL),
 ('r-qf-0','t-queued-failed',0,'claudeAgent',NULL,'failed','2026-10-26T09:00:00.000Z','2026-10-26T09:05:00.000Z','{}',NULL),
 ('r-qf-1','t-queued-failed',1,'claudeAgent',NULL,'queued','2026-10-26T11:31:00.000Z','','{}',NULL),
 ('r-sq-0','t-stale-queued',0,'claudeAgent',NULL,'queued','2026-10-26T08:00:00.000Z',NULL,'{}',NULL),
 ('r-sq-1','t-stale-queued',1,'claudeAgent',NULL,'completed','2026-10-26T08:10:00.000Z','2026-10-26T08:30:00.000Z','{}',NULL),
 ('r-set-0','t-settled',0,'codex',NULL,'completed','2026-10-25T08:00:00.000Z','2026-10-25T09:00:00.000Z','{}',NULL),
 ('r-se-0','t-settled-early',0,'codex',NULL,'completed','2026-10-25T08:00:00.000Z','2026-10-25T08:20:00.000Z','{}',NULL),
 ('r-se-1','t-settled-early',1,'codex',NULL,'interrupted','2026-10-25T09:00:00.000Z','2026-10-25T10:00:00.000Z','{}',NULL),
 ('r-w-0','t-watch',0,'codex',NULL,'completed','2026-10-26T07:00:00.000Z','2026-10-26T07:10:00.000Z','{}',NULL),
 ('r-sub-0','t-live/root/agent-1',0,'codex',NULL,'starting','2026-10-26T10:05:00.000Z',NULL,'{}',NULL),
 ('r-arch-0','t-archived',0,'codex',NULL,'running','2026-10-20T06:00:00.000Z',NULL,'{}',NULL);

-- PRs from the table: one duplicate of a payload PR (watch false there, so
-- kept), one new, one without a snapshot, one on an archived thread.
INSERT INTO projection_thread_pull_requests VALUES
 ('t-live','github.com','hemsoft-dev/gh-x',12,'https://github.com/hemsoft-dev/gh-x/pull/12','branch','2026-10-26T11:00:00.000Z','{"state":"open"}',NULL),
 ('t-live','github.com','hemsoft-dev/gh-x',13,'https://github.com/hemsoft-dev/gh-x/pull/13','branch','2026-10-26T11:00:00.000Z',NULL,NULL),
 ('t-watch','github.com','hemsoft-dev/gh-x',22,'https://github.com/hemsoft-dev/gh-x/pull/22','linked','2026-10-26T07:05:00.000Z','{"state":"closed","title":"x"}',NULL),
 ('t-archived','github.com','hemsoft-dev/gh-x',30,'https://github.com/hemsoft-dev/gh-x/pull/30','linked','2026-10-20T06:00:00.000Z','{"state":"open"}',NULL);

-- Pending requests: on a running run (kept), on a completed run with a
-- twice-encoded ordinal (dropped), on a cancelled-free unmatched ordinal
-- (kept), without an ordinal (kept), resolved (excluded), on an archived
-- thread (excluded).
INSERT INTO orchestration_v2_projection_runtime_requests VALUES
 ('rq-1','t-live','n1',NULL,'user_input','pending','2026-10-26T11:59:30.000Z',NULL,
  '{"id":"request:thread%3At-live%3Aordinal%3A1%3Aq1","questions":[{"text":"secret prompt text never sent"}]}'),
 ('rq-2','t-live','n2',NULL,'user_input','pending','2026-10-26T10:20:00.000Z',NULL,
  '{"id":"request:thread%253At-live%253Aordinal%253A0%253Aq0"}'),
 ('rq-3','t-queued-completed','n3',NULL,'approval','pending','2026-10-26T11:31:00.000Z',NULL,
  '{"id":"request:ordinal%25253A7"}'),
 ('rq-4','t-watch','n4',NULL,'user_input','pending','2026-10-26T07:20:00.000Z',NULL,
  '{"id":"request:no-ordinal"}'),
 ('rq-5','t-live','n5',NULL,'user_input','resolved','2026-10-26T11:00:00.000Z','2026-10-26T11:01:00.000Z',
  '{"id":"request:ordinal%3A1:resolved"}'),
 ('rq-6','t-archived','n6',NULL,'user_input','pending','2026-10-20T06:30:00.000Z',NULL,
  '{"id":"request:ordinal%3A0"}');
