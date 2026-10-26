# Measured reliability

Use `EvidenceCalibrationProvider` with `engine.WithOperationalAwareness` when you have a measured cohort for the exact deployed configuration. You supply the evidence repository, immutable cohort registrations and a `CalibrationAuthorizer`. Without a matching registration, reliability stays unknown.

To publish a cohort:

1. Freeze your discovery and holdout item digests before evaluation. Keep each holdout item once, with one independent trial per item.
2. Capture any prediction before its outcome. Record every planned trial, including missing results, grader errors and cancellations, in `CalibrationCohort`.
3. Encode the cohort with `json.Marshal`, seal it with `evidence.Seal`, and save it through your authorized evidence repository. The artifact producer must identify your trusted evaluator.
4. Call `MeasureCalibration(cohort, cohortRef)`. Encode, seal and save the returned `CalibrationMeasurement` as a second artifact from the same producer.
5. Register that profile reference, agent, model and exact `CalibrationBinding` with `NewEvidenceCalibrationProvider`. Give the provider to `engine.OperationalAwarenessConfig.Calibration` and choose your minimum resolved sample count and maximum age.

The provider reads both artifacts, reproduces the statistics and checks the complete immutable references. Canonical JSON is required. You cannot select between overlapping registrations by score: the constructor rejects duplicate runtime routes.

Your `CalibrationAuthorizer` runs before and after hydration. It must check the current run's grants, task class, provider/controller/environment revisions, comparable deadline constraints and current revocation or contamination state. Evidence access alone does not establish those deployment facts. A revoked grant, deleted artifact, changed digest, expired retention period or mismatched runtime configuration prevents a measured observation.

## What the numbers mean

The estimate is confirmed successful trials divided by all planned holdout trials. Missing results, grader errors and cancellations remain in that denominator. The fixed 95% Wilson interval uses the same denominator, so you can see uncertainty without treating an unresolved trial as a confirmed success.

Brier score and ten reliability bins use resolved trials that have a prior prediction. `PredictedTrials` reports this separate denominator. If you have no such predictions, Brier is absent. Empty bins retain a zero count; their zero-valued averages do not represent observations.

The first estimator accepts at most 4,096 distinct holdout items. Repeated samples and correlated trials need a separately versioned estimator with an appropriate sampling unit. Discovery/holdout overlap is rejected, while broader contamination still needs your evaluator's checks. Neither an estimate nor its interval grants permission, replaces required verification or establishes certainty about an individual result.

## Comparing frozen benchmarks

`NewFrozenBenchmark` accepts a host-owned `BenchmarkRoster` with exact plan digests and baseline/candidate job request digests. Freeze the complete roster before reading scores. `Compare` delegates paired statistics to Sentinel's published `comparison.CompareRecordedGroups`; the absolute cohort math above is separate because a paired improvement interval is not an absolute success interval.

You supply `BenchmarkAuthorizer` to approve the exact Cortex scope, Sentinel app/tenant mapping and disclosure before and after reads. The adapter supports up to 32 plan groups, 256 jobs and 8,192 captured trials across both arms. It snapshots validated records before comparison and retains pending jobs, required-criterion failures and unknown cost. A pending evaluation produces an inconclusive report.

The tests exercise real Sentinel evaluation and frozen-plan storage with deterministic fixtures across coding, retrieval, tool workflow, multi-agent, long-context and safety groups. These verify integration behavior. You still need your own corpus, current provider/model runs and independent outcome checks to qualify a deployment; the fixtures do not supply a live performance profile.
