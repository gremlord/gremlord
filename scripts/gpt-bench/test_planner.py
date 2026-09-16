"""Validate the external grader independently of any model-generated code."""
import copy
import unittest

from planner import ReferencePlanner, check


class GraderTests(unittest.TestCase):
    def test_reference_passes_all_cumulative_stages(self):
        for phase in (1, 2, 3):
            self.assertTrue(check(ReferencePlanner, phase)['passed'])

    def test_rejects_dependency_and_transaction_regressions(self):
        class SameWave(ReferencePlanner):
            def waves(self, targets=None, **limits):
                return [self.plan(targets)]

        class PrerequisitesDirty(ReferencePlanner):
            def affected(self, changed, targets=None):
                return self.plan(changed)

        class Nonatomic(ReferencePlanner):
            def update(self, changes):
                if not isinstance(changes, dict): raise ValueError('changes')
                for key, value in changes.items():
                    if value is None:
                        if key not in self.tasks: raise ValueError('delete')
                        del self.tasks[key]
                    else: self.tasks[key] = copy.deepcopy(value)
                ReferencePlanner(self.tasks)

        for bad, phase in [(SameWave, 2), (PrerequisitesDirty, 2), (Nonatomic, 3)]:
            with self.subTest(bad=bad.__name__), self.assertRaises(AssertionError):
                check(bad, phase)


if __name__ == '__main__': unittest.main()
