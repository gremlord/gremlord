#!/usr/bin/env python3
"""Frozen three-turn dependency-planner workflow, separate from queue tuning."""
import copy
import hashlib
import heapq
import importlib.util
import json
from pathlib import Path
import random
import subprocess
import sys

FIRST = '''Extend planner.py into a deterministic dependency planner using Python stdlib.
Planner(tasks) accepts a dict from nonempty string task IDs to dicts containing
"deps" (a list of distinct task ID strings) and "cost" (a positive integer;
bool is invalid). Every dependency must exist. Reject malformed tasks, missing
dependencies, self-dependencies, and cycles with ValueError. Keep an independent
snapshot of input; caller mutation after construction must not change behavior.
Implement plan(targets=None)->list[str]. None selects every task; a list selects
exactly those targets and their transitive prerequisites. [] selects nothing.
Unknown targets or malformed target lists raise ValueError. Duplicate targets
are allowed. Return a topological order, repeatedly choosing the lexicographically
smallest currently ready ID. Tasks outside the selected prerequisite closure
must not appear. The empty graph is valid. Include tests. Preserve this API in
later changes; follow-ups will add resource-aware scheduling and graph updates.
'''
SECOND = '''Extend Planner while retaining all previous behavior.
Add waves(targets=None, *, max_parallel, budget)->list[list[str]]. Both limits
must be positive integers (bool invalid). Select the same prerequisite closure
as plan(). If any selected task costs more than budget, raise ValueError before
scheduling. At each wave, consider only tasks whose dependencies finished in
EARLIER waves. Order ready tasks by cost descending, then ID ascending; scan
that order, selecting tasks that fit the remaining budget until max_parallel
is reached. Skip a ready task that cannot fit and continue scanning. Never make
a dependent ready within its prerequisite's own wave. Return IDs within each
wave in selection order. Empty selection returns [], after validating limits.
Add affected(changed, targets=None)->list[str]. changed is a list of known IDs;
malformed or unknown IDs raise ValueError, even if outside the selected closure.
Return changed tasks and their transitive dependents within the selected closure,
in the order produced by plan(targets). Do not include prerequisites just because
their dependent changed. Duplicate changed IDs are allowed. Neither method may
mutate graph state. Cover budget packing, ties, dependency barriers and subsets.
'''
THIRD = '''Add transactional graph updates and portable snapshots, retaining every
previous contract. update(changes)->None accepts a dict ID->replacement task dict,
or ID->None to delete an existing task. A replacement may add a new task. Validate
the FINAL graph as a whole so a batch can add dependencies, remove dependent tasks,
and change edges in any mapping order. Deleting an unknown ID is ValueError.
Malformed changes, missing dependencies and cycles raise ValueError and leave the
entire previous graph unchanged. Deep-copy accepted changes.
Add dump()->dict with exactly version=1 and tasks=a list of {id,deps,cost} records.
Sort records by ID and dependency lists lexicographically for canonical output.
Return independent data so editing the dump cannot change the planner.
Add Planner.load(data)->Planner. Require version to be integer 1 (bool invalid),
a task-record list with unique IDs, and a graph satisfying constructor validation.
Malformed snapshots raise ValueError. Ignore extra fields in input records or the
outer snapshot; dump emits only the specified fields. The empty snapshot is valid.
Round-trip via JSON must preserve plan, waves, and affected. Test atomic rollback,
batch forward references, deletion, snapshot isolation, and deterministic output.
'''


class ReferencePlanner:
    def __init__(self, tasks):
        if not isinstance(tasks, dict):
            raise ValueError('tasks')
        self.tasks = {}
        for key, value in tasks.items():
            if not isinstance(key, str) or not key or not isinstance(value, dict):
                raise ValueError('task')
            deps, cost = value.get('deps'), value.get('cost')
            if (not isinstance(deps, list) or any(not isinstance(x, str) or not x for x in deps)
                    or len(set(deps)) != len(deps) or type(cost) is not int or cost <= 0):
                raise ValueError('task')
            self.tasks[key] = dict(deps=deps[:], cost=cost)
        if any(d not in self.tasks for t in self.tasks.values() for d in t['deps']):
            raise ValueError('missing')
        self.plan()

    def _ids(self, ids):
        if not isinstance(ids, list) or any(not isinstance(x, str) or x not in self.tasks for x in ids):
            raise ValueError('ids')
        return set(ids)

    def _selected(self, targets):
        pending = list(self.tasks if targets is None else self._ids(targets))
        selected = set()
        while pending:
            key = pending.pop()
            if key not in selected:
                selected.add(key)
                pending.extend(self.tasks[key]['deps'])
        return selected

    def plan(self, targets=None):
        selected = self._selected(targets)
        indegree = {k:len(self.tasks[k]['deps']) for k in selected}
        successors = {k:[] for k in selected}
        for k in selected:
            for dep in self.tasks[k]['deps']:
                successors[dep].append(k)
        ready = [k for k in selected if not indegree[k]]
        heapq.heapify(ready)
        result = []
        while ready:
            k = heapq.heappop(ready); result.append(k)
            for child in successors[k]:
                indegree[child] -= 1
                if not indegree[child]: heapq.heappush(ready, child)
        if len(result) != len(selected): raise ValueError('cycle')
        return result

    def waves(self, targets=None, *, max_parallel, budget):
        if type(max_parallel) is not int or max_parallel <= 0 or type(budget) is not int or budget <= 0:
            raise ValueError('limits')
        pending = self._selected(targets)
        if any(self.tasks[k]['cost'] > budget for k in pending): raise ValueError('budget')
        completed, waves = set(), []
        while pending:
            ready = sorted((k for k in pending if set(self.tasks[k]['deps']) <= completed),
                           key=lambda k:(-self.tasks[k]['cost'], k))
            wave, remaining = [], budget
            for k in ready:
                if len(wave) < max_parallel and self.tasks[k]['cost'] <= remaining:
                    wave.append(k); remaining -= self.tasks[k]['cost']
            waves.append(wave); completed.update(wave); pending.difference_update(wave)
        return waves

    def affected(self, changed, targets=None):
        dirty = self._ids(changed)
        ordered = self.plan(targets)
        for k in ordered:
            if any(d in dirty for d in self.tasks[k]['deps']): dirty.add(k)
        return [k for k in ordered if k in dirty]

    def update(self, changes):
        if not isinstance(changes, dict): raise ValueError('changes')
        proposed = copy.deepcopy(self.tasks)
        for k, value in changes.items():
            if value is None:
                if k not in proposed: raise ValueError('delete')
                del proposed[k]
            else: proposed[k] = value
        self.tasks = ReferencePlanner(proposed).tasks

    def dump(self):
        return dict(version=1, tasks=[dict(id=k, deps=sorted(v['deps']), cost=v['cost'])
                    for k,v in sorted(self.tasks.items())])

    @classmethod
    def load(cls, data):
        if (not isinstance(data, dict) or type(data.get('version')) is not int
                or data['version'] != 1 or not isinstance(data.get('tasks'), list)):
            raise ValueError('snapshot')
        tasks = {}
        for item in data['tasks']:
            if not isinstance(item, dict) or not isinstance(item.get('id'), str) or item['id'] in tasks:
                raise ValueError('record')
            tasks[item['id']] = item
        return cls(tasks)


def candidate(path):
    spec = importlib.util.spec_from_file_location('candidate_planner', path)
    module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
    return module.Planner


def rejects(fn):
    try: fn()
    except ValueError: return
    raise AssertionError('expected ValueError')


def graph():
    return {'z':{'deps':[], 'cost':4}, 'a':{'deps':[], 'cost':7},
            'b':{'deps':[], 'cost':3}, 'c':{'deps':['a','b'], 'cost':2},
            'd':{'deps':['c','z'], 'cost':5}, 'e':{'deps':['z'], 'cost':1}}


def check(Planner, phase):
    data = graph(); p = Planner(data); data['c']['deps'].clear(); data['a']['cost'] = 99
    assert p.plan() == ['a','b','c','z','d','e']
    assert p.plan(['d','d']) == ['a','b','c','z','d']
    assert p.plan(['e']) == ['z','e']
    assert p.plan([]) == [] and Planner({}).plan() == []
    for ids in ['a', [1], ['missing'], [None]]: rejects(lambda: p.plan(ids))
    malformed = [[], {'':{'deps':[], 'cost':1}}, {'x':{'deps':[], 'cost':True}},
                 {'x':{'deps':[], 'cost':0}}, {'x':{'deps':[], 'cost':1.2}},
                 {'x':{'deps':'x', 'cost':1}}, {'x':{'deps':['missing'], 'cost':1}},
                 {'x':{'deps':['x'], 'cost':1}}, {'x':{}},
                 {'x':{'deps':[['x']], 'cost':1}},
                 {'x':{'deps':['y','y'], 'cost':1}, 'y':{'deps':[], 'cost':1}},
                 {'x':{'deps':['y'], 'cost':1}, 'y':{'deps':['x'], 'cost':1}}]
    for value in malformed: rejects(lambda: Planner(value))
    if phase >= 2:
        assert p.waves(max_parallel=3,budget=10) == [['a','b'],['z','c'],['d','e']]
        assert p.waves(['e'],max_parallel=1,budget=4) == [['z'],['e']]
        assert p.waves([],max_parallel=1,budget=1) == []
        assert Planner({'a':{'deps':[],'cost':1},'b':{'deps':[],'cost':1}}).waves(max_parallel=2,budget=2) == [['a','b']]
        for limit in [0,-1,True,1.5]:
            rejects(lambda:p.waves([],max_parallel=limit,budget=10))
            rejects(lambda:p.waves([],max_parallel=2,budget=limit))
        rejects(lambda:p.waves(max_parallel=3,budget=6))
        assert p.affected(['c']) == ['c','d']
        assert p.affected(['z','z']) == ['z','d','e']
        assert p.affected(['a'],['e']) == []
        assert p.affected(['z'],['d']) == ['z','d']
        rejects(lambda:p.affected(['missing'],[]))
        rejects(lambda:p.affected('a'))
    if phase >= 3:
        before = p.dump()
        assert before == ReferencePlanner(graph()).dump()
        leaked = p.dump(); leaked['tasks'][0]['deps'].append('bad'); leaked['tasks'][0]['cost']=99
        assert p.dump() == before
        for changes in [[], {'ghost':None}, {'z':None}, {'new':{'deps':['missing'],'cost':1}},
                        {'a':{'deps':['d'],'cost':1}}, {'ok':{'deps':[],'cost':1}, 'bad':{}}]:
            rejects(lambda:p.update(changes)); assert p.dump() == before
        patch = {'new':{'deps':['later'],'cost':2}, 'later':{'deps':[],'cost':1}}
        p.update(patch); patch['later']['cost']=99; patch['new']['deps'].clear()
        assert p.plan(['new']) == ['later','new']
        p.update({'a':None,'c':{'deps':['b'],'cost':2}})
        assert 'a' not in p.plan() and p.plan(['d']) == ['b','c','z','d']
        snapshot = json.loads(json.dumps(p.dump())); restored=Planner.load(snapshot)
        snapshot['tasks'][0]['cost']=999
        assert restored.dump() == p.dump()
        assert restored.waves(max_parallel=2,budget=7) == p.waves(max_parallel=2,budget=7)
        assert restored.affected(['b']) == p.affected(['b'])
        assert Planner.load({'version':1,'tasks':[]}).dump() == {'version':1,'tasks':[]}
        for snap in [[], {}, {'version':True,'tasks':[]}, {'version':2,'tasks':[]},
                     {'version':1,'tasks':{}}, {'version':1,'tasks':[{}]},
                     {'version':1,'tasks':[{'id':'a','deps':[],'cost':1}]*2},
                     {'version':1,'tasks':[{'id':'a','deps':['a'],'cost':1}]}]: rejects(lambda:Planner.load(snap))
        assert Planner.load({'version':1,'tasks':[{'id':'x','deps':[],'cost':1,'extra':9}], 'extra':9}).dump() == {'version':1,'tasks':[{'id':'x','deps':[],'cost':1}]}
    # Fixed random DAGs cover tie order, target closure, resource packing and
    # dirty propagation without sharing reference source with the candidate.
    rng=random.Random(91016)
    for _ in range(24):
        keys=['a','b','c','d','e','f']
        data={k:dict(deps=[d for d in keys[:i] if rng.random()<.3],cost=rng.randint(1,7)) for i,k in enumerate(keys)}
        actual,expected=Planner(data),ReferencePlanner(data)
        targets=rng.sample(keys,rng.randrange(7))
        assert actual.plan(targets)==expected.plan(targets)
        if phase>=2:
            assert actual.waves(targets,max_parallel=3,budget=9)==expected.waves(targets,max_parallel=3,budget=9)
            changed=rng.sample(keys,2)
            assert actual.affected(changed,targets)==expected.affected(changed,targets)
        if phase>=3: assert Planner.load(actual.dump()).dump()==expected.dump()
    return {'phase':phase,'passed':True,'random_dags':24}


def prepare(out):
    out.mkdir(parents=True,exist_ok=False);repo=out/'repo';repo.mkdir()
    (repo/'planner.py').write_text('class Planner:\n    def __init__(self, tasks):\n        self.tasks = tasks\n    def plan(self, targets=None):\n        return sorted(self.tasks if targets is None else targets)\n')
    (repo/'README.md').write_text(FIRST);(repo/'.gitignore').write_text('__pycache__/\n*.pyc\n')
    subprocess.run(['git','init','-q',str(repo)],check=True)
    subprocess.run(['git','-C',str(repo),'add','.'],check=True)
    subprocess.run(['git','-C',str(repo),'-c','user.name=Benchmark','-c','user.email=bench@localhost','commit','-qm','Dependency planner starter'],check=True)
    grader=out/'grader.py';grader.write_bytes(Path(__file__).read_bytes())
    turns=[dict(prompt=p,verifier=[sys.executable,str(grader),'verify',str(i)]) for i,p in enumerate([FIRST,SECOND,THIRD],1)]
    (out/'sequence.json').write_text(json.dumps(turns,indent=2))
    manifest=dict(version=1,name='planner-three-turn-v1',tasks=[dict(id='dependency_planner',repo=str(repo),base=subprocess.check_output(['git','-C',str(repo),'rev-parse','HEAD'],text=True).strip(),prompt=FIRST,verifier=dict(run=turns[-1]['verifier'],timeout='1m'))])
    (out/'manifest.yaml').write_text(json.dumps(manifest,indent=2))
    for phase in (1,2,3):print(json.dumps(check(ReferencePlanner,phase)))
    try:check(candidate(repo/'planner.py'),1)
    except AssertionError:pass
    else:raise AssertionError('broken starter passed')
    (out/'validation.json').write_text(json.dumps(dict(reference_phases_passed=3,broken_starter_rejected=True,grader_sha256=hashlib.sha256(grader.read_bytes()).hexdigest()),indent=2))


if __name__=='__main__':
    if sys.argv[1]=='prepare':prepare(Path(sys.argv[2]).resolve())
    elif sys.argv[1]=='verify':print(json.dumps(check(candidate(Path.cwd()/'planner.py'),int(sys.argv[2]))))
