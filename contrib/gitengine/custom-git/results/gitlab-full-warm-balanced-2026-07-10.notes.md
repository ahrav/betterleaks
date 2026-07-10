# GitLab full warm balanced run notes

- This artifact predates Git-environment parity in the benchmark harness.
  Reference workers used `core.deltaBaseCacheLimit=128m`; custom workers
  inherited Git's 96 MiB default. Every custom row is marked
  `profile_confounded_delta_cache_96m_vs_128m`. Counts and digests remain valid;
  timing and helper RSS must not be used as an equal-profile engine comparison.

- JSONL row 1 embeds `quarantined_due_to_overlap`: it is correctness-only and
  timing-quarantined. A gix 100-commit
  comparison (PIDs 91983 and 92141) overlapped the opening reference run.
- Row 1 nevertheless passed the enforced authoritative counts and established
  canonical digest
  `15bf91a9c237467054c9e2f3e11f405109256231e5c7928d13621b2cdb7d6e72`.
- JSONL row 9 embeds `invalid_input_no_scan` because a mistyped revision digest
  aborted preflight. The sidecar duplicates both dispositions as audit history;
  it is no longer needed to correct contradictory raw-row validity.
