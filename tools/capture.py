#!/usr/bin/env python3
"""Capture scrubbed llama-server fixtures. GET requests only.

Usage: python tools/capture.py http://host:8011 testdata/b12679 [--watch-seconds 300]
"""
import argparse
import json
import os
import time
import urllib.error
import urllib.request

SCRUB = "<scrubbed>"
SCRUB_KEYS = {"chat_template", "generation_prompt", "media_marker"}


def scrub(v, key=None):
    if key in SCRUB_KEYS:
        return SCRUB
    if key == "model_path" and isinstance(v, str):
        return v.replace("\\", "/").rsplit("/", 1)[-1]
    if isinstance(v, dict):
        return {k: scrub(x, k) for k, x in v.items()}
    if isinstance(v, list):
        return [scrub(x) for x in v]
    if isinstance(v, str) and len(v) > 200:
        return SCRUB
    return v


def get(base, path):
    req = urllib.request.Request(base.rstrip("/") + path, method="GET")
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def get_json(base, path):
    status, body = get(base, path)
    try:
        return status, scrub(json.loads(body))
    except ValueError:
        return status, None


def write(path, obj):
    with open(path, "w", encoding="utf-8") as f:
        json.dump(obj, f, indent=2)
        f.write("\n")


def busy(slots):
    return isinstance(slots, list) and any(
        isinstance(s, dict) and s.get("is_processing") for s in slots)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("base")
    ap.add_argument("out")
    ap.add_argument("--watch-seconds", type=float, default=0)
    a = ap.parse_args()
    os.makedirs(a.out, exist_ok=True)

    for name in ("health", "props"):
        st, obj = get_json(a.base, "/" + name)
        write(os.path.join(a.out, name + ".json"), obj)
        print(name, st)
    st, obj = get_json(a.base, "/metrics")
    print("metrics", st)
    if st != 200:
        write(os.path.join(a.out, "metrics_501.json"), obj)
    st, slots = get_json(a.base, "/slots")
    print("slots", st, "busy" if busy(slots) else "idle")
    write(os.path.join(a.out, "slots_busy.json" if busy(slots) else "slots_idle.json"), slots)

    if a.watch_seconds > 0:
        seqdir = os.path.join(a.out, "slots_seq")
        os.makedirs(seqdir, exist_ok=True)
        t0 = time.time()
        n = 0
        recording = False
        while time.time() - t0 < a.watch_seconds and n < 120:
            _, slots = get_json(a.base, "/slots")
            if busy(slots):
                if not recording:
                    write(os.path.join(a.out, "slots_busy.json"), slots)
                recording = True
                n += 1
                write(os.path.join(seqdir, "%03d.json" % n),
                      {"t_ms": int((time.time() - t0) * 1000), "slots": slots})
            elif recording:
                write(os.path.join(a.out, "slots_idle.json"), slots)
                break
            elif isinstance(slots, list):
                write(os.path.join(a.out, "slots_idle.json"), slots)
            time.sleep(0.5)
        print("samples", n)


if __name__ == "__main__":
    main()
