# Experiment results

All times are seconds from the moment the leechers started (runner `t0`). Mean and sample standard deviation are taken over repeated runs. Raw data: `results/<exp>/runs.csv` and the per-peer CSVs.

## Experiment 1: swarm size and seeder count

| swarm | seeders | leechers | runs | mean s | std s | min s | max s | timeouts |
|---|---|---|---|---|---|---|---|---|
| 4 | 1 | 3 | 5 | 18.84 | 0.68 | 17.83 | 19.56 | 0 |
| 6 | 1 | 5 | 5 | 19.93 | 0.52 | 19.23 | 20.46 | 0 |
| 8 | 1 | 7 | 5 | 20.47 | 0.91 | 19.09 | 21.55 | 0 |
| 10 | 1 | 9 | 5 | 20.79 | 0.33 | 20.33 | 21.09 | 0 |
| 4 | 2 | 2 | 5 | 9.43 | 0.14 | 9.31 | 9.59 | 0 |
| 6 | 2 | 4 | 5 | 11.89 | 0.32 | 11.59 | 12.34 | 0 |
| 8 | 2 | 6 | 5 | 13.94 | 0.14 | 13.79 | 14.07 | 0 |
| 10 | 2 | 8 | 5 | 14.95 | 0.09 | 14.84 | 15.06 | 0 |

## Experiment 2: piece-selection strategy

| strategy | runs | mean s | std s | min s | max s | timeouts |
|---|---|---|---|---|---|---|
| rarest | 5 | 20.53 | 0.47 | 20.03 | 21.19 | 0 |
| random | 5 | 21.42 | 0.91 | 20.31 | 22.68 | 0 |
| sequential | 5 | 65.16 | 0.32 | 64.71 | 65.53 | 0 |

## Experiment 3: free-rider

| choking | peers | runs | mean s | std s | min s | max s |
|---|---|---|---|---|---|---|
| tft | contributors (mean of 6) | 5 | 21.64 | 1.23 | 20.04 | 22.62 |
| tft | free-rider | 5 | 19.97 | 0.47 | 19.64 | 20.78 |
| off | contributors (mean of 6) | 5 | 27.72 | 0.80 | 26.95 | 28.99 |
| off | free-rider | 5 | 23.43 | 1.51 | 21.27 | 24.87 |
