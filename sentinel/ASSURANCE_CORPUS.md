# Fixed assurance reports

Use `NewAssuranceCorpusRunner` to review a complete, preregistered corpus from retained evidence. You get five separate arms: unchecked execution, required verification, reflection enabled, qualified memory enabled and a reviewed team. Each arm contains the same twelve cases. The reflection, memory and team arms change one setting from the verification arm.

The case inputs and acceptance requirements are in `testdata/assurance-corpus-v1.json`. They cover coding, units, sources, external state, independent review, long-context correction, wrong scope, stale evidence, an irrelevant answer, prompt injection, an interrupted effect and a required-service outage. These are public regression cases. Do not use them as an unexposed optimizer adoption holdout.

## Run the corpus in your host

1. Prepare each case and arm through your existing Cortex runtime. Keep model and environment revisions fixed, and bind each of the 60 slots to its prepared digest. Freeze an `AssuranceManifest` before execution. Your `AssuranceAuthorizer` must verify preregistration and the actual arm settings; setting an arm label does not enable a feature.
2. Execute through the normal prepared runtime, scheduler and verifier services. This report package never calls a model or retries an effect. Keep your experiment's finite shared budget, cancellation and recovery policy in those services.
3. Have an independent assessor retain one canonical JSON `AssuranceObservation` per observed slot. Bind it to the runner's `Digest()`, exact prepared digest and a source digest covering the run, verification and accounting records. Outcome comes from independent assessment. `ClaimedSuccess` and `Verified` describe what the system claimed, so you can measure a false success.
4. Seal and store each observation with `evidence.Seal` and your authorized evidence repository. Use the registered producer, exact scope and an explicit retention deadline. `AuthorizeAssuranceObservation` authenticates the source ledger, assessment, usage and current grants before the report uses those values.
5. Call `Report(ctx, refs)` with the explicit slot-to-artifact mappings. An omitted slot stays missing. A supplied reference that is denied, deleted, unavailable or changed fails the read, so an outage cannot silently prune the denominator.

The runner accepts exactly 60 distinct slots, at most 60 observations of 16 KiB each, and four optional comparison rosters. A report has a 30-second context deadline. Callbacks must honor cancellation. Every observation is reauthorized before the final response, including those hydrated earlier in the pass.

You can supply frozen Sentinel comparison rosters for verification versus unchecked execution, and reflection, memory or team versus verification. The runner delegates to `FrozenBenchmark.Compare`, preserving its paired statistics, required failures and unknown-cost rules. Your benchmark authorizer verifies that each exact job roster maps to the declared corpus and arms. Absent comparisons appear in `missing_comparisons`; they never imply no difference.

## Read the denominators

Confirmed and verified success use all twelve planned slots per arm. False-success rate uses resolved cases where the system claimed success; unresolved claims have a separate count. Selective accuracy uses resolved, non-abstained cases. Abstention and recovery report their observed denominators, with field coverage alongside them. You can see which measurements are missing before comparing rates.

Calibration reuses the existing confirmed/planned Wilson interval, Brier score and reliability bins. Prediction must have been captured before its outcome. The public corpus has one trial per distinct case in each arm; correlated repetitions need the separately versioned estimator described in `CALIBRATION.md`.

Latency uses nearest-rank p50 and p95 over observed durations, with the observed count. Known costs remain grouped by currency. Total cost and cost per independently successful verified result stay null unless every slot has known accounting in the same currency. A zero cost needs an explicit measured zero. Include generation, tools, grading, retries and recovery in the source ledger; estimates belong outside this known-cost field.

Security counts are known observations. `zero_observed_violations` requires all five security scenarios to have resolved independent verdicts and every counter present. Any recorded unauthorized operation, secret leak, artifact substitution, duplicate effect or unsafe publication yields `violations_observed`, even outside a designated security case. Unresolved cases keep the status incomplete. None of these statuses grants rollout or activation authority.

## Reproduce the fixture artifact

From `sentinel/`:

```sh
GOWORK=off go test -race -count=1 -run 'TestAssurance|TestFrozenBenchmark|TestCalibration' .
CORTEX_ASSURANCE_ARTIFACT_DIR=/tmp/cortex-assurance-artifacts \
  GOWORK=off go test -count=1 -run '^TestAssuranceFixtureArtifact$' .
```

`testdata/assurance-fixture-report.json` was produced by that fixture test. It contains synthetic authenticated telemetry, one deliberately missing slot and no model calls. Separate tests exercise real Sentinel recorded scoring with complete, pending and failed required checks. The artifact validates accounting behavior. It does not measure model quality, installed authority, live recovery or Linux sandbox isolation.

For an installed measurement, retain the manifest, current runtime pins, authorized source artifacts, complete recorded job rosters and the generated report. Record the environment and any missing checks with it. Keep user-deferred Linux qualification and live-provider evidence separate from deterministic fixture results.
