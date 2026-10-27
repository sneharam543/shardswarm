# Free-rider study (Experiment 3 follow-up)

**Question:** in Experiment 3 (16 MiB file), tit-for-tat did not slow the free-rider. Was that because the runs were too short, or because of the code? Each step changed one thing and repeated Experiment 3 five times per policy.

**Setup:** 1 seeder + 6 contributors + 1 free-rider, a **64 MiB** file, 1 MiB/s upload cap on every peer, rarest-first. Each run lasts 70–90 s, which is 7–9 rechoke rounds. Times are completion times in seconds from leecher start, as mean ± sample std over 5 runs.

| Step | Code change | TFT: contributors | TFT: free-rider | Off: contributors | Off: free-rider |
|------|-------------|-------------------|-----------------|-------------------|-----------------|
| 1 | none (longer runs only) | 79.9 ± 5.9 | 70.5 ± 0.4 | 87.4 ± 1.5 | 73.9 ± 1.7 |
| 2 | free slots and rank ties go to peers that have given us more (`totalDown`) | 76.5 ± 6.1 | 70.5 ± 0.8 | 85.9 ± 2.3 | 73.9 ± 2.7 |
| 3 | step 2 + only Piece data counts against the upload cap | 81.8 ± 3.2 | 71.2 ± 1.0 | 88.5 ± 1.3 | 74.6 ± 1.6 |

## Findings

1. **Run length is not the cause.** With roughly 4× longer runs, the free-rider still finished about 9 s before the contributors' mean.
2. **The slot-allocation fix had no measurable effect** (free-rider 70.5 s in both step 1 and step 2).
3. **The Piece-only rate limit had no measurable effect on the outcome either.** It does remove a real delay: a unit test shows a Have message waiting 951 ms behind capped piece data before the change, and about 50 ms after it.
4. **Tit-for-tat still helps the contributors.** In every step they are 6–10 s faster with tit-for-tat than with choking off.
5. **Most likely structural reason (a hypothesis, not yet tested):**
   - each contributor has only about 6 interested peers competing for 4 upload slots (3 regular + 1 optimistic);
   - so the free-rider is unchoked by most contributors most of the time, and there is little to withhold;
   - tit-for-tat needs scarce slots, as in real swarms where many peers compete for 4 slots.

   The free-rider's time (about 70.5 s) is close to the 64 s floor that a single 1 MiB/s seeder imposes.
6. **Not explained:** why the free-rider is *faster* than the contributors, not just as fast. This was not measured; per-source byte counts would be needed.

**Raw data:** `1-long-runs-current-policy/`, `2-long-runs-contributors-first/`, `3-long-runs-piece-only-limit/` (runs.csv, per-peer CSVs, plots).
