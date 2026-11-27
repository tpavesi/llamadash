# Test fixtures

All fixtures are scrubbed: `chat_template`, `generation_prompt`, `media_marker` and any
string over 200 chars are replaced by `"<scrubbed>"`; `model_path` is reduced to the file name.

## b12679/ (captured with `tools/capture.py` from a real llama-server, build b12679-af5c58347)

- `health.json`, `props.json` - `/health` and `/props` (one slot, n_ctx 262144, speculative decoding).
- `slots_busy.json` - `/slots` with the single slot processing (first busy sample of the watch).
- `slots_idle.json` - `/slots` after the request finished.
- `slots_seq/NNN.json` - 93 polls at ~500ms: `{"t_ms": ..., "slots": [...]}`, real capture.
- `metrics_501.json` - HAND-WRITTEN. The server was observed returning 501 with a JSON error
  body earlier; during the capture `/metrics` returned 200 (the server now reports
  `endpoint_metrics: true`), so the 501 body here is an approximation of llama.cpp's
  "not supported" error shape, not a recording.

## synthetic/ (hand-written)

- `metrics.prom` - Prometheus text with the series the dashboard needs, plus a labelled series
  and two malformed lines. Note: the real b12679 `/metrics` has no `kv_cache_usage_ratio`
  (it exposes `n_tokens_max` instead); the series is kept as the brief specified.
- `slots_two_busy.json` - 4 slots, two processing; slot 1 has `next_token` as an object, slot 3
  has no `next_token`.

## What the sequence shows (read by tasks 2 and 5)

The watch began when the request was already past prompt processing and generating, so:

- (a) `n_decoded` reset on a new `id_task`: NOT observed. The whole sequence is one task
  (`id_task` 6894). Within a task `n_decoded` rises monotonically (1573 -> 5015 over ~47s, ~74/s).
  After the request ended, the idle slot kept `id_task` 6894 but `n_decoded` was 0 and
  `has_next_token` false, so idle shows as `n_decoded == 0`.
- (b) During generation `n_prompt_tokens_processed` (11666) and `n_prompt_tokens_cache` (133408)
  stay constant; `n_prompt_tokens` (146649 -> 150091) grows with `n_decoded` (it equals
  cache + processed + decoded - 2 here, i.e. it counts generated tokens too). So
  `processed + cache` is the prompt size, and prompt progress cannot be derived from
  `n_prompt_tokens`. How the counters move DURING prefill was not observed. After the slot
  goes idle, `n_prompt_tokens_processed` and `n_prompt_tokens_cache` read 0 and
  `n_prompt_tokens` keeps its final value (150115).
- `slots_seq` timing: `t_ms` is relative to the start of the watch.

## hwmon/ (hand-written)

Small `/sys/class/hwmon` trees for `internal/hwmon`; values are invented but realistic.

- `amd/` - AMD board: `k10temp` (`Tctl` 60.125 C and two `TccdN`), a `nct6798` Super I/O with
  seven unlabelled fan inputs of which three read 0 (unconnected headers), two `nvme` chips and a
  network chip with temperatures only, and `hwmon4` without a `name` file.
- `intel/` - Intel board: `coretemp` with `Package id 0` and cores, `it8688` with labelled fans
  (one label empty).
- `gpuonly/` - only an `amdgpu` chip with a fan: unavailable.
- `garbage/` - empty, text, fractional, negative and huge fan values, a value without its
  newline, a label with control characters, and a `Tctl` of 200 C.

## openrgb/ (`openrgb --list-detailed` output)

- `list_detailed.txt` - one motherboard, as OpenRGB 1.0 printed it for a real board (the board's
  model name is public product information; nothing else in it identifies a machine).
- `list_two_devices.txt` - HAND-WRITTEN: a log line OpenRGB prints before the list, the board,
  and a keyboard without an `Off` mode.
- `list_garbage.txt` - HAND-WRITTEN: control characters, a 300-character name, bad indexes, an
  unclosed quote and 40 devices (more than the parser keeps).
- `list_empty.txt` - empty.
