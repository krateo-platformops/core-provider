### Time to all-Ready, per run

| run | arm | Ready | all-Ready (s) | timed out | cdc pod | node | image digest | restarts / OOM |
|---|---|---|---|---|---|---|---|---|
| 1 | off | 150/150 | 257 | False | `satcharts-v0-1-0-controller-7b7588f459-qf9gl` | zk7m | `sha256:ae54ea35441a` | 0 / False |
| 2 | on | 150/150 | 282 | False | `satcharts-v0-1-0-controller-69688fb4f8-6sbpb` | zk7m | `sha256:ae54ea35441a` | 0 / False |
| 3 | on | 150/150 | 273 | False | `satcharts-v0-1-0-controller-565d7b4cb5-2z5l6` | zk7m | `sha256:ae54ea35441a` | 0 / False |
| 4 | off | 150/150 | 253 | False | `satcharts-v0-1-0-controller-675fc6b669-tf2gx` | zk7m | `sha256:ae54ea35441a` | 0 / False |
| 5 | off | 150/150 | 253 | False | `satcharts-v0-1-0-controller-6f6d64dd6d-82h8x` | zk7m | `sha256:ae54ea35441a` | 0 / False |
| 6 | on | 150/150 | 280 | False | `satcharts-v0-1-0-controller-64ddd5f48-wv8zd` | zk7m | `sha256:ae54ea35441a` | 0 / False |

### Burst: 150 compositions created at once

| run | arm | reconciles | p50 / p95 (s) | pressure mean / max 30s window (%) | 30s windows over 50 | throttled (%) | CPU (cores, % of limit) | gate capacity min / mean | gate samples below workers | cuts (log) |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | off | 465 | 39.45 / 219.67 | 56.9 / 81.3 | 25/32 | 97.6 | 0.246, 98.3 | - / - | 0/0 | 0 |
| 2 | on | 469 | 32.33 / 226.55 | 50.1 / 68.7 | 14/29 | 87.2 | 0.243, 97.3 | 1 / 4.2 | 86/86 | 17 |
| 3 | on | 465 | 22.16 / 225.44 | 51.0 / 74.4 | 7/22 | 89.1 | 0.245, 98.1 | 2 / 4.1 | 72/72 | 18 |
| 4 | off | 463 | 22.10 / 216.93 | 58.7 / 83.4 | 18/21 | 97.5 | 0.251, 100.3 | - / - | 0/0 | 0 |
| 5 | off | 478 | 23.82 / 215.53 | 57.3 / 68.5 | 18/21 | 99.4 | 0.251, 100.6 | - / - | 0/0 | 0 |
| 6 | on | 460 | 24.48 / 227.00 | 49.0 / 69.4 | 8/30 | 87.1 | 0.237, 94.8 | 1 / 4.1 | 88/88 | 18 |

### Burst, per arm

| metric | off | on |
|---|---|---|
| time to all-Ready (s) | 254.3 (min 253.0, max 257.0, sd 2.3, n=3) | 278.3 (min 273.0, max 282.0, sd 4.7, n=3) |
| reconcile p50 (s) | 28.5 (min 22.1, max 39.5, sd 9.6, n=3) | 26.3 (min 22.2, max 32.3, sd 5.3, n=3) |
| reconcile p95 (s) | 217.4 (min 215.5, max 219.7, sd 2.1, n=3) | 226.3 (min 225.4, max 227.0, sd 0.8, n=3) |
| reconciles run | 468.7 (min 463.0, max 478.0, sd 8.1, n=3) | 464.7 (min 460.0, max 469.0, sd 4.5, n=3) |
| cpu.pressure, whole-burst mean (%) | 57.6 (min 56.9, max 58.7, sd 0.9, n=3) | 50.0 (min 49.0, max 51.0, sd 1.0, n=3) |
| cpu.pressure, max 30s window (%) | 77.7 (min 68.5, max 83.4, sd 8.1, n=3) | 70.8 (min 68.7, max 74.4, sd 3.1, n=3) |
| throttled periods (%) | 98.1 (min 97.5, max 99.4, sd 1.1, n=3) | 87.8 (min 87.1, max 89.1, sd 1.1, n=3) |
| CPU used (% of limit) | 99.7 (min 98.3, max 100.6, sd 1.3, n=3) | 96.8 (min 94.8, max 98.1, sd 1.7, n=3) |
| gate capacity, mean | - | 4.1 (min 4.1, max 4.2, sd 0.1, n=3) |
| gate capacity, min | - | 1.3 (min 1.0, max 2.0, sd 0.6, n=3) |
| gate cuts (log lines) | 0.0 (min 0.0, max 0.0, sd 0.0, n=3) | 17.7 (min 17.0, max 18.0, sd 0.6, n=3) |

### At rest after the burst (150 compositions, after a grace period)

| run | arm | reconciles | p50 / p95 (s) | pressure mean / max 30s window (%) | 30s windows over 50 | throttled (%) | CPU (cores, % of limit) | gate capacity min / mean | gate samples below workers | cuts (log) |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | off | 430 | 74.86 / 165.13 | 60.1 / 87.2 | 22/32 | 99.5 | 0.249, 99.8 | - / - | 0/0 | 0 |
| 2 | on | 378 | 86.49 / 226.18 | 47.8 / 69.2 | 10/27 | 87.8 | 0.233, 93.0 | 2 / 4.3 | 83/83 | 15 |
| 3 | on | 384 | 85.69 / 224.96 | 46.5 / 66.0 | 9/26 | 88.8 | 0.228, 91.1 | 2 / 4.1 | 85/85 | 16 |
| 4 | off | 442 | 72.38 / 99.85 | 60.5 / 79.6 | 18/18 | 99.9 | 0.241, 96.6 | - / - | 0/0 | 0 |
| 5 | off | 447 | 71.14 / 97.47 | 62.6 / 81.0 | 20/24 | 100.0 | 0.260, 103.9 | - / - | 0/0 | 0 |
| 6 | on | 377 | 89.27 / 229.36 | 48.9 / 65.3 | 14/29 | 89.3 | 0.239, 95.6 | 2 / 4.4 | 88/88 | 15 |

### Light load (10 compositions at rest): the false-throttle probe

| run | arm | reconciles | p50 / p95 (s) | pressure mean / max 30s window (%) | 30s windows over 50 | throttled (%) | CPU (cores, % of limit) | gate capacity min / mean | gate samples below workers | cuts (log) |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | off | 50 | 7.50 / 9.75 | 6.4 / 13.5 | 0/43 | 31.3 | 0.033, 13.4 | - / - | 0/0 | 0 |
| 2 | on | 50 | 7.50 / 9.75 | 7.0 / 12.9 | 0/34 | 29.8 | 0.034, 13.4 | 16 / 16.0 | 0/93 | 0 |
| 3 | on | 50 | 7.50 / 9.75 | 6.5 / 13.3 | 0/39 | 27.6 | 0.031, 12.4 | 16 / 16.0 | 0/108 | 0 |
| 4 | off | 50 | 7.45 / 9.74 | 5.8 / 12.0 | 0/31 | 31.0 | 0.032, 12.9 | - / - | 0/0 | 0 |
| 5 | off | 50 | 7.45 / 9.74 | 5.5 / 12.0 | 0/39 | 28.2 | 0.029, 11.5 | - / - | 0/0 | 0 |
| 6 | on | 50 | 7.45 / 9.74 | 5.5 / 14.5 | 0/39 | 27.3 | 0.028, 11.2 | 16 / 16.0 | 0/105 | 0 |

### Instrument and control

- run 1 (off): gate samples 0 in the burst, max gate input -%, cut log lines 0 (0 without a timestamp), floor (capacity 1) reached: False
- run 2 (on): gate samples 86 in the burst, max gate input 58.5%, cut log lines 37 (0 without a timestamp), floor (capacity 1) reached: True
- run 3 (on): gate samples 72 in the burst, max gate input 57.7%, cut log lines 39 (0 without a timestamp), floor (capacity 1) reached: True
- run 4 (off): gate samples 0 in the burst, max gate input -%, cut log lines 0 (0 without a timestamp), floor (capacity 1) reached: False
- run 5 (off): gate samples 0 in the burst, max gate input -%, cut log lines 0 (0 without a timestamp), floor (capacity 1) reached: False
- run 6 (on): gate samples 88 in the burst, max gate input 61.3%, cut log lines 37 (0 without a timestamp), floor (capacity 1) reached: True
