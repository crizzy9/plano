# Train a Plano delay model

Use historical flight snapshots from a source you are allowed to retain for training. Record each feature snapshot before the flight pushes back, then join it to the actual gate-out time and the time your collector learned that result. Keep one observation per stable flight instance ID at a consistent forecast lead time. Plano ships a separate BTS schedule model. Fitting the live estimate requires your own timestamped observations.

Plano's live AeroAPI collector keys forecasts by the verified provider flight ID. A later result reached through a schedule alias updates the same outcome. Exports omit older schedule aliases that lack capture-time identity. Use a fresh export when comparing physical-flight counts with earlier datasets. [AeroAPI flight ID availability](https://support.flightaware.com/hc/en-us/articles/33161341318935-Why-Is-A-Frequently-Used-Endpoint-Returning-A-Null-Value)

The default `plano export-delay-samples` command exports the standard cohort, collected 30 minutes to two hours before departure. Use `plano export-delay-samples --early` for observations more than two and up to 25 hours ahead. The same flight can appear once in each export with the same hashed identity. Train and evaluate each cohort separately and mount the early fit using `PLANO_EARLY_DELAY_MODEL_FILE`. [Collector and model settings](../../docs/SELF_HOSTING.md#airport-routing-imports-and-delay-models)

Prepare a JSON Lines file with one object per line. This example shows the format. Its values are illustrative.

```json
{"flightId":"sample-flight-id","observedAt":"2026-11-05T14:00:00Z","scheduledDeparture":"2026-11-05T15:00:00Z","departedAt":"2026-11-05T15:38:00Z","labelObservedAt":"2026-11-05T15:40:00Z","features":{"airlineDelayMinutes":25,"inboundDelayMinutes":30,"airportDelayMinutes":20,"groundStopMinutes":0,"weather":true,"airTrafficControl":false}}
```

`core.ExtractDelayFeatures` gives historical snapshot exporters the server's flight and advisory feature derivation. The four numeric signals measure nonnegative minutes after scheduled departure. Airline delay comes from the published departure estimate. Inbound turnaround uses the linked inbound flight's expected or actual gate arrival plus Plano's 40-minute turnaround assumption. Airport delay is the largest matching advisory delay. Ground stop minutes measure the interval between scheduled departure and a matching stop's end time. Weather and ATC flags reflect recognized language in applicable advisories. Use only records that were available at `observedAt`, including the schedule as known at that time. The target is nonnegative minutes from that schedule to gate-out. Cancelled flights and records without gate-out times have no label for this model. Use `labelObservedAt` for the first provider observation that includes gate-out. Older inputs can omit it, in which case the trainer assumes the outcome was available at `departedAt`. This assumption can overstate what the model could have learned in real time.

With `PLANO_TAF=true`, the live collector adds `terminalWeatherForecast: true` when a prevailing adverse NOAA terminal forecast covers the expected departure or arrival. Plano must have accepted the forecast into its cache by the sample's observation time. A false or omitted value means there was no qualifying evidence at that time. Fit each cohort from a consistent capture setup so weather feed coverage is comparable between training and evaluation. [Aviation Weather Center](https://aviationweather.gov/data/api/)

Training accepts 32 to 100,000 unique examples, with lead times from 25 hours before to two hours after scheduled departure and labels up to 24 hours late. A split may require more examples because earlier flights whose outcomes became available at or after the holdout boundary are purged. The newest fifth of forecasts form the default holdout and never enter the fit. Forecasts with equal observation timestamps stay on the same side. The trainer needs at least 24 usable training examples and eight holdout examples.

```sh
go run ./cmd/plano-train -input examples.jsonl -output delay-model.json
```

The command reports learned coverage, mean absolute error, root mean square error, and signed bias on the later flights. `learnedOrFallback` uses the model within its observed feature ranges and the operational baseline elsewhere. `publishedEstimateBaseline` uses the airline estimate when recorded and scheduled departure otherwise. `eligibleRows` counts flights with a positive operational delay, weather or ATC advisory signal, or a terminal forecast signal supported by the fit. `eligibleCoverage` is the share of those rows where the model supplied an estimate. By default the trainer writes an artifact only when at least eight rows are eligible, eligible coverage is at least 50%, and mean absolute error across the full holdout beats both baselines.

When both terminal weather states appear in the training rows, the trainer fits a v2 model with a separate terminal forecast term. It also fits a comparison model on the same rows with that term excluded. The `terminalWeather` report includes error and coverage for the adverse-forecast subset. The deployment gate requires at least eight flights in that subset, at least 50% learned coverage there, and lower mean absolute error than the comparison fit both overall and within that subset. `-allow-regression` writes an artifact for offline experiments even if the comparison fails. Validate operational accuracy with representative traffic and forecast lead times before deployment.

Set `PLANO_DELAY_MODEL_FILE` to the artifact path on the server. A learned result exposes its historical baseline, signed feature contributions, and the model ID alongside the published schedule and estimates. Feature contributions explain the model's estimate. When the latest flight update still shows the flight awaiting gate-out, the forecast cannot precede that observation. A separately verified airport closure can move the forecast later. Provider advisories and inbound timing retain their own evidence. Unsupported values, missing observations, and absent model files leave the operational forecast available.

Keep `PLANO_TAF=true` when loading a v2 model. Earlier v1 artifacts retain their existing feature set. Saved v2 estimates and queued alerts recheck the forecast evidence before use, including whether a revised forecast changed the feature.

## Evaluate a deployed artifact

Use separately collected later forecasts with observed gate-out outcomes. Keep them separate from examples used to fit the model or choose its parameters. A training holdout can qualify by date, so passing the cutoff alone does not establish an independent validation set.

```sh
go run ./cmd/plano-train -evaluate-model delay-model.json -input later-examples.jsonl
```

The command writes a JSON report to stdout and leaves the model file intact. It needs at least eight flights with `observedAt` strictly after the artifact's last fitted label. The report counts earlier observations, including those exactly at the cutoff, in `excludedThroughTraining`. `holdoutFrom` and `holdoutRows` describe the evaluated cohort. Compare `learnedOrFallback` with both baselines and check `learnedCoverage` to see how often the model supplied the estimate. The operational and learned scores use `observedAt` as the predeparture status time and respect the elapsed delay. The published baseline retains the original airline estimate. The JSONL features support scoring the forecast before the separate live NOTAM closure adjustment. Evaluation reports the metrics even when the model underperforms. Assess live accuracy using representative traffic and forecast lead times.

A retrospective at Chicago O'Hare joined archived TAFs to BTS departures
using a 60-minute lead and an assumed five-minute publication lag. In August
2026, 38.79% of the 1,985 departures with adverse prevailing forecasts were at
least 15 minutes late, compared with 30.68% of the 29,921 other covered departures.
Predicting the June mean delay for each weather group gave an August MAE of
35.03 minutes, compared with 34.65 for the pooled June mean and 28.19 for the
scheduled departure. The archive lacks receipt times, and this comparison
covers the weather signal alone. Live v2 evaluation requires timestamped
flight, advisory, and forecast captures with later gate-out outcomes.
[Iowa Environmental Mesonet TAF archive](https://mesonet.agron.iastate.edu/cgi-bin/request/taf.py?help=) · [BTS monthly archives](https://transtats.bts.gov/PREZIP/)

## Fit a historical departure-risk model from BTS

The separate `bts-risk` command uses U.S. reporting carriers’ historical gate departures to estimate the probability of a delay of at least 15 minutes. It reads scheduled origin and local departure time, then learns rates in three-hour windows. It shrinks each window's rate toward the origin airport's overall rate. The sample size counts operated flights with a usable recorded departure delay in that window. Training and evaluation exclude cancellations. The model serves routes present in the training data and requires at least 100 flights in the corresponding window. [BTS Reporting Carrier On-Time Performance](https://www.transtats.bts.gov/TableInfo.asp?QO_fu146_anzr=b0-gvzr&gnoyr_VQ=FGJ) · [BTS 2026 reporting directive](https://www.bts.gov/explore-topics-and-geography/modes/aviation/number-40-technical-directive-reporting-time)

Supply the publication date of the training archive and a separate later archive. The CLI streams CSV files or single-CSV monthly ZIPs. It selects the retrospective holdout by local flight date after publication and within the model's one-year validity period. It writes an artifact when coverage reaches 80% and the Brier score beats both the airport-only and nationwide rates. A lower Brier score means the probabilities were closer to the observed outcomes. Unsupported holdout rows use the airport or nationwide rate for that comparison. Live details omit the historical estimate when the route, time zone, or sample is unavailable. [Monthly archives](https://transtats.bts.gov/PREZIP/)

```sh
go run ./cmd/plano-train bts-risk \
  -training june-2026.zip -published-on 2026-08-12 \
  -holdout august-2026.zip -output historical-departure-risk.json
```

This historical probability appears separately from the expected departure time and live delay explanations. It uses only the scheduled time window at the origin airport. The BTS archives contain completed operations and scheduled times, so evaluating the live delay model still requires timestamped forecast features and gate-out labels.

### Bundled model evaluation

Plano ships `bts-departure-risk-v1-4092eb4a786057c3`, fitted on 597,558 labeled operations across 351 origin airports in June 2026. The June archive was published on August 12. The civil-date holdout includes 367,129 eligible flights from August 13 through 31, of which 342,510 received a model estimate. The table scores all eligible rows, using the airport or nationwide baseline for unsupported rows. This evaluates one summer holdout using the schedules in BTS's completed records.

| Estimate | Brier score |
| --- | ---: |
| Model with fallback | 0.169120 |
| Origin airport only | 0.179919 |
| Nationwide | 0.180122 |

- Training archive SHA-256 `acd9d03e223e079e54b77065015f0ce9b6de92f1f46148048600cc9813a47a6e`. [June 2026 BTS ZIP](https://transtats.bts.gov/PREZIP/On_Time_Reporting_Carrier_On_Time_Performance_1987_present_2026_6.zip)
- Holdout archive SHA-256 `203509350f574f81728ca51175a17360d0413aa990512f8e0c8f1342f5baf23a`. [August 2026 BTS ZIP](https://transtats.bts.gov/PREZIP/On_Time_Reporting_Carrier_On_Time_Performance_1987_present_2026_8.zip)
- Bundled artifact SHA-256 `38e20f8b2d1d6a43c5cb072b030fda0019d3905b1f89fbae112fb7b44479ac65`. It contains aggregate counts. [Bundled artifact](../../internal/historicalrisk/bts-departure-risk-2026-06.json)
