# txAdmin / FXServer Compatibility Matrix

- Matrix version: **0.1** (research draft)
- Status: **unverified — requires live-lab validation** before any txAdmin
  lifecycle behaviour is enabled ([Stage 4 plan](../stage-4-plan.md),
  [spec/runtime-layout.md](runtime-layout.md) §3).
- txAdmin range investigated: **v8.0.0 – v8.1.1** (releases 2025-03-06 →
  2026-06-05); legacy v7.0.0 noted only where it is bundled with an FXServer
  channel.
- FXServer build range investigated: **25770 (recommended, bundles txAdmin
  8.0.1) – 34833 (latest, bundles txAdmin 8.1.1)**; `critical/optional` 7290
  (txAdmin 7.0.0) is out of scope. Source: Cfx changelog API
  `https://changelogs-live.fivem.net/api/changelog/versions/linux/server`
  (fetched 2026-08-15).
- Research date: 2026-08-15. Repository note: `github.com/tabarra/txAdmin`
  now redirects to `github.com/citizenfx/txAdmin`; all links below use the
  citizenfx URLs.
- RCON source verification: citizenfx/fivem commit
  `f26e3aacb0f6fe8a8c76227a79688b0853dadb45` on 2026-08-21 confirms the UDP
  out-of-band request `0xffffffff + "rcon <password> <command>"`, response
  `0xffffffff + "print <console output>"`, and fixed authentication failures.
  This proves protocol framing, not txAdmin coexistence; E4 remains `lab`.

Verification vocabulary used in every table:

| Status | Meaning |
|---|---|
| `doc` | Stated in official documentation |
| `source` | Read in txAdmin/FXServer source at `master` on the research date |
| `secondary` | Third-party guide, forum, or inference from the above |
| `lab` | Proven in the FXVCS lab (none yet — this is a draft) |

Nothing in this document is `lab` yet. Stage 4 must not enable a txAdmin
mechanism whose row is not `lab`.

## 1. Directive ownership

Owner vocabulary:

- **txAdmin** — set or enforced by txAdmin at spawn or by its cfg validator;
  FXVCS must not write it.
- **operator bootstrap `server.cfg`** — hand-maintained stable file that ends
  with `exec fxvcs.cfg`; FXVCS reads it for `--from-cfg` seeding only.
- **fxvcs.cfg generated** — written by FXVCS into the inactive realization
  from the immutable activation plan and typed inputs
  ([Stage 4 plan](../stage-4-plan.md)).
- **FXVCS-forbidden** — FXVCS never emits it; if found in a template it is a
  validation error.

| Directive / concern | Owner | Notes | Evidence | Status |
|---|---|---|---|---|
| `exec fxvcs.cfg` | operator bootstrap | Only FXVCS line in bootstrap; `exec` paths are relative to server data or `@resource/` | [server-commands](https://docs.fivem.net/docs/server-manual/server-commands/) | doc |
| `ensure <resource>` for enabled release resources | fxvcs.cfg generated | Deterministic order from the release activation plan; disabled resources omitted | [Stage 4 plan](../stage-4-plan.md); [resource-activation](resource-activation.md) | spec |
| `start`/`stop`/`ensure` `monitor`, `txadmin`, `txadminclient` | FXVCS-forbidden | txAdmin's validator comments these out ("you MUST NOT start/stop/ensure txadmin resources"); FXServer itself runs `start monitor` when convar `txAdminServerMode` exists | [fxsConfigHelper.ts](https://github.com/citizenfx/txAdmin/blob/master/core/lib/fxserver/fxsConfigHelper.ts); [ServerInstance.cpp](https://github.com/citizenfx/fivem/blob/master/code/components/citizen-server-main/src/ServerInstance.cpp) | source |
| `runtime:system/monitor` in RuntimeResourceCatalog | coordination (record) / txAdmin (start) | Accepted only when staging discovers `citizen/system_resources/monitor`; FXVCS never `ensure`s it under txAdmin | [resource-activation §4](resource-activation.md); [docs.fivem.net txAdmin](https://docs.fivem.net/docs/resources/txAdmin/) | doc |
| `onesync on|legacy|off` | txAdmin (txadmin supervisor) / operator bootstrap (systemd, manual) | txAdmin injects `+set onesync <setting>` and its validator comments out any cfg `onesync` line ("MUST only be set in the txAdmin settings page") | [FxRunner/utils.ts](https://github.com/citizenfx/txAdmin/blob/master/core/modules/FxRunner/utils.ts); fxsConfigHelper.ts | source |
| `sv_maxclients` | operator bootstrap | txAdmin validates (must be ≤ `TXHOST_MAX_SLOTS`, ≥32 needs OneSync legacy/on, >64 needs `on`) but does not inject | fxsConfigHelper.ts; server-commands doc | source/doc |
| `endpoint_add_tcp` / `endpoint_add_udp` | operator bootstrap | txAdmin validates: valid `ip:port`, interface available, port not in 40120–40150 ("dedicated for txAdmin"), not txAdmin's port, no duplicates, must match `TXHOST_FXS_PORT` when set, missing endpoints is fatal | fxsConfigHelper.ts; [env-config.md](https://github.com/citizenfx/txAdmin/blob/master/docs/env-config.md) | source |
| `sv_licenseKey` | operator bootstrap (or secret-bearing fxvcs.cfg fragment via typed secret input) | Not validated by txAdmin; recipe writes `sv_licenseKey "{{svLicense}}"`. If FXVCS renders it, it must be a `secret` InputSource through `cfgQuote` into the protected fragment outside `resources/` | [default-fivem/server.cfg](https://github.com/citizenfx/txAdmin-recipes/blob/main/default-fivem/server.cfg); [Stage 4 plan](../stage-4-plan.md) | source |
| `rcon_password` | operator bootstrap or secret fragment | Not touched by txAdmin; RCON disabled if unset | server-commands doc; fxsConfigHelper.ts (no check) | doc/source |
| `sv_hostname`, `sv_projectName`, `sv_projectDesc`, `sets tags`, `sets locale` | operator bootstrap or fxvcs.cfg generated from `target`/`environment` inputs | Non-secret; deterministic | recipe server.cfg | source |
| `sv_enforceGameBuild` | operator bootstrap | Startup-only convar; changing it requires full restart — FXVCS classifies as full-restart change | server-commands doc | doc |
| `sv_scriptHookAllowed`, `sv_pureLevel`, `sv_endpointPrivacy`, `steam_webApiKey`, `load_server_icon`, `sv_lan`, `sv_master1` | operator bootstrap | Global; changes require full restart | server-commands doc | doc |
| `sets sv_appearAllowlisted`, `sets sv_allowlistInstructions` | txAdmin | Validator comments cfg lines out ("overwritten by the txAdmin settings page") | fxsConfigHelper.ts | source |
| `+set` prefixed lines inside cfg | FXVCS-forbidden | Validator error ("remove the '+'") | fxsConfigHelper.ts | source |
| `set txAdmin-*`, `setr txAdmin-menuEnabled`, `set txAdmin-luaComHost/luaComToken`, `set txAdminServerMode`, `sets txAdmin-version` | txAdmin | Injected as spawn args; FXVCS never emits `txAdmin-*` | FxRunner/utils.ts | source |
| `set`/`setr`/`sets` semantics | n/a | `set` server-only, `sets` server-info (listed), `setr` replicated to clients. FXVCS health token uses `set` only | server-commands doc; [health-and-rollback §1](health-and-rollback.md) | doc |
| `gamename` | operator bootstrap | Validator errors on duplicate/mismatch | fxsConfigHelper.ts | source |
| `add_ace`, `add_principal`, `{{addPrincipalsMaster}}` | operator bootstrap | txAdmin master principal is recipe-inserted; FXVCS-generated ACE lines are out of scope for v1 | recipe server.cfg | source |
| `resources_useSystemChat` | operator bootstrap | Recipe default | recipe server.cfg | source |
| Validator side effects on `server.cfg` | txAdmin | On every spawn txAdmin runs `validateFixServerConfig`; offending lines are commented out with `## [txAdmin CFG validator]: <reason>` after a `.bkp` copy; fatal errors prevent start | [FxRunner/index.ts](https://github.com/citizenfx/txAdmin/blob/master/core/modules/FxRunner/index.ts); fxsConfigHelper.ts | source |
| Whether the validator follows `exec fxvcs.cfg` into the generated file | **unknown** | Must be tested: if the validator recurses into `exec`ed files it may rewrite `fxvcs.cfg` in the active realization (read-only) and fail; if it does not, forbidden directives in fxvcs.cfg would slip past it | lab plan E1 | open |

## 2. Process ownership per supervisor

| Supervisor | Who starts/stops/restarts | How FXVCS observes process generation | Usable lifecycle mechanisms | Status |
|---|---|---|---|---|
| `txadmin` | txAdmin spawns FXServer as a child (`spawn(bin, [...args, '+exec', cfgPath])`, cwd = Server Data Path). Restart = `killServer()` (event `serverShuttingDown`, wait `shutdownNoticeDelayMs` default 5000 ms, destroy) then respawn (`restartSpawnDelayMs` 500 ms; bind-error backoff 5→45 s). Scheduler restarts at configured HH:MM with warnings at 30/15/10/5/4/3/2/1 min. Host shutdown writes `quit` to stdin. | Child PID/start time of FXServer; `/info.json` `server` string and `/dynamic.json`; txAdmin `configChanged`/`scheduledRestart` events are visible only inside the server. FXVCS records PID + start time before activation and compares afterwards. | (a) **stdin console via txAdmin**: `POST /fxserver/commands` `{action: restart_res|start_res|ensure_res|stop_res|refresh_res, parameter}` (perm `commands.resources`) and `POST /fxserver/controls` `{action: start|stop|restart}` (perm `control.server`) — **auth is a browser session cookie plus `x-txadmin-csrftoken`; there is no public API key**; (b) live console WebSocket (perm `console.write`); (c) **RCON** on FXServer directly if `rcon_password` set (independent of txAdmin); (d) `POST /fxserver/schedule` (temporary schedule / skip). systemd restart of the txAdmin unit is *not* an FXServer restart mechanism. | source (auth model), doc (RCON); **none lab** |
| `systemd` | Declared unit owns FXServer (`run.sh +exec server.cfg`); restart via `systemctl restart <unit>` by the typed systemd adapter. | `systemctl show -p MainPID,ExecMainStartTimestamp`; `/info.json`. | Full restart via systemd; resource-level `refresh`/`ensure`/`stop`/`restart` via RCON if enabled. | doc (vanilla setup) |
| `manual` | Operator. | `/info.json` + PID if discoverable. | RCON resource commands only; no full-restart claim. | spec |

Notes for the `txadmin` row:

- txAdmin communicates with FXServer over **stdin** (`proc.stdin.write(cmd+'\n')`) and delivers events as the restricted console command `txaEvent <name> <json>` handled by `resource/sv_main.lua`. FXVCS must not register `txaEvent` or trigger `txAdmin:events:*` itself ([events.md](https://github.com/citizenfx/txAdmin/blob/master/docs/events.md)).
- Because the only txAdmin HTTP surface is session/CSRF-authenticated and undocumented as a public API ([router.ts](https://github.com/citizenfx/txAdmin/blob/master/core/modules/WebServer/router.ts), [authMws.ts](https://github.com/citizenfx/txAdmin/blob/master/core/modules/WebServer/middlewares/authMws.ts); dev-notes lists "txAdmin API/integrations" as future), the [Stage 4 plan](../stage-4-plan.md) defers a txAdmin-specific lifecycle adapter until its integration contract and authentication behavior are validated. The candidate v1 path under `txadmin` supervision is **RCON for resource-level actions + txAdmin-owned full restarts (scheduler or operator) represented via `activationWindow`**; anything else needs a `lab` row.
- `TXHOST_API_TOKEN` authenticates only `GET /host/status` (`x-txadmin-envtoken`), a read-only status endpoint suitable for observed evidence, not control.

## 3. Multiple instances on one host

| Concern | Finding | Evidence | Status |
|---|---|---|---|
| txAdmin panel port | Default 40120; set with `TXHOST_TXA_PORT` (cannot be 30120). The convars `txAdminPort`, `txAdminInterface`, `txDataPath` and `txAdminZapConfig.json` are **deprecated and will stop working** | env-config.md; [getNativeVars.ts](https://github.com/citizenfx/txAdmin/blob/master/core/boot/getNativeVars.ts) | source |
| FXServer game port | Operator's `endpoint_add_*`; may be forced with `TXHOST_FXS_PORT` (cannot be 40120); ports 40120–40150 reserved by validator | env-config.md; fxsConfigHelper.ts | source |
| `txData` separation | `TXHOST_DATA_PATH` per instance; official guidance: "we strongly encourage you to set up separate txDatas for your servers. As otherwise the `admins.json` file will conflict." Default Linux path `<fxserver_root>/../../../txData` | env-config.md | source |
| Profiles (`+set serverProfile`) | Profile = `<txData>/<profile>/{config.json,data/,logs/}`; convar "should be avoided; profile concept faces likely deprecation". FXVCS treats **one txData per instance** as the supported layout and profiles as legacy | [setup.ts](https://github.com/citizenfx/txAdmin/blob/master/core/boot/setup.ts); env-config.md | source |
| Server Data Path / CFG File Path | txAdmin settings `dataPath` (fatal if invalid) and `cfgPath` (default `server.cfg`, relative to dataPath) — these are what FXVCS's `<serverData>` and bootstrap `server.cfg` must match | [ConfigStore schema/server.ts](https://github.com/citizenfx/txAdmin/blob/master/core/modules/ConfigStore/schema/server.ts) | source |
| FXVCS layout mapping | `<serverData>` = txAdmin Server Data Path; `resources` and `fxvcs.cfg` are indirections into `<deployRoot>/current`; `<txData>` outside; two targets need distinct `<deployRoot>` and distinct txData/ports | [runtime-layout §1](runtime-layout.md) | spec |
| Does FXServer/txAdmin tolerate `resources` being a symlink? | Unknown for txAdmin's resource scanning and the validator; FXServer `refresh` rescans `resources` | lab plan E3 | open |

## 4. Open questions and lab plan (Stage 4 gate)

Each experiment runs on Linux with the `recommended` and `latest` FXServer
builds above and their bundled txAdmin, using a throwaway license key and the
CFX default recipe. Pass criteria are binary; a failed experiment moves the
row to "unsupported" for that version, it never becomes a warning.

| ID | Question | Experiment | Pass criteria |
|---|---|---|---|
| E1 | Does the cfg validator recurse into `exec fxvcs.cfg` and rewrite it? | Put a forbidden line (`ensure monitor`) only inside `fxvcs.cfg` (read-only file in a realization); start via txAdmin | Either (a) validator ignores exec'ed files and FXServer logs the forbidden line at runtime, or (b) validator refuses. Record which; FXVCS's own generator must reject forbidden directives regardless. `server.cfg` bootstrap must be byte-identical after start (no `.bkp`, no `## [txAdmin CFG validator]` lines) |
| E2 | Does txAdmin edit `server.cfg` when it is valid? | Start 3× with a valid bootstrap; hash before/after | Hash unchanged; no `.bkp` created |
| E3 | Symlinked `resources` and `fxvcs.cfg` under `<serverData>` | `resources -> <deployRoot>/current/resources`, `fxvcs.cfg -> .../runtime/fxvcs.cfg`; start; run `refresh`; swap `current`; run `refresh` + `ensure` new resource | All enabled resources start; after pointer swap `refresh` sees added resources; no path errors in txAdmin console; txAdmin's resource list (`commands.resources`) matches |
| E4 | RCON alongside txAdmin | `rcon_password` set in bootstrap; issue `refresh`, `ensure x`, `stop x`, `restart x`, `status` over RCON while txAdmin supervises | Commands execute; txAdmin does not comment out `rcon_password`; results visible in txAdmin live console |
| E5 | Process-generation detection across txAdmin restart | Record child PID/start time; trigger `POST /fxserver/controls restart` and a scheduled restart; observe | FXVCS detects a new generation within one poll interval; an activation attempt overlapping the restart is marked invalid, never successful |
| E6 | Full restart during activation window | Configure scheduler HH:MM; set `activationWindow` to exclude it; deploy | Agent waits; no overlap; `serverShuttingDown` event lead time ≥ `shutdownNoticeDelayMs` |
| E7 | txAdmin HTTP control API usability | Attempt `POST /fxserver/commands` with (a) no auth, (b) `TXHOST_API_TOKEN`, (c) session+CSRF | Expected: (a) 401/403, (b) rejected, (c) accepted — confirming no headless credential exists. Outcome decides whether a txAdmin adapter is feasible without browser-session automation (expected: **not feasible in v1**) |
| E8 | `/info.json`, `/dynamic.json` fields for observed runtime evidence | Fetch on both builds under txAdmin | `server` = `FXServer-<git description>`; `vars` includes `txAdmin-version` (from `sets`); `sv_maxclients` present in dynamic.json; record exact strings for `observedRuntime.fxserverBuild/txadminVersion` parsers |
| E9 | System resources inventory | List `citizen/system_resources/` in both artifacts | `monitor` present; inventory digest reproducible for `systemResourcesDigest` |
| E10 | Two txAdmin instances on one host | Two `TXHOST_DATA_PATH`, two `TXHOST_TXA_PORT`, two `endpoint_add_*`, two `<deployRoot>` | Both start; FXVCS rejects duplicate roots; each agent target reconciles independently; validator does not flag the other instance's ports |
| E11 | OneSync ownership under txAdmin | Put `onesync legacy` in bootstrap while txAdmin setting is `on` | Validator comments out cfg line; effective value is txAdmin's — FXVCS documents that OneSync is not a release/target input under `txadmin` supervision |
| E12 | Windows developer parity (not agent) | n/a in Stage 4 (agent is Linux-only) | — |
| E13 | Deprecated convars | Start with `+set txDataPath` on latest | Warning logged; still works or not — record for migration docs |
| E14 | Resource-level hot actions safety | `restart <res>` for a resource with `persistentPaths`, and one with `mutableMode: copy` | State preserved; no file-lock errors on Linux |

Additional open questions to resolve during the lab:

- Exact FXServer build at which `TXHOST_*` env vars became authoritative
  (needed for the "txAdmin version range" floor of any adapter).
- Whether `refresh` is required before `ensure` of a resource added by a
  pointer swap (expected yes; drives the RCON adapter's `refresh; ensure`
  ordering).
- Whether txAdmin's `resourceStartingTolerance` (90 s) and `bootCooldown`
  (45 s) interact with FXVCS `startupGrace`; recommend `startupGrace ≥
  bootCooldown`.
- Whether `/players.json` anonymization affects the `maxPlayersForRestart`
  gate (expected: `clients` from `/dynamic.json` suffices).

## 5. Reference URLs

- txAdmin README: https://github.com/citizenfx/txAdmin/blob/master/README.md
- txAdmin releases: https://github.com/citizenfx/txAdmin/releases
- Env/host config (TXHOST_*): https://github.com/citizenfx/txAdmin/blob/master/docs/env-config.md
- Events: https://github.com/citizenfx/txAdmin/blob/master/docs/events.md
- Permissions: https://github.com/citizenfx/txAdmin/blob/master/docs/permissions.md
- Recipes: https://github.com/citizenfx/txAdmin/blob/master/docs/recipe.md ; default recipe https://github.com/citizenfx/txAdmin-recipes/blob/main/default-fivem/
- Spawn/args: https://github.com/citizenfx/txAdmin/blob/master/core/modules/FxRunner/utils.ts ; lifecycle https://github.com/citizenfx/txAdmin/blob/master/core/modules/FxRunner/index.ts
- cfg validator: https://github.com/citizenfx/txAdmin/blob/master/core/lib/fxserver/fxsConfigHelper.ts
- Scheduler: https://github.com/citizenfx/txAdmin/blob/master/core/modules/FxScheduler.ts
- HTTP routes/auth: https://github.com/citizenfx/txAdmin/blob/master/core/modules/WebServer/router.ts , .../middlewares/authMws.ts , https://github.com/citizenfx/txAdmin/blob/master/core/routes/fxserver/controls.ts , .../commands.ts
- Settings schema: https://github.com/citizenfx/txAdmin/blob/master/core/modules/ConfigStore/schema/server.ts , .../restarter.ts
- Profile setup: https://github.com/citizenfx/txAdmin/blob/master/core/boot/setup.ts ; deprecated convars https://github.com/citizenfx/txAdmin/blob/master/core/boot/getNativeVars.ts
- Monitor resource guard: https://github.com/citizenfx/txAdmin/blob/master/resource/sv_main.lua
- FXServer `start monitor` on `txAdminServerMode`: https://github.com/citizenfx/fivem/blob/master/code/components/citizen-server-main/src/ServerInstance.cpp ; launcher mode selection https://github.com/citizenfx/fivem/blob/master/code/server/launcher/src/Server.cpp
- FXServer HTTP endpoints: https://github.com/citizenfx/fivem/blob/master/code/components/citizen-server-impl/src/InfoHttpHandler.cpp
- FXServer resource HTTP-handler contract (request address/headers/method/path and response bounds): https://github.com/citizenfx/fivem/blob/f26e3aacb0f6fe8a8c76227a79688b0853dadb45/ext/native-decls/SetHttpHandler.md
- FXServer RCON framing/authentication: https://github.com/citizenfx/fivem/blob/f26e3aacb0f6fe8a8c76227a79688b0853dadb45/code/components/citizen-server-impl/include/RconOutOfBand.h ; UDP out-of-band dispatch: https://github.com/citizenfx/fivem/blob/f26e3aacb0f6fe8a8c76227a79688b0853dadb45/code/components/citizen-server-impl/include/WithOutOfBand.h
- Server commands/convars: https://docs.fivem.net/docs/server-manual/server-commands/
- Vanilla setup: https://docs.fivem.net/docs/server-manual/setting-up-a-server-vanilla/ ; txAdmin setup: https://docs.fivem.net/docs/server-manual/setting-up-a-server-txadmin/
- txAdmin resource doc (legacy convars): https://docs.fivem.net/docs/resources/txAdmin/
- cfx-server-data resources: https://github.com/citizenfx/cfx-server-data/tree/master/resources
- Artifact channels API: https://changelogs-live.fivem.net/api/changelog/versions/linux/server ; EOL policy https://docs.fivem.net/docs/server-manual/end-of-support-end-of-life/ (secondary)
- RCON with txAdmin (secondary): https://zap-hosting.com/guides/docs/fivem-rcon/

## 6. Change log

- 0.1 (2026-08-15): initial research draft; no lab rows.
