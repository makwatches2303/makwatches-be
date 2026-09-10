#!/usr/bin/env python3
"""Builds a Lambda `--environment file://...` JSON payload from a .env file.

Usage:
  render_env.py ENV_FILE OUT_FILE [--include K1,K2,...] [--exclude K1,K2,...] [--set K=V ...]

--include restricts to only the listed keys (present-in-.env ones; missing
  keys are silently skipped). --exclude keeps everything except the listed
  keys. At most one of the two may be given. --set K=V is applied last and
  always wins. Prints a byte-size sanity check to stderr (Lambda's combined
  env-var limit is 4096 bytes).
"""
import argparse
import json
import sys


def parse_env_file(path):
    variables = {}
    with open(path) as f:
        for line in f:
            line = line.rstrip("\n")
            if not line or line.lstrip().startswith("#") or "=" not in line:
                continue
            key, value = line.split("=", 1)
            key, value = key.strip(), value.strip()
            if len(value) >= 2 and value[0] == value[-1] and value[0] in ("'", '"'):
                value = value[1:-1]
            variables[key] = value
    return variables


def main():
    p = argparse.ArgumentParser()
    p.add_argument("env_file")
    p.add_argument("out_file")
    p.add_argument("--include", default="")
    p.add_argument("--exclude", default="")
    p.add_argument("--set", action="append", default=[])
    args = p.parse_args()

    if args.include and args.exclude:
        sys.exit("--include and --exclude are mutually exclusive")

    variables = parse_env_file(args.env_file)
    if args.include:
        keep = set(args.include.split(","))
        variables = {k: v for k, v in variables.items() if k in keep}
    elif args.exclude:
        drop = set(args.exclude.split(","))
        variables = {k: v for k, v in variables.items() if k not in drop}

    for tok in args.set:
        if "=" in tok:
            k, v = tok.split("=", 1)
            variables[k] = v

    with open(args.out_file, "w") as f:
        json.dump({"Variables": variables}, f)

    total = sum(len(k) + len(v) for k, v in variables.items())
    print(f"  {args.out_file}: {len(variables)} vars, ~{total} bytes of key+value "
          f"(Lambda's combined limit is 4096 bytes)", file=sys.stderr)


if __name__ == "__main__":
    main()
