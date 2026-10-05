# Workspace condition reasons

`GET /v1/workspaces[/{id}]` serves `phase` and `conditions`. The `reason` of
a condition is a CamelCase token; the portal's lifecycle progress maps the
tokens below to steps and wording, and shows an unknown token as a generic
"working on it" step with the token in the details line. Condition
`message` text is a fixed string and never carries a Kubernetes or
operator error.

## Starting (create, start)

Read `RuntimeReady` (and `ConnectionReady`, which carries the same pod
reason) while it is `False`.

| reason | meaning |
|---|---|
| `Provisioning` | the runtime pod does not exist yet, or is not scheduled and reports nothing more |
| `Unschedulable` | no node can run the pod now (capacity, taints, or an unbound volume; the scheduler gives one reason for all) |
| `PreparingPod` | scheduled; the kubelet is still setting up the pod sandbox, network and volumes (`PodReadyToStartContainers` is not yet true) |
| `PullingImage` | the sandbox is ready and the kubelet is pulling an image |
| `ContainerCreating` | as `PreparingPod`/`PullingImage`, from a kubelet that does not report `PodReadyToStartContainers` |
| `PodInitializing` | an init container (the Kasm adapter) is running |
| `ImagePullBackOff`, `ErrImagePull` | an image (including an init container's) cannot be pulled |
| `CrashLoopBackOff`, `CreateContainerConfigError`, `PodFailed` | the container does not start |
| `NotReady` | the container runs; the runtime's readiness check has not passed |
| `BackendError` | the platform could not complete a runtime step (cluster quota, admission, API-server error); it retries with backoff and the boot deadline still applies |
| `BootDeadlineExceeded` | the start did not finish within the template's `bootDeadline`; `phase` is `Failed` |

`StorageReady` is `False/Provisioning` while a retained home volume is not
bound, and `False/WaitingForDisk` for a workspace created from retained data
while the disk is being handed over to it (the claim is named but the volume
is not yet retargeted; transient, retried). `Degraded/RetainedClaimMissing`
is different: the claim is not named at all, and the platform refuses to
start the workspace rather than build an empty disk.

## Failed

Once `phase` is `Failed` the three step conditions are frozen: the reason on
`ConnectionReady` is the step that stalled (for example `ImagePullBackOff`),
and it stays after the five-minute cleanup of the failed incarnation.
`RuntimeReady` reads `BootDeadlineExceeded`. A new start unfreezes them.

## Stopping

`BackendError` on `RuntimeReady` means the runtime could not be stopped yet
(retrying). Otherwise `phase` `Stopping` then `Stopped`.

## Deleting

While a workspace is deleted, `RuntimeReady` is `False` with the reason of the
teardown step in progress, in this order:

`BlockingConnects` → `RevokingLeases` → `DrainingStreams` (up to 45 s;
`Degraded/StreamDraining` explains the wait, `DrainTimedOut` when the budget
ran out) → `StoppingRuntime` → `ApplyingRetention` (Retain keeps the disk,
Ephemeral destroys it; `Degraded/RetentionPending` while it is blocked) →
`CleaningUp` (`Degraded/CleanupRetry` when a step failed and will retry).

## Events

`GET /v1/workspaces/{id}/events` serves curated lifecycle and condition
events. The `reason` tokens are the ones above plus the intent steps
`Created`, `StartRequested`, `StopRequested`, `IdleTimeout`,
`DisconnectTimeout`, `MaxDurationReached`, `DeleteRequested` and
`TemplateUpdateSkipped` (a start whose family re-point the compatibility
guard refused), and one `Failed.<reason>` warning synthesized from
`status.phase = Failed`.

## Message parameters (`params`)

Since v0.4, `WorkspaceEvent` and `WorkspaceCondition` may carry `params`: a
flat string map holding the values the message interpolates (or used to
select it), so a client can localize the full text instead of parsing the
English. `params` is additive and optional — an absent key means the value
was not recorded, and clients must keep treating `message` as the fallback
rendering. Params never carry secrets, tickets or internal hostnames.

| param | produced on | meaning |
|---|---|---|
| `revision` | every intent-derived event (`Created`, `StartRequested`, `StopRequested`, `IdleTimeout`, `DisconnectTimeout`, `MaxDurationReached`, `DeleteRequested`, `TemplateUpdateSkipped`) | the intent revision the event id embeds (`StartRequested.4` → `4`) |
| `cause` | stop events recorded with a platform cause | the broker-recorded stop reason (`idle_timeout`, `disconnect_timeout`, `max_duration`, `requested`) |
| `skipReason` | `TemplateUpdateSkipped` | the guard token the message parenthesizes: `runtime-changed`, `experience-changed`, `data-policy-changed`, `storage-smaller` |
| `condition` | every condition-derived event | the `WorkspaceCondition.type` that produced the event |
| `status` | every condition-derived event | its status (`True`/`False`/`Unknown`) |
| `step` | teardown marks (`BlockingConnects`…`CleaningUp` on `RuntimeReady`; `CleanupRetry`, `RetentionPending`, `StreamDraining`, `DrainTimedOut` on `Degraded`) | the teardown step token: `block-connects`, `revoke-leases`, `drain-streams`, `stop-runtime`, `retention`, `cleanup` |
| `budgetSeconds` | `Degraded/StreamDraining`, `Degraded/DrainTimedOut` | the stream-drain budget in seconds |

Condition params travel on the
`workspaces.cdi.tinyorbit.vn/condition-params` object annotation, keyed
`"<type>.<reason>"`; the API projects only the entry matching each
condition's type and reason, so a stale entry under a different reason
never applies. An operator older than v0.4 writes no annotation — the
conditions then carry no `params` and clients fall back to `message`.
