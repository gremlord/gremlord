#!/usr/bin/env python3
"""Create frozen local pilot tasks, then validate graders before any model run.

These are synthetic engineering tasks, not a representative coding benchmark.
Only starter code and public contracts are placed in candidate repositories.
"""
import hashlib
import importlib.util
import json
import random
import subprocess
import sys
from pathlib import Path


SSE_SPEC = """Implement sse.Decoder, an incremental SSE decoder for UTF-8 bytes.
feed(bytes) returns a list of dispatched event dicts, each with exactly data,
event, and id (strings). Data lines join with LF, and remove the final added LF.
An empty line dispatches only if at least one data field appeared (data: alone
counts). Default event is message; event resets after every blank line. Event
IDs persist across dispatched events; id containing NUL is ignored, and id:
resets to empty. Ignore comments, retry, and unknown fields. Split a field at
the first colon and strip exactly one following ASCII space, not arbitrary
whitespace. Accept LF, CR, and CRLF, including any split across feed calls.
Ignore one leading UTF-8 BOM. UTF-8 codepoints may cross byte chunks. Do not
emit a partially received event. close() returns any event completed by a
pending final CR delimiter, discards unfinished event data, and otherwise
returns []. Inputs are valid UTF-8 by close. Instances are independent.
Preserve the public API. You may add tests; no dependencies outside stdlib.
"""

LEDGER_SPEC = """Implement ledger.apply_batch(state, events) for an out-of-order
entity ledger. State maps entity IDs to {seq: integer, deleted: bool, fields:
dict}. Events are dicts with id (nonempty string), seq (nonnegative integer,
bool is not accepted), op ('put', 'patch', 'delete') and payload (a dict for
put/patch; absent or None for delete). Reject invalid input events with
ValueError, including invalid stale events. Validation applies to the entire
batch. Within one batch, two events with the same id and seq must be deeply
equal or raise ValueError, even if stale. Identical duplicates are harmless.
For each entity apply valid events in increasing sequence order; skip seq <=
the stored seq. put replaces all fields and clears deleted. delete writes a
tombstone with empty fields. patch merges shallowly: None removes a key;
other values replace it. A patch on an absent or deleted entity creates a
live entity from empty fields. State is initially valid. Never mutate state,
events, or nested input values, including on error; return a fully independent
deep copy, even for no-op batches. Preserve unrelated entities and tombstones.
Do not add dependencies outside stdlib. You may add tests.
"""

CONTEXT_SPEC = """Implement policy.decide(request) -> {pool, timeout_ms, audit}.
Use the signed ALDER policy records in the dossier below. For each key, the
largest numeric revision among signed records for tenant ALDER wins; unsigned
records and other tenants never apply. Every required key has a signed value.
Request has tenant, path, bytes, and premium (bool). For non-ALDER requests,
return pool=external_pool, timeout_ms=external_timeout, audit=False.
For ALDER, choose in this priority: blocked_pool when path equals blocked_path
or begins blocked_path + '/'; bulk_pool when bytes >= bulk_threshold; vip_pool
when premium; otherwise default_pool. Comparison is case-sensitive; no URL
decoding, trimming, or normalization. Timeout is fast_timeout for vip_pool and
default_pool; slow_timeout for bulk_pool; blocked_timeout for blocked_pool.
Audit is True precisely for ALDER requests with bytes >= audit_threshold or
whose chosen pool is blocked_pool. Inputs have valid types and nonnegative
bytes. The dossier is supplied as task context, not in the repo. Implement the
resolved policy values in code. No external dependencies. Add useful tests.
"""

POLICY = {
    "external_pool": "edge-17", "external_timeout": 913,
    "blocked_pool": "quarantine-42", "blocked_path": "/private-v3",
    "bulk_pool": "batch-29", "bulk_threshold": 65537,
    "vip_pool": "priority-61", "default_pool": "interactive-83",
    "fast_timeout": 1703, "slow_timeout": 12007,
    "blocked_timeout": 211, "audit_threshold": 8193,
}


def dossier():
    rng = random.Random(73109)
    records = []
    keys = list(POLICY)
    for i in range(1050):
        key = keys[i % len(keys)]
        value = rng.randrange(100, 90000) if isinstance(POLICY[key], int) else f"archive-{rng.randrange(10000)}"
        records.append(dict(tenant=f"TENANT-{i % 47}", key=key, revision=rng.randrange(1, 600), signed=True, value=value, note="historical regional routing migration; retained for audit and tenant isolation"))
    for i, (key, value) in enumerate(POLICY.items()):
        old = value - 1 if isinstance(value, int) else value + "-old"
        records.insert(23 + i * 71, dict(tenant="ALDER", key=key, revision=7, signed=True, value=old))
        records.insert(60 + i * 77, dict(tenant="ALDER", key=key, revision=31, signed=True, value=value))
        records.insert(140 + i * 70, dict(tenant="ALDER", key=key, revision=900, signed=False, value=old))
    return "\n".join(json.dumps(r, separators=(",", ":")) for r in records)


def module(path):
    spec = importlib.util.spec_from_file_location("candidate", path)
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    return m


def check_sse(m):
    def ev(data, event="message", id=""):
        return dict(data=data, event=event, id=id)
    cases = [
        (b"data: hello\n\n", [ev("hello")]),
        ("\ufeff:comment\r\nid: abc\revent: tick\rdata: hé😀\rdata:  two\r\rdata:\n\n".encode(), [ev("hé😀\n two", "tick", "abc"), ev("", id="abc")]),
        (b"event: forgotten\n\ndata:x\n\nid: a\ndata:y\n\nid: bad\x00id\ndata:z\n\nid:\ndata:end\n\n", [ev("x"), ev("y", id="a"), ev("z", id="a"), ev("end")]),
        (b"data\nunknown: no\nretry: 4\ndata: a:b\n\n", [ev("\na:b")]),
        (b"data: done\n\ndata: unfinished", [ev("done")]),
        (b"data: unfinished\n", []),
        (b"data: x\r\r", [ev("x")]),
        (b"data: \t tab \n\n", [ev("\t tab ")]),
    ]
    checks = 0
    for raw, expected in cases:
        # Every split exercises state boundaries, including UTF-8 and CRLF.
        for split in range(len(raw) + 1):
            d = m.Decoder()
            got = d.feed(raw[:split]) + d.feed(raw[split:]) + d.close()
            assert got == expected, (raw, split, got, expected)
            checks += 1
        d = m.Decoder()
        got = []
        for b in raw:
            got.extend(d.feed(bytes([b])))
        got.extend(d.close())
        assert got == expected, (got, expected)
    d = m.Decoder()
    assert d.feed(b"data: waiting\n") == [], "premature dispatch"
    assert d.feed(b"\n") == [ev("waiting")]
    d1, d2 = m.Decoder(), m.Decoder()
    assert d1.feed(b"id:a\ndata:first\n\n") == [ev("first", id="a")]
    assert d2.feed(b"data:second\n\n") == [ev("second")]
    rng = random.Random(889)
    expected = [ev(f"line-{i} 🦔\nvalue:{i}", "message" if i % 2 else "update", str(i)) for i in range(100)]
    raw = "".join(f"id:{i}\nevent:{e['event']}\ndata:{e['data'].split(chr(10))[0]}\ndata:value:{i}\n\n" for i, e in enumerate(expected)).encode()
    d, got, pos = m.Decoder(), [], 0
    while pos < len(raw):
        n = rng.randrange(1, 29)
        got += d.feed(raw[pos:pos+n]); pos += n
    got += d.close()
    assert got == expected
    return checks + 13


class SSEReference:
    """Whole-prefix reference, deliberately separate from incremental candidates."""
    def __init__(self):
        self.raw = b""
        self.emitted = 0

    def feed(self, data):
        self.raw += data
        return self._events(False)

    def close(self):
        return self._events(True)

    def _events(self, final):
        import codecs
        import re
        decoder = codecs.getincrementaldecoder("utf-8-sig")()
        text = decoder.decode(self.raw, final=final)
        if not final and text.endswith("\r"):
            text = text[:-1]
        lines = re.split(r"\r\n|\r|\n", text)[:-1]
        data, event, last_id, events = [], "", "", []
        for line in lines:
            if line == "":
                if data:
                    events.append(dict(data="\n".join(data), event=event or "message", id=last_id))
                data, event = [], ""
                continue
            key, colon, value = line.partition(":")
            if value.startswith(" "):
                value = value[1:]
            if key == "data":
                data.append(value)
            elif key == "event":
                event = value
            elif key == "id" and "\0" not in value:
                last_id = value
        new = events[self.emitted:]
        self.emitted = len(events)
        return new


def ledger_oracle(state, events):
    import copy
    out = copy.deepcopy(state)
    seen = {}
    for e in events:
        if not isinstance(e, dict) or not isinstance(e.get("id"), str) or not e["id"] or type(e.get("seq")) is not int or e["seq"] < 0 or e.get("op") not in ("put", "patch", "delete"):
            raise ValueError("event")
        if (e["op"] in ("put", "patch") and not isinstance(e.get("payload"), dict)) or (e["op"] == "delete" and e.get("payload") is not None):
            raise ValueError("payload")
        key = e["id"], e["seq"]
        if key in seen and seen[key] != e:
            raise ValueError("conflict")
        seen[key] = e
    for e in sorted(seen.values(), key=lambda e: e["seq"]):
        old = out.get(e["id"])
        if old is not None and e["seq"] <= old["seq"]:
            continue
        fields = {}
        if e["op"] == "put":
            fields = copy.deepcopy(e["payload"])
        elif e["op"] == "patch":
            fields = copy.deepcopy(old["fields"]) if old and not old["deleted"] else {}
            for k, v in e["payload"].items():
                if v is None:
                    fields.pop(k, None)
                else:
                    fields[k] = copy.deepcopy(v)
        out[e["id"]] = dict(seq=e["seq"], deleted=e["op"] == "delete", fields=fields)
    return out


def check_ledger(m):
    import copy
    rng = random.Random(701)
    for _ in range(120):
        state = {"keep": dict(seq=8, deleted=False, fields={"nested": [1, {"x": 2}]}), "a": dict(seq=4, deleted=True, fields={})}
        events = []
        for seq in range(21):
            op = rng.choice(["put", "patch", "delete"])
            e = dict(id=rng.choice(["a", "b", "c"]), seq=seq, op=op)
            if op != "delete":
                e["payload"] = {rng.choice(["x", "y", "z"]): rng.choice([None, {"value": [seq]}, seq])}
            events.append(e)
        events += copy.deepcopy(events[:4])
        rng.shuffle(events)
        before = copy.deepcopy((state, events))
        want = ledger_oracle(state, events)
        got = m.apply_batch(state, events)
        assert got == want, (got, want)
        assert (state, events) == before, "input mutated"
        got["keep"]["fields"]["nested"][1]["x"] = 99
        assert (state, events) == before, "output aliases input"
    bads = [dict(id="", seq=1, op="delete"), dict(id="a", seq=True, op="put", payload={}), dict(id="a", seq=-1, op="delete"), dict(id="a", seq=1, op="patch", payload=[]), dict(id="a", seq=1, op="delete", payload={}), dict(id="a", seq=1, op="other")]
    state = {"a": dict(seq=9, deleted=False, fields={"n": [1]})}
    for bad in bads:
        batch = [dict(id="b", seq=20, op="put", payload={"ok": [1]}), bad]
        before = copy.deepcopy((state, batch))
        try:
            m.apply_batch(state, batch)
        except ValueError:
            pass
        else:
            raise AssertionError("invalid stale input accepted")
        assert (state, batch) == before
    for seq in [1, 10]:
        batch = [dict(id="a", seq=seq, op="put", payload={"x": 1}), dict(id="a", seq=seq, op="delete")]
        try:
            m.apply_batch(state, batch)
        except ValueError:
            pass
        else:
            raise AssertionError("conflicting duplicate accepted")
    out = m.apply_batch(state, [])
    out["a"]["fields"]["n"].append(2)
    assert state["a"]["fields"]["n"] == [1]
    return 129


def policy_oracle(r):
    p = POLICY
    if r["tenant"] != "ALDER":
        return dict(pool=p["external_pool"], timeout_ms=p["external_timeout"], audit=False)
    blocked = r["path"] == p["blocked_path"] or r["path"].startswith(p["blocked_path"] + "/")
    if blocked:
        pool, timeout = p["blocked_pool"], p["blocked_timeout"]
    elif r["bytes"] >= p["bulk_threshold"]:
        pool, timeout = p["bulk_pool"], p["slow_timeout"]
    else:
        pool, timeout = p["vip_pool"] if r["premium"] else p["default_pool"], p["fast_timeout"]
    return dict(pool=pool, timeout_ms=timeout, audit=blocked or r["bytes"] >= p["audit_threshold"])


def check_context(m):
    import copy
    n = 0
    for tenant in ["ALDER", "alder", "OTHER"]:
        for path in ["/", "/private-v3", "/private-v3/", "/private-v3/doc", "/private-v30", "/PRIVATE-V3", "/private-v3%2fdoc"]:
            for size in [0, 8192, 8193, 8194, 65536, 65537, 65538, 1000000]:
                for premium in [False, True]:
                    r = dict(tenant=tenant, path=path, bytes=size, premium=premium)
                    before = copy.deepcopy(r)
                    assert m.decide(r) == policy_oracle(r), r
                    assert r == before, "request mutated"
                    n += 1
    return n


def verify(task, workspace):
    file = {"sse": "sse.py", "ledger": "ledger.py", "context": "policy.py"}[task]
    m = module(workspace / file)
    checks = {"sse": check_sse, "ledger": check_ledger, "context": check_context}[task](m)
    print(json.dumps(dict(task=task, checks=checks, passed=True)))


def prepare(out):
    out.mkdir(parents=True, exist_ok=False)
    grader = out / "grader.py"
    grader.write_bytes(Path(__file__).read_bytes())
    tasks = []
    for task, spec, starter in [
        ("sse", SSE_SPEC, "class Decoder:\n    def feed(self, data):\n        return []\n    def close(self):\n        return []\n"),
        ("ledger", LEDGER_SPEC, "def apply_batch(state, events):\n    return state.copy()\n"),
        ("context", CONTEXT_SPEC, "def decide(request):\n    return dict(pool='default', timeout_ms=1000, audit=False)\n"),
    ]:
        repo = out / task
        repo.mkdir()
        file = {"sse": "sse.py", "ledger": "ledger.py", "context": "policy.py"}[task]
        (repo / file).write_text(starter)
        (repo / "README.md").write_text(spec)
        (repo / ".gitignore").write_text("__pycache__/\n*.pyc\n")
        subprocess.run(["git", "init", "-q", str(repo)], check=True)
        subprocess.run(["git", "-C", str(repo), "add", "."], check=True)
        subprocess.run(["git", "-C", str(repo), "-c", "user.name=Benchmark", "-c", "user.email=bench@localhost", "commit", "-qm", "Frozen pilot starter"], check=True)
        base = subprocess.check_output(["git", "-C", str(repo), "rev-parse", "HEAD"], text=True).strip()
        try:
            verify(task, repo)
        except AssertionError:
            pass
        else:
            raise AssertionError(f"{task} broken starter unexpectedly passes")
        prompt = spec + ("\nPolicy dossier (JSONL):\n" + dossier() if task == "context" else "")
        tasks.append(dict(id=task, repo=str(repo), base=base, prompt=prompt, verifier=dict(run=[sys.executable, str(grader), "verify", task], timeout="30s")))
    # JSON is a YAML subset, so no extra Python dependency is needed.
    manifest = dict(version=1, name="gpt-harness-pilot-v1", tasks=tasks)
    text = json.dumps(manifest, indent=2)
    (out / "manifest.yaml").write_text(text)
    # Check independent oracle behavior as well as broken-starter rejection.
    from types import SimpleNamespace
    check_sse(SimpleNamespace(Decoder=SSEReference))
    check_ledger(SimpleNamespace(apply_batch=ledger_oracle))
    check_context(SimpleNamespace(decide=policy_oracle))
    validation = dict(broken_starters_rejected=3, references_passed=3, context_chars=len(tasks[-1]["prompt"]), manifest_sha256=hashlib.sha256(text.encode()).hexdigest(), grader_sha256=hashlib.sha256(grader.read_bytes()).hexdigest())
    (out / "validation.json").write_text(json.dumps(validation, indent=2) + "\n")
    print(json.dumps(validation))


if __name__ == "__main__":
    if sys.argv[1] == "verify":
        verify(sys.argv[2], Path.cwd())
    elif sys.argv[1] == "prepare":
        prepare(Path(sys.argv[2]).resolve())
    else:
        raise SystemExit("usage: fixtures.py prepare OUTPUT | verify TASK")
