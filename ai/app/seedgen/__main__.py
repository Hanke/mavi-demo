"""CLI: python -m app.seedgen {plan | generate | render}

plan      print the deterministic distribution (no network)
generate  write infra/db/seed/data/candidates.json with the model (needs ANTHROPIC_API_KEY)
          --count N        how many candidates (default 200)
          --only 3,17,42   regenerate just these slots and merge into the existing file
render    write infra/db/seed/*.sql from the JSON files (no network)
"""

from __future__ import annotations

import argparse
import json
import sys
from collections.abc import Callable
from pathlib import Path
from typing import cast

from app.seedgen import generate, plan, render


def cmd_plan(args: argparse.Namespace) -> int:
    slots = plan.build_plan(count=cast(int, args.count))
    print(plan.summarize(slots))
    return 0


def _parse_slots(spec: str) -> set[int]:
    wanted: set[int] = set()
    for raw in spec.split(","):
        part = raw.strip()
        if not part:
            continue
        if "-" in part:
            lo, hi = part.split("-", 1)
            wanted.update(range(int(lo), int(hi) + 1))
        else:
            wanted.add(int(part))
    return wanted


def cmd_generate(args: argparse.Namespace) -> int:
    slots = plan.build_plan(count=cast(int, args.count))
    only = cast(str | None, args.only)
    if only:
        wanted = _parse_slots(only)
        slots = [s for s in slots if s.index in wanted]
    print(f"generating {len(slots)} candidates with {generate.MODEL} ...", file=sys.stderr)
    gen = generate.Generator()
    results = gen.run(slots)
    total = generate.merge_into_file(render.CANDIDATES_JSON, results, generate.MODEL)
    print(
        f"wrote {render.CANDIDATES_JSON} ({total} candidates; {gen.usage_in} input / {gen.usage_out} output tokens)",
        file=sys.stderr,
    )
    return 0


def cmd_specs(args: argparse.Namespace) -> int:
    slots = plan.build_plan(count=cast(int, args.count))
    wanted = _parse_slots(cast(str, args.slots))
    slots = [s for s in slots if s.index in wanted]
    print("# Writing brief\n")
    print(generate.SYSTEM_PROMPT)
    print("\n# Output\n")
    print(
        'A JSON file of the shape {"candidates": [{slot, headline, resume_text, skills, other_software, other_certifications}]},'
    )
    print("one entry per spec below, then `python -m app.seedgen ingest <file>`.\n")
    print("# Specs\n")
    print(json.dumps(generate.specs_for(slots), indent=2, ensure_ascii=False))
    return 0


def cmd_ingest(args: argparse.Namespace) -> int:
    slots = plan.build_plan(count=cast(int, args.count))
    path = Path(cast(str, args.file))
    batch = generate.GeneratedBatch.model_validate_json(path.read_text(encoding="utf-8"))
    good, bad = generate.ingest(batch, slots)
    total = generate.merge_into_file(render.CANDIDATES_JSON, good, cast(str, args.model)) if good else 0
    print(
        f"ingested {len(good)} of {len(batch.candidates)} entries from {path} ({total} candidates in file)",
        file=sys.stderr,
    )
    for index, problems in sorted(bad.items()):
        print(f"  slot {index}: {'; '.join(problems)}", file=sys.stderr)
    return 1 if bad else 0


def cmd_render(_: argparse.Namespace) -> int:
    for path in render.write_all():
        print(f"wrote {path}", file=sys.stderr)
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="python -m app.seedgen", description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    sub = parser.add_subparsers(dest="command", required=True)
    p = sub.add_parser("plan")
    p.add_argument("--count", type=int, default=plan.DEFAULT_COUNT)
    p.set_defaults(func=cmd_plan)
    g = sub.add_parser("generate")
    g.add_argument("--count", type=int, default=plan.DEFAULT_COUNT)
    g.add_argument("--only", default=None, help="comma-separated slot numbers to regenerate")
    g.set_defaults(func=cmd_generate)
    sp = sub.add_parser("specs")
    sp.add_argument("--count", type=int, default=plan.DEFAULT_COUNT)
    sp.add_argument("--slots", required=True, help="slot numbers, e.g. 1-20 or 3,17,42")
    sp.set_defaults(func=cmd_specs)
    ing = sub.add_parser("ingest")
    ing.add_argument("file")
    ing.add_argument("--count", type=int, default=plan.DEFAULT_COUNT)
    ing.add_argument("--model", default=generate.MODEL, help="recorded in the file's generator metadata")
    ing.set_defaults(func=cmd_ingest)
    r = sub.add_parser("render")
    r.set_defaults(func=cmd_render)
    args = parser.parse_args(argv)
    func = cast(Callable[[argparse.Namespace], int], args.func)
    return func(args)


if __name__ == "__main__":
    sys.exit(main())
