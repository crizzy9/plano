# Train a Plano delay model

Use historical flight snapshots from a source you are allowed to retain for training. Record each feature snapshot before the flight pushes back, then join it to the actual gate-out time and the time your collector learned that result. Keep one observation per stable flight instance ID at a consistent forecast lead time. Plano ships no fitted model or historical training dataset.

Plano's live AeroAPI collector keys forecasts by the verified provider flight ID. A later result reached through a schedule alias updates the same outcome. Exports omit older schedule aliases that lack capture-time identity. Use a fresh export when comparing physical-flight counts with earlier datasets. [AeroAPI flight ID availability](https://support.flightaware.com/hc/en-us/articles/33161341318935-Why-Is-A-Frequently-Used-Endpoint-Returning-A-Null-Value)

Prepare a JSON Lines file with one object per line. This example shows the format. Its values are illustrative.

```json
{"flightId":"sample-flight-id","observedAt":"2026-11-05T14:00:00Z","scheduledDeparture":"2026-11-05T15:00:00Z","departedAt":"2026-11-05T15:38:00Z","labelObservedAt":"2026-11-05T15:40:00Z","features":{"airlineDelayMinutes":25,"inboundDelayMinutes":30,"airportDelayMinutes":20,"groundStopMinutes":0,"weather":true,"airTrafficControl":false}}
```

`core.ExtractDelayFeatures` gives historical snapshot exporters the same feature derivation used by the server. The four numeric signals measure nonnegative minutes after scheduled departure. Airline delay comes from the published departure estimate. Inbound turnaround uses the linked inbound flight's expected or actual gate arrival plus Plano's 40-minute turnaround assumption. Airport delay is the largest matching advisory delay. Ground stop minutes measure the interval between scheduled departure and a matching stop's end time. Weather and ATC flags reflect recognized language in applicable advisories. Use only records that were available at `observedAt`, including the schedule as known at that time. The target is nonnegative minutes from that schedule to gate-out. Cancelled flights and records without gate-out times have no label for this model. Use `labelObservedAt` for the first provider observation that includes gate-out. Older inputs can omit it, in which case the trainer assumes the outcome was available at `departedAt`. This assumption can overstate what the model could have learned in real time.

Training accepts 32 to 100,000 unique examples, with lead times from 25 hours before to two hours after scheduled departure and labels up to 24 hours late. A split may require more examples because earlier flights whose outcomes became available at or after the holdout boundary are purged. The newest fifth of forecasts form the default holdout and never enter the fit. Forecasts with equal observation timestamps stay on the same side. The trainer needs at least 24 usable training examples and eight holdout examples.

```sh
go run ./cmd/plano-train -input examples.jsonl -output delay-model.json
```

The command reports learned coverage, mean absolute error, root mean square error, and signed bias on the later flights. `learnedOrFallback` uses the model within its observed feature ranges and the operational baseline elsewhere. `publishedEstimateBaseline` uses the airline estimate when recorded and scheduled departure otherwise. By default the trainer writes an artifact only when learned coverage is at least 50% and mean absolute error beats both baselines on that holdout. `-allow-regression` writes an artifact for offline experiments even if the comparison fails. The minimum sample counts exercise the trainer. Validate operational accuracy with representative traffic and forecast lead times before deployment.

Set `PLANO_DELAY_MODEL_FILE` to the artifact path on the server. A learned result exposes its historical baseline, signed feature contributions, and the model ID alongside the published schedule and estimates. Feature contributions explain the model's estimate. When the latest flight update still shows the flight awaiting gate-out, the forecast cannot precede that observation. A separately verified airport closure can move the forecast later. Provider advisories and inbound timing retain their own evidence. Unsupported values, missing observations, and absent model files leave the operational forecast available.

## Evaluate a deployed artifact

Use separately collected later forecasts with observed gate-out outcomes. Keep them separate from examples used to fit the model or choose its parameters. A training holdout can qualify by date, so passing the cutoff alone does not establish an independent validation set.

```sh
go run ./cmd/plano-train -evaluate-model delay-model.json -input later-examples.jsonl
```

The command writes a JSON report to stdout and leaves the model file intact. It needs at least eight flights with `observedAt` strictly after the artifact's last fitted label. The report counts earlier observations, including those exactly at the cutoff, in `excludedThroughTraining`. `holdoutFrom` and `holdoutRows` describe the evaluated cohort. Compare `learnedOrFallback` with both baselines and check `learnedCoverage` to see how often the model supplied the estimate. The operational and learned scores use `observedAt` as the predeparture status time and respect the elapsed delay. The published baseline retains the original airline estimate. The JSONL features support scoring the forecast before the separate live NOTAM closure adjustment. Evaluation reports the metrics even when the model underperforms. Assess live accuracy using representative traffic and forecast lead times.
