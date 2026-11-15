# CLI workflow claims

- **Workflow-owned completion.** A successful nonempty native drain closes its
  input members as part of execution. That is not stale-work cancellation:
  claim-time input retirement must preserve the workflow's downstream steps.
  Match completion evidence to the same workflow and input convoy; unknown
  evidence must not authorize retirement. Purged inputs still fail explicitly.
  `TestClassRoutedHookPreservesPostDrainWork` covers this boundary through the
  class-routed claim path and a real graph store.
