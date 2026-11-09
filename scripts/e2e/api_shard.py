#!/usr/bin/env python3
"""Pick this shard's API e2e tests: longest-processing-time-first over measured durations.

    go test -tags=e2e -list '^Test' ./test/... | grep '^Test' \
      | python3 scripts/e2e/api_shard.py <index> <total>

Durations come from scripts/e2e/api-test-durations.tsv (seconds, from CI logs). A test
missing from it weighs DEFAULT_SECONDS, so a new test is placed without a table edit.
Round-robin over sorted names left one shard ~2x the lightest (two of the three
longest tests landed together), and that shard was the e2e job's critical path.
"""
import pathlib
import sys

DEFAULT_SECONDS = 15
TABLE = pathlib.Path(__file__).with_name("api-test-durations.tsv")


def main() -> int:
    index, total = int(sys.argv[1]), int(sys.argv[2])
    if not 0 <= index < total:
        sys.exit(f"shard index {index} out of range for {total} shards")
    known = {}
    for line in TABLE.read_text().splitlines():
        if line.strip():
            name, seconds = line.split("\t")
            known[name] = float(seconds)
    tests = sorted({t.strip() for t in sys.stdin if t.strip().startswith("Test")})
    loads = [0.0] * total
    picked = [[] for _ in range(total)]
    for name in sorted(tests, key=lambda t: (-known.get(t, DEFAULT_SECONDS), t)):
        shard = min(range(total), key=lambda i: (loads[i], i))
        loads[shard] += known.get(name, DEFAULT_SECONDS)
        picked[shard].append(name)
    print(f"shard {index + 1}/{total}: {len(picked[index])} tests, ~{loads[index] / 60:.1f} min "
          f"(all shards: {', '.join(f'{x / 60:.1f}' for x in loads)})", file=sys.stderr)
    print("\n".join(sorted(picked[index])))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
