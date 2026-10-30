#!/usr/bin/env python3
"""Extract and verify a source-edit fixture from an immutable SWE trace.

This preserves five original tool calls, including complete Write/Edit text.
It is a trace-derived fixture, not a complete trace or compilation replay.
"""

import argparse
import copy
import hashlib
import json
from pathlib import Path, PurePosixPath


TRACE_ID = "biofabric-rust-rewrite-897c0fc8-381"
SOURCE_SHA256 = "6345988d0e84ca897b54a875fa6b4a4fe879b620ad095d3a61bfde825490d9e2"
SELECTED = {
    50: ("call_4ddcd8df06ba4b95bc244c91", "Write"),
    55: ("call_13c2a9e63d394ed8b1656659", "Write"),
    60: ("call_b86af56480314c82944e5187", "Edit"),
    63: ("call_5b1a809063144a479bae4f55", "Edit"),
    65: ("call_ecf9273a896d455a8f80a5b8", "Edit"),
}
TARGETS = (
    "/app/crates/core/src/model/link.rs",
    "/app/crates/core/src/model/network.rs",
)
READ_IDS = {
    "call_6b9344f81af8415eb145211a",
    "call_392839277e1b4d50b05d51ae",
}
PHASES = ("after_writes", "after_edits")


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def apply_tool(files, tool):
    """Apply original UTF-8 Write/Edit semantics; reject ambiguous edits."""
    params = tool["params"]
    path = params["file_path"]
    if path not in TARGETS:
        raise ValueError(f"unexpected destination {path}")
    if tool["name"] == "Write":
        files[path] = params["content"]
    elif tool["name"] == "Edit":
        old = params["old_string"]
        new = params["new_string"]
        if not old or path not in files:
            raise ValueError(f"missing edit input at step {tool['step_id']}")
        count = files[path].count(old)
        replace_all = params.get("replace_all", False)
        if count == 0 or (not replace_all and count != 1):
            raise ValueError(f"ambiguous edit at step {tool['step_id']}: {count} matches")
        files[path] = files[path].replace(old, new) if replace_all else files[path].replace(old, new, 1)
    else:
        raise ValueError(f"unsupported original tool {tool['name']}")


def expected_states(tools):
    files = {}
    states = {}
    for tool in tools:
        apply_tool(files, tool)
        if tool["step_id"] in (55, 65):
            phase = "after_writes" if tool["step_id"] == 55 else "after_edits"
            states[phase] = copy.deepcopy(files)
    return states


def file_manifest(files):
    return [
        {"file_path": path, "sha256": sha256(text.encode("utf-8")), "size_bytes": len(text.encode("utf-8"))}
        for path, text in sorted(files.items())
    ]


def extract(source):
    raw = source.read_bytes()
    digest = sha256(raw)
    if digest != SOURCE_SHA256:
        raise ValueError(f"source hash mismatch: {digest}; expected {SOURCE_SHA256}")
    trace = json.loads(raw)
    tools = []
    excluded = []
    read_probes = []
    all_count = 0
    for step in trace["steps"]:
        sid = step["step_id"]
        calls = step.get("tool_calls") or []
        all_count += len(calls)
        omitted = []
        for call in calls:
            cid, name = call["tool_call_id"], call["function_name"]
            if sid in SELECTED and (cid, name) == SELECTED[sid]:
                tools.append({
                    "step_id": sid, "call_id": cid, "name": name,
                    "phase": "before_checkpoint" if sid <= 55 else "after_restore",
                    "params": copy.deepcopy(call["arguments"]),
                })
            elif 50 <= sid <= 65:
                omitted.append({"call_id": cid, "name": name})
            if cid in READ_IDS:
                read_probes.append({
                    "original_step_id": sid, "call_id": cid, "name": name,
                    "params": copy.deepcopy(call["arguments"]),
                    "validation_only": True,
                    "historical_observation_reused": False,
                })
        if 50 <= sid <= 65 and sid not in SELECTED:
            excluded.append({"step_id": sid, "tools": omitted})
        elif omitted:
            raise ValueError(f"unexpected extra tool in selected step {sid}")
    if [(t["step_id"], (t["call_id"], t["name"])) for t in tools] != list(SELECTED.items()):
        raise ValueError("selected original calls missing, duplicated, or reordered")
    if len(read_probes) != 2:
        raise ValueError("original Read parameter probes missing")
    states = expected_states(tools)
    return {
        "schema_version": 1,
        "fixture_id": "biofabric-381-source-edit-50-65-v1",
        "evidence_type": "trace_derived_source_edit_fixture",
        "complete_original_trace": False,
        "source": {
            "trace_id": TRACE_ID, "normalized_file": "agent/trajectory.json",
            "sha256": digest, "size_bytes": len(raw),
            "original_steps": len(trace["steps"]), "original_tool_calls": all_count,
        },
        "scope": {
            "preserved": "Exact original call IDs, step IDs and full tool arguments; UTF-8 file bytes and absolute paths.",
            "precondition": "Destination parent directories exist; both files are completely established by the original Write calls.",
            "checkpoint": "Execute Write steps 50 and 55; pause, publish and restore; execute Edit steps 60, 63 and 65.",
            "oracle": "Independent byte application of the original arguments; compare both full files before checkpoint, after restore and after edits.",
            "not_tested": "Compilation, Rust/Java dependencies, original task outcome, agent task management, original timing, or complete trace compatibility.",
        },
        "tools": tools,
        "excluded_steps": excluded,
        "excluded_ranges": [{"step_id_lt": 50}, {"step_id_gt": 65}],
        "exclusion_reason": "Explicitly selected file-edit fixture; skipped Bash/Read/TaskUpdate steps are not counted as passing original trace calls.",
        "read_probes": read_probes,
        "read_probe_contract": "Original Read parameters from steps 13 and 20 can be reapplied as validation-only probes. Their historical results describe an earlier state and must not be reused as the checkpoint oracle. Full-file bytes are authoritative.",
        "checkpoints": {
            phase: {"after_step_id": 55 if phase == "after_writes" else 65,
                    "expected_files": file_manifest(states[phase])}
            for phase in PHASES
        },
    }


def verify(fixture, source=None):
    if fixture["schema_version"] != 1 or fixture["complete_original_trace"] is not False:
        raise ValueError("incorrect fixture evidence boundary")
    tools = fixture["tools"]
    if [(t["step_id"], (t["call_id"], t["name"])) for t in tools] != list(SELECTED.items()):
        raise ValueError("unexpected call selection")
    if fixture["source"]["sha256"] != SOURCE_SHA256:
        raise ValueError("unexpected trace identity")
    states = expected_states(tools)
    for phase in PHASES:
        if fixture["checkpoints"][phase]["expected_files"] != file_manifest(states[phase]):
            raise ValueError(f"incorrect oracle for {phase}")
    if source is not None and fixture != extract(source):
        raise ValueError("fixture is not the exact deterministic extraction of the original trace")
    return states


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    gen = sub.add_parser("extract", help="extract from the pinned original trace")
    gen.add_argument("--source", type=Path, required=True)
    gen.add_argument("--output", type=Path, required=True)
    check = sub.add_parser("verify", help="verify oracle; optionally compare all fields to original source")
    check.add_argument("--fixture", type=Path, required=True)
    check.add_argument("--source", type=Path)
    check.add_argument("--directory", type=Path, help="read guest files downloaded into an isolated root directory")
    check.add_argument("--phase", choices=PHASES, default="after_edits")
    export = sub.add_parser("expected", help="write independent expected UTF-8 files to a new directory")
    export.add_argument("--fixture", type=Path, required=True)
    export.add_argument("--phase", choices=PHASES, required=True)
    export.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.command == "extract":
        fixture = extract(args.source)
        verify(fixture, args.source)
        # Refuse accidental replacement of a prior fixture or receipt.
        with args.output.open("x", encoding="utf-8") as stream:
            json.dump(fixture, stream, ensure_ascii=False, indent=2)
            stream.write("\n")
    else:
        fixture = json.loads(args.fixture.read_text(encoding="utf-8"))
        states = verify(fixture, getattr(args, "source", None))
        if args.command == "expected":
            args.output.mkdir()
            for path, content in states[args.phase].items():
                dest = args.output.joinpath(*PurePosixPath(path).parts[1:])
                dest.parent.mkdir(parents=True, exist_ok=True)
                with dest.open("xb") as stream:
                    stream.write(content.encode("utf-8"))
        elif args.directory is not None:
            for path, content in states[args.phase].items():
                observed = args.directory.joinpath(*PurePosixPath(path).parts[1:]).read_bytes()
                if observed != content.encode("utf-8"):
                    raise ValueError(f"observed file mismatch: {path}")
    print(json.dumps({"result": "passed", "command": args.command,
                      "fixture_id": fixture["fixture_id"],
                      "complete_original_trace": False}, sort_keys=True))


if __name__ == "__main__":
    main()
