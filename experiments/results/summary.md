# Experiment results

All times are seconds from the moment the leechers started (runner `t0`). Mean and sample standard deviation are taken over repeated runs. Raw data: `results/<exp>/runs.csv` and the per-peer CSVs.


## Claim test: 6 peers, strategy x choking, with a free-rider

Distribution time = the last of the 4 contributors finishing. The free-rider is compared with the average contributor (a single peer against a single peer); positive = the free-rider is slower.

| strategy | choking | runs | distribution s (last contributor) | average contributor s | free-rider s | free-rider minus average contributor s |
|---|---|---|---|---|---|---|
| rarest | tft | 5 | 22.27 ± 1.67 | 22.13 ± 1.60 | 20.11 ± 0.51 | -2.02 |
| rarest | off | 5 | 25.83 ± 0.70 | 25.73 ± 0.73 | 21.41 ± 1.24 | -4.33 |
| random | tft | 5 | 20.35 ± 0.79 | 20.23 ± 0.73 | 19.77 ± 0.52 | -0.46 |
| random | off | 5 | 24.92 ± 0.84 | 24.73 ± 0.81 | 23.66 ± 0.56 | -1.08 |
| sequential | tft | 5 | 64.59 ± 0.25 | 64.43 ± 0.24 | 64.49 ± 0.11 | +0.05 |
| sequential | off | 5 | 80.09 ± 0.00 | 80.02 ± 0.00 | 80.11 ± 0.00 | +0.09 |
## Experiment 1: swarm size and seeder count

| swarm | seeders | leechers | runs | mean s | std s | min s | max s | timeouts |
|---|---|---|---|---|---|---|---|---|
| 4 | 1 | 3 | 5 | 19.18 | 0.42 | 18.57 | 19.59 | 0 |
| 6 | 1 | 5 | 5 | 20.01 | 0.75 | 19.00 | 21.00 | 0 |
| 8 | 1 | 7 | 5 | 21.23 | 0.72 | 20.21 | 22.19 | 0 |
| 10 | 1 | 9 | 5 | 21.37 | 0.59 | 20.50 | 22.11 | 0 |
| 4 | 2 | 2 | 5 | 9.59 | 0.39 | 9.06 | 10.07 | 0 |
| 6 | 2 | 4 | 5 | 12.02 | 0.11 | 11.82 | 12.09 | 0 |
| 8 | 2 | 6 | 5 | 13.90 | 0.14 | 13.75 | 14.05 | 0 |
| 10 | 2 | 8 | 5 | 15.03 | 0.21 | 14.80 | 15.32 | 0 |

## Experiment 2: piece-selection strategy

| strategy | runs | mean s | std s | min s | max s | timeouts |
|---|---|---|---|---|---|---|
| rarest | 5 | 21.21 | 0.63 | 20.75 | 22.30 | 0 |
| random | 5 | 22.00 | 0.67 | 20.99 | 22.85 | 0 |
| sequential | 5 | 65.11 | 0.18 | 64.88 | 65.38 | 0 |

## Experiment 3: free-rider

| choking | peers | runs | mean s | std s | min s | max s |
|---|---|---|---|---|---|---|
| tft | contributors (mean of 6) | 5 | 20.84 | 0.81 | 20.23 | 22.04 |
| tft | free-rider | 5 | 21.07 | 0.91 | 20.31 | 22.32 |
| off | contributors (mean of 6) | 5 | 28.54 | 1.30 | 27.50 | 30.78 |
| off | free-rider | 5 | 24.67 | 1.75 | 22.92 | 26.43 |
