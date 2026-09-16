#!/usr/bin/env python3
"""Three-turn durable queue scenario with cumulative, external grading."""
import concurrent.futures
import importlib.util
import json
import sqlite3
import subprocess
import sys
import tempfile
import threading
from pathlib import Path

FIRST = """Build a durable work queue in workqueue.py using only Python stdlib.
Implement Queue(path) backed by SQLite, with methods:
enqueue(key, payload, priority=0, available_at=0) -> integer job ID;
claim(worker, now, lease_seconds) -> job dict or None;
ack(job_id, worker, token, now) -> bool;
inspect(job_id) -> job dict or None; close() -> None.
The key is a nonempty string. Payload is any JSON-serializable value. Duplicate
enqueue keys return the existing ID without altering its original payload,
priority or availability, even after completion. Returned dicts contain id,
key, payload (decoded JSON), priority, available_at, status, attempts,
lease_until, worker, token. New jobs have status='pending', attempts=0, and
lease_until/worker/token=None. Claims choose eligible pending jobs (available_at
<= now) by priority descending, then ID ascending. A claim atomically sets
status='leased', increments attempts, and assigns worker, a fresh nonempty
opaque token, and lease_until=now+lease_seconds. Ack succeeds only for a matching
leased ID/worker/token and now < lease_until, marking done and clearing all
lease fields. Return False otherwise, without modifying the row. Repeated ack
is False. lease_seconds must be >0; invalid key or duration raises ValueError.
All times are explicit numeric arguments: never substitute wall-clock time.
State must survive closing/reopening and must work with multiple Queue objects
on the same DB. No duplicate successful claims across concurrent workers.
Do not mutate input payloads or expose mutable internal state. Include tests.
For this first step you need not reclaim expired leases; follow-ups will add
recovery and batch operations. Preserve this public API throughout.
"""

SECOND = """Extend the existing queue with crash recovery while preserving all
earlier behavior. Queue(path, max_attempts=3) now accepts a positive integer
retry limit (bool is invalid). claim() must also reclaim expired leases where
lease_until <= now, assigning a NEW token so the old owner cannot ack/renew
even if it has the same worker name. Claims may use pending jobs or expired
leases, with the original priority/ID order and availability constraint.
If an eligible pending job or expired lease already has attempts >= max_attempts,
mark it dead, clear lease fields, and keep scanning; never return it again.
Add renew(job_id, worker, token, now, lease_seconds)->bool: for a live matching
lease only, set lease_until=now+lease_seconds without changing attempts/token.
Add release(job_id, worker, token, now, retry_at)->bool: live matching lease only;
set status pending, available_at=retry_at, clear lease fields, retain attempts.
Add cancel(job_id)->bool: pending or leased becomes cancelled and clears lease
fields; terminal or missing IDs return False. Terminal means done/dead/cancelled.
Fresh Queue objects after a crash must see the same state. Use atomic SQLite
transactions; validate duration/limit before writes. Check expiry equality and
stale-token races. Keep prior requirements even when not repeated here.
"""

THIRD = """Final change: add atomic batches and compatibility with persisted v1
databases. Preserve all previous single-job and recovery behavior.
enqueue_many(items)->list[int], where items is a list of dicts with key,payload
and optional priority=0, available_at=0. Preserve input order in returned IDs.
Validate/serialize EVERY item before any insert. Any invalid key or non-JSON
payload raises ValueError and inserts nothing, including when that key already
exists. Duplicate keys (existing or within a batch) return the first row's ID
and never overwrite it. Single enqueue follows the same validation behavior.
claim_many(worker, now, lease_seconds, limit)->list[dict] atomically claims up
to limit eligible jobs in exactly the same order and with the same recovery
rules as claim(). limit must be a nonnegative integer (bool invalid); 0 returns
[]. Invalid limit/duration raises ValueError without writes. Concurrent batch
workers must never receive the same lease. Add terminal rows neither to batches
nor to claims. Add useful regression tests.
Legacy v1 schema (PRAGMA user_version=1) is exactly:
CREATE TABLE jobs(id INTEGER PRIMARY KEY AUTOINCREMENT, key TEXT NOT NULL UNIQUE,
payload TEXT NOT NULL, priority INTEGER NOT NULL DEFAULT 0,
available_at REAL NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'pending');
payload is JSON text; v1 rows have status pending or done. Opening such a DB
must migrate it transactionally without dropping IDs, dedupe keys, payloads,
priority, availability, or status. Backfill attempts=0 and null lease fields.
Opening it again must be safe. Also keep working with DBs made by your earlier
implementation. Test migration with existing nonconsecutive IDs and reopen.
"""


def load(path):
    spec = importlib.util.spec_from_file_location("candidate_queue", path)
    m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
    return m.Queue


def check(Queue, phase, root):
    groups = 0
    def fresh(name, **kw):
        return Queue(str(root / (name + ".db")), **kw)
    q = fresh("ordering")
    payload = {"x": [1, 2]}
    a = q.enqueue("a", payload, priority=2, available_at=4)
    b = q.enqueue("b", [3], priority=7, available_at=5)
    c = q.enqueue("c", "text", priority=7, available_at=5)
    payload["x"].append(99)
    assert q.enqueue("a", {"bad": 1}, priority=99) == a
    assert q.inspect(a)["payload"] == {"x": [1, 2]}
    assert q.claim("w", 3, 10) is None
    x = q.claim("w", 5, 10)
    assert x["id"] == b and x["attempts"] == 1 and x["lease_until"] == 15
    assert isinstance(x["token"], str) and x["token"]
    assert q.claim("other", 5, 10)["id"] == c
    assert q.claim("other", 5, 10)["id"] == a
    assert q.ack(b, "bad", x["token"], 6) is False
    assert q.ack(b, "w", "wrong", 6) is False
    assert q.ack(b, "w", x["token"], 15) is False
    assert q.ack(b, "w", x["token"], 14) is True
    assert q.ack(b, "w", x["token"], 14) is False
    q.close(); q = fresh("ordering")
    assert q.inspect(b)["status"] == "done"
    assert all(q.inspect(b)[k] is None for k in ("worker", "token", "lease_until"))
    assert q.enqueue("b", None) == b
    assert q.inspect(99999) is None
    q.close(); groups += 1

    q = fresh("validation")
    for key in ("", None, 4):
        try: q.enqueue(key, {})
        except ValueError: pass
        else: raise AssertionError("bad key accepted")
    job = q.enqueue("ok", {})
    for duration in (0, -1):
        try: q.claim("w", 0, duration)
        except ValueError: pass
        else: raise AssertionError("bad duration accepted")
    assert q.inspect(job)["attempts"] == 0
    q.close(); groups += 1

    # Separate connections coordinate through SQLite, not a per-object lock.
    path = root / "concurrent.db"
    q = Queue(str(path))
    ids = {q.enqueue(f"job-{i}", i) for i in range(40)}; q.close()
    barrier = threading.Barrier(4)
    def worker(n):
        q = Queue(str(path)); got = []; barrier.wait()
        while True:
            x = q.claim(str(n), 1, 100)
            if x is None: break
            got.append(x["id"])
            assert q.ack(x["id"], str(n), x["token"], 2)
        q.close(); return got
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        got = sum(pool.map(worker, range(4)), [])
    assert len(got) == len(set(got)) == 40 and set(got) == ids
    groups += 1

    if phase >= 2:
        q = fresh("recovery", max_attempts=2)
        jid = q.enqueue("crash", {"nested": [1]})
        old = q.claim("same-worker", 10, 5); q.close()
        q = fresh("recovery", max_attempts=2)
        assert q.claim("new", 14, 5) is None
        new = q.claim("same-worker", 15, 5)
        assert new["id"] == jid and new["attempts"] == 2 and new["token"] != old["token"]
        assert not q.ack(jid, "same-worker", old["token"], 16)
        assert not q.renew(jid, "same-worker", old["token"], 16, 100)
        assert q.renew(jid, "same-worker", new["token"], 16, 9)
        assert q.inspect(jid)["lease_until"] == 25
        assert q.claim("new", 24, 5) is None
        following = q.enqueue("following", None)
        assert q.claim("new", 25, 5)["id"] == following
        assert q.inspect(jid)["status"] == "dead"
        assert all(q.inspect(jid)[k] is None for k in ("worker", "token", "lease_until"))
        assert not q.cancel(jid)
        q.close(); groups += 1

        q = fresh("release")
        jid = q.enqueue("r", {})
        x = q.claim("w", 0, 10)
        assert not q.release(jid, "bad", x["token"], 1, 30)
        assert not q.release(jid, "w", x["token"], 10, 30)
        assert q.release(jid, "w", x["token"], 1, 30)
        assert q.claim("w", 29, 10) is None
        new = q.claim("w", 30, 10)
        assert new["attempts"] == 2 and new["token"] != x["token"]
        assert q.cancel(jid)
        assert not q.ack(jid, "w", new["token"], 31)
        assert not q.cancel(jid) and not q.cancel(9999)
        assert q.claim("w", 100, 10) is None
        pending = q.enqueue("p", {})
        assert q.cancel(pending) and q.inspect(pending)["status"] == "cancelled"
        q.close(); groups += 1
        for limit in (0, -1, True, 1.5):
            try: fresh("badlimit", max_attempts=limit)
            except ValueError: pass
            else: raise AssertionError("bad retry limit accepted")
        groups += 1

    if phase >= 3:
        q = fresh("batch")
        old = q.enqueue("old", {"keep": True})
        for items in ([{"key": "new", "payload": 1}, {"key": "", "payload": 2}], [{"key": "new", "payload": 1}, {"key": "old", "payload": {1, 2}}]):
            try: q.enqueue_many(items)
            except ValueError: pass
            else: raise AssertionError("invalid batch accepted")
            # A leaked first insert would be claimable after old.
            before = q.claim_many("audit", 0, 10, 10)
            assert [x["id"] for x in before] == [old] if items[1]["key"] == "" else before == []
        result = q.enqueue_many([{"key": "x", "payload": 1, "priority": 4}, {"key": "old", "payload": "ignored"}, {"key": "x", "payload": 99}, {"key": "y", "payload": 2, "priority": 8}])
        assert result[1] == old and result[0] == result[2]
        assert q.inspect(result[0])["payload"] == 1
        assert q.inspect(old)["payload"] == {"keep": True}
        assert q.claim_many("w", 0, 10, 0) == []
        for limit in (-1, True, 1.5):
            try: q.claim_many("w", 0, 10, limit)
            except ValueError: pass
            else: raise AssertionError("bad batch limit accepted")
        got = q.claim_many("w", 0, 10, 2)
        assert [x["id"] for x in got] == [result[3], result[0]]
        q.close(); groups += 1

        path = root / "batchrace.db"; q = Queue(str(path))
        ids = set(q.enqueue_many([{"key": f"b{i}", "payload": i} for i in range(31)])); q.close()
        barrier = threading.Barrier(3)
        def batchworker(i):
            q = Queue(str(path)); barrier.wait(); got=[]
            while True:
                batch = q.claim_many(str(i), 0, 50, 4)
                if not batch: break
                got.extend(x["id"] for x in batch)
            q.close(); return got
        with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
            got = sum(pool.map(batchworker, range(3)), [])
        assert len(got) == len(set(got)) == 31 and set(got) == ids
        groups += 1

        path = root / "legacy.db"
        c = sqlite3.connect(path)
        c.executescript("PRAGMA user_version=1;CREATE TABLE jobs(id INTEGER PRIMARY KEY AUTOINCREMENT,key TEXT NOT NULL UNIQUE,payload TEXT NOT NULL,priority INTEGER NOT NULL DEFAULT 0,available_at REAL NOT NULL DEFAULT 0,status TEXT NOT NULL DEFAULT 'pending');")
        c.executemany("INSERT INTO jobs VALUES(?,?,?,?,?,?)", [(7, "legacy-done", '{"keep":[1]}', 9, 0, "done"), (23, "legacy-future", '"future"', 3, 100, "pending"), (42, "legacy-ready", 'null', 4, 0, "pending")]);c.commit();c.close()
        q = Queue(str(path))
        assert q.enqueue("legacy-done", "ignored") == 7
        assert q.inspect(7)["payload"] == {"keep": [1]} and q.inspect(7)["status"] == "done"
        assert q.inspect(23)["attempts"] == 0 and q.inspect(23)["available_at"] == 100
        got = q.claim_many("w", 0, 10, 10)
        assert [x["id"] for x in got] == [42]
        assert q.enqueue("after", {}) > 42
        q.close();q = Queue(str(path));assert q.inspect(42)["status"] == "leased";q.close()
        groups += 1
    return groups


class ReferenceQueue:
    """Small reference used to validate graders before model execution."""
    def __init__(self, path, max_attempts=3):
        if type(max_attempts) is not int or max_attempts <= 0: raise ValueError("max_attempts")
        self.limit=max_attempts;self.c=sqlite3.connect(path, timeout=20);self.c.row_factory=sqlite3.Row
        self.c.execute("BEGIN IMMEDIATE")
        self.c.execute("CREATE TABLE IF NOT EXISTS jobs(id INTEGER PRIMARY KEY AUTOINCREMENT,key TEXT NOT NULL UNIQUE,payload TEXT NOT NULL,priority INTEGER NOT NULL DEFAULT 0,available_at REAL NOT NULL DEFAULT 0,status TEXT NOT NULL DEFAULT 'pending')")
        columns={r[1] for r in self.c.execute("PRAGMA table_info(jobs)")}
        for name,typ in [('attempts','INTEGER NOT NULL DEFAULT 0'),('lease_until','REAL'),('worker','TEXT'),('token','TEXT')]:
            if name not in columns:self.c.execute(f"ALTER TABLE jobs ADD COLUMN {name} {typ}")
        self.c.execute("PRAGMA user_version=2");self.c.commit()
    def close(self):self.c.close()
    def inspect(self,jid):
        r=self.c.execute("SELECT * FROM jobs WHERE id=?",(jid,)).fetchone()
        if r is None:return None
        d=dict(r);d['payload']=json.loads(d['payload']);return d
    def enqueue(self,key,payload,priority=0,available_at=0):return self.enqueue_many([dict(key=key,payload=payload,priority=priority,available_at=available_at)])[0]
    def enqueue_many(self,items):
        data=[]
        for x in items:
            if not isinstance(x.get('key'),str) or not x['key']:raise ValueError('key')
            try:raw=json.dumps(x['payload'])
            except (TypeError,ValueError):raise ValueError('payload')
            data.append((x['key'],raw,x.get('priority',0),x.get('available_at',0)))
        try:
            self.c.execute('BEGIN IMMEDIATE');ids=[]
            for row in data:
                self.c.execute('INSERT OR IGNORE INTO jobs(key,payload,priority,available_at) VALUES(?,?,?,?)',row)
                ids.append(self.c.execute('SELECT id FROM jobs WHERE key=?',(row[0],)).fetchone()[0])
            self.c.commit();return ids
        except BaseException:self.c.rollback();raise
    def claim(self,worker,now,lease_seconds):
        got=self.claim_many(worker,now,lease_seconds,1);return got[0] if got else None
    def claim_many(self,worker,now,lease_seconds,limit):
        import uuid
        if lease_seconds<=0 or type(limit) is not int or limit<0:raise ValueError('claim')
        if limit==0:return []
        try:
            self.c.execute('BEGIN IMMEDIATE');out=[]
            rows=self.c.execute("SELECT id,attempts FROM jobs WHERE available_at<=? AND (status='pending' OR (status='leased' AND lease_until<=?)) ORDER BY priority DESC,id",(now,now)).fetchall()
            for row in rows:
                if row['attempts']>=self.limit:
                    self.c.execute("UPDATE jobs SET status='dead',worker=NULL,token=NULL,lease_until=NULL WHERE id=?",(row['id'],));continue
                self.c.execute("UPDATE jobs SET status='leased',attempts=attempts+1,worker=?,token=?,lease_until=? WHERE id=?",(worker,uuid.uuid4().hex,now+lease_seconds,row['id']))
                out.append(self.inspect(row['id']))
                if len(out)==limit:break
            self.c.commit();return out
        except BaseException:self.c.rollback();raise
    def _owned(self,jid,worker,token,now,sql,args=()):
        r=self.c.execute(sql+" WHERE id=? AND status='leased' AND worker=? AND token=? AND lease_until>?",(*args,jid,worker,token,now));self.c.commit();return r.rowcount==1
    def ack(self,jid,worker,token,now):return self._owned(jid,worker,token,now,"UPDATE jobs SET status='done',worker=NULL,token=NULL,lease_until=NULL")
    def renew(self,jid,worker,token,now,lease_seconds):
        if lease_seconds<=0:raise ValueError('duration')
        return self._owned(jid,worker,token,now,"UPDATE jobs SET lease_until=?",(now+lease_seconds,))
    def release(self,jid,worker,token,now,retry_at):return self._owned(jid,worker,token,now,"UPDATE jobs SET status='pending',available_at=?,worker=NULL,token=NULL,lease_until=NULL",(retry_at,))
    def cancel(self,jid):
        r=self.c.execute("UPDATE jobs SET status='cancelled',worker=NULL,token=NULL,lease_until=NULL WHERE id=? AND status IN ('pending','leased')",(jid,));self.c.commit();return r.rowcount==1


def persisted_check(queue, phase, root):
    root.mkdir(exist_ok=True)
    q=queue(str(root/'from-first-turn.db'))
    meta=root/'expected.json'
    if phase==1:
        done=q.enqueue('original-done',{'keep':['v1',7]},priority=9)
        x=q.claim('original',0,10);assert x['id']==done
        assert q.ack(done,'original',x['token'],1)
        crash=q.enqueue('original-crash',[1,{'v':2}],priority=4)
        lease=q.claim('same-worker',2,5);assert lease['id']==crash
        future=q.enqueue('original-future',{'migration':True},priority=42,available_at=50)
        meta.write_text(json.dumps(dict(done=done,crash=crash,future=future,token=lease['token'])))
    else:
        expected=json.loads(meta.read_text())
        assert q.inspect(expected['done'])['status']=='done'
        assert q.inspect(expected['done'])['payload']=={'keep':['v1',7]}
        assert q.enqueue('original-done','do not overwrite')==expected['done']
        if phase==2:
            x=q.claim('same-worker',7,10)
            assert x['id']==expected['crash'] and x['token']!=expected['token'] and x['attempts']==2
            assert not q.ack(x['id'],'same-worker',expected['token'],8)
            assert q.ack(x['id'],'same-worker',x['token'],8)
            assert q.claim('w',49,10) is None
        else:
            assert q.inspect(expected['crash'])['status']=='done'
            row=q.inspect(expected['future'])
            assert row['payload']=={'migration':True} and row['priority']==42 and row['available_at']==50
            if row['status']=='pending':
                xs=q.claim_many('v3',50,10,9);assert [x['id'] for x in xs]==[expected['future']]
                assert q.ack(xs[0]['id'],'v3',xs[0]['token'],51)
            else:assert row['status']=='done' # final grading can safely repeat
    q.close()


def verify(phase, queue=None):
    candidate=queue is None
    queue=queue or load(Path.cwd()/'workqueue.py')
    with tempfile.TemporaryDirectory(prefix='queue-grade-') as tmp:
        groups=check(queue,phase,Path(tmp))
    if candidate:
        persisted_check(queue,phase,Path.cwd().parent/'persisted-grading-data')
        groups+=1
    print(json.dumps(dict(phase=phase,passed=True,groups=groups)))


def prepare(out):
    import hashlib
    out.mkdir(parents=True,exist_ok=False);repo=out/'repo';repo.mkdir()
    starter="class Queue:\n    def __init__(self, path, max_attempts=3):\n        pass\n    def enqueue(self, *args, **kwargs):\n        return 1\n    def inspect(self, *args):\n        return None\n"
    (repo/'workqueue.py').write_text(starter);(repo/'README.md').write_text(FIRST);(repo/'.gitignore').write_text('__pycache__/\n*.pyc\n*.db\n*.db-*\n')
    subprocess.run(['git','init','-q',str(repo)],check=True);subprocess.run(['git','-C',str(repo),'add','.'],check=True);subprocess.run(['git','-C',str(repo),'-c','user.name=Benchmark','-c','user.email=bench@localhost','commit','-qm','Durable queue starter'],check=True)
    grader=out/'grader.py';grader.write_bytes(Path(__file__).read_bytes())
    turns=[dict(prompt=p,verifier=[sys.executable,str(grader),'verify',str(i)]) for i,p in enumerate([FIRST,SECOND,THIRD],1)]
    (out/'sequence.json').write_text(json.dumps(turns,indent=2))
    manifest=dict(version=1,name='durable-queue-three-turn-v1',tasks=[dict(id='durable_queue',repo=str(repo),base=subprocess.check_output(['git','-C',str(repo),'rev-parse','HEAD'],text=True).strip(),prompt=FIRST,verifier=dict(run=turns[-1]['verifier'],timeout='1m'))])
    (out/'manifest.yaml').write_text(json.dumps(manifest,indent=2))
    for phase in (1,2,3):verify(phase,ReferenceQueue)
    with tempfile.TemporaryDirectory(prefix='queue-reference-persist-') as tmp:
        for phase in (1,2,3):persisted_check(ReferenceQueue,phase,Path(tmp)/'state')
    try:verify(1,load(repo/'workqueue.py'))
    except (AssertionError,TypeError,AttributeError):pass
    else:raise AssertionError('broken starter passed')
    (out/'validation.json').write_text(json.dumps(dict(reference_phases_passed=3,broken_starter_rejected=True,grader_sha256=hashlib.sha256(grader.read_bytes()).hexdigest()),indent=2))


if __name__=='__main__':
    if sys.argv[1]=='prepare':prepare(Path(sys.argv[2]).resolve())
    elif sys.argv[1]=='verify':verify(int(sys.argv[2]))
