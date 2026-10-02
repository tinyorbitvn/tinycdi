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
bound.

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
