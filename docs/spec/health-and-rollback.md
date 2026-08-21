# Deployment Health and Rollback Contract

Status: planning draft

## 1. Deployment health policy

Deployment-time readiness is distinct from continuous monitoring. A target may
configure:

```yaml
health:
  startupGrace: 3m
  interval: 5s
  timeout: 2s
  successThreshold: 3
  failureThreshold: 3
  soakDuration: 2m
  onFailure: keep # rollback | keep | none
  maxAutomaticRollbacks: 1
  rollbackWindow: 30m
  activationWindow: null
  maxPlayersForRestart: null
  checks:
    - type: tcp
    - type: fivem-http
      endpoints: [/info.json, /dynamic.json]
    - type: rcon-status
    - type: fxvcs-resource-state
```

Checks are typed and bounded. TCP proves only a listener; FiveM HTTP and RCON
prove their respective interfaces. The optional FXVCS-shipped `fxvcs-health`
resource is ordered last and exposes an authenticated loopback-only report of
`GetResourceState` for every enabled resource. Its generated token uses a server
`set` convar, never `setr`, so it is not replicated to clients. Other trusted
server-side resources can still read it; the token protects against unrelated
local processes, not malicious code already inside the server trust boundary.
It is not the sole health signal and does not prove gameplay correctness.
The host-local `AgentConfig.runtime.resourceState` binding supplies an exact
numeric loopback host/port plus a protected token reference. FXVCS generates
the helper into the immutable realization, ensures it last, sends a Bearer-
authenticated request to `/fxvcs-health/state`, strictly bounds and decodes the
report, and persists only boolean/interface evidence—not resource names,
states, response bodies, or token material. Missing bindings, non-loopback
addresses, malformed/oversized reports, redirects, and non-started expected
resources fail closed.

A realization becomes last-known-good only after readiness thresholds and the
soak duration pass. `keep` leaves the newly activated but unhealthy realization
available for operator diagnosis without another restart. `none` reports the
failure without performing an automatic recovery action. `rollback` creates a
new realization from the previous Release using current authorized configuration
and secret policy.

Player-count and activation-window gates are portable optional policy in v1;
when configured, the agent waits rather than forcibly activating outside the
gate. Cloud may add scheduling/approval UX without changing agent semantics.

### 1.1 Portable activation-window expression

`activationWindow` is either `null` or the exact versioned form:

```text
window.v1;tz=<IANA timezone>;days=<*|Mon,Tue,...>;start=HH:MM;end=HH:MM
```

For example, weekday activation from 04:00 through 04:59 in Chicago is:

```text
window.v1;tz=America/Chicago;days=Mon,Tue,Wed,Thu,Fri;start=04:00;end=05:00
```

The timezone is explicit; host-local time is never implied. Named days use the
canonical `Mon,Tue,Wed,Thu,Fri,Sat,Sun` order, or `*` for every day. `start` is
inclusive and `end` is exclusive. When `start` is later than `end`, the window
crosses midnight and the named day is the day on which it starts. Equal times
are invalid rather than ambiguously meaning zero or twenty-four hours.

Evaluation compares local wall-clock minutes. A minute skipped by a daylight
saving transition is therefore never open; a repeated minute is open in both
occurrences. Unknown versions, timezones, fields, day aliases, duplicate or
non-canonical days, and invalid/equal clock values fail closed during document
validation and final locked planning.

## 2. Deployment phases and retry suppression

Deployment phase and retry suppression are separate. The observed phase is one
of:

- `active-healthy`: active realization passed thresholds and soak.
- `active-unhealthy`: the unhealthy desired realization remains active under
  `keep`; health evaluation continues so late recovery can reach
  `active-healthy`.
- `rolled-back`: a prior realization is active after automatic rollback and
  normally reports `desired != active`.
- `activation-failed`: staging or activation failed before the desired
  realization became active.

A failure does not change Git or Cloud desired state. To prevent a reconcile
loop, the agent records the failed realization-input tuple:

```text
release ID
environment revision
target content digest
hook/template digests
safe secret version identifiers
```

`retryHeld: true` applies to the failed tuple after `keep`, rollback, or a
pre-activation failure. The agent does not retry it until desired state changes
or an authorized operator invokes `fxvcs agent retry <target>` (or the equivalent
Cloud action), which records an audit event. Health continues while
`active-unhealthy`; if that exact tuple later reaches healthy thresholds and
soak, the phase becomes `active-healthy` and `retryHeld` clears automatically.
No other health fluctuation clears it.

Automatic rollback is capped per target and time window. If the rollback
realization also fails, the agent stops, preserves the safest active state it can
identify, and requires operator intervention; it never oscillates. Agent-local
safety floors can reduce or disable automatic rollback and cannot be relaxed by
Git or Cloud policy.

## 3. Restart and activation races

Health and activation correlate to the observed process generation. A restart
outside the chosen lifecycle adapter during staging, activation, startup grace,
or soak invalidates that attempt. The agent aborts or retries under bounded
policy and reports the competing supervisor event. Scheduled restarts should be
represented through `activationWindow` or supervisor integration when proven.
