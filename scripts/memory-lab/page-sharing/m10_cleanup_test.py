#!/usr/bin/env python3
"""Failure contracts: an uncertain VM must never lose its live RAM source."""
import json
from pathlib import Path
import tempfile
import unittest

from m10_check import cleanup_resources


class Process:
    def __init__(self, pid):
        self.pid = pid
        self.returncode = None

    def poll(self):
        return self.returncode


class VM:
    def __init__(self, pid, fail=False, exited_on_error=False):
        self.process = Process(pid)
        self.fail = fail
        self.exited_on_error = exited_on_error
        self.attempted = False

    def close(self):
        self.attempted = True
        if self.fail:
            if self.exited_on_error:
                self.process.returncode = -15
            raise RuntimeError('injected close failure')
        self.process.returncode = -15


class Helper:
    def __init__(self, pid):
        self.process = Process(pid)
        self.closed = False

    def close(self):
        self.closed = True
        self.process.returncode = 0


class CleanupContract(unittest.TestCase):
    def test_one_unknown_mapping_preserves_source_and_checks_other_children(self):
        own = [VM(1), VM(2, fail=True), VM(3)]
        helper = Helper(4)
        with tempfile.TemporaryDirectory() as directory:
            run = Path(directory)
            receipt = cleanup_resources(own, [helper], own, [helper], run, False, True)
            self.assertTrue(all(vm.attempted for vm in own))
            self.assertFalse(helper.closed)
            self.assertTrue(receipt['quarantined'])
            self.assertFalse(receipt['all_vm_exits_confirmed'])
            self.assertEqual(json.loads((run / 'result.json').read_text())['status'], 'failed')
            self.assertEqual(json.loads((run / 'quarantine.json').read_text())['status'], 'quarantined')

    def test_non_mapping_close_error_does_not_prevent_safe_source_drain(self):
        own = [VM(1, fail=True, exited_on_error=True), VM(2)]
        helper = Helper(3)
        with tempfile.TemporaryDirectory() as directory:
            receipt = cleanup_resources(own, [helper], own, [helper], Path(directory), True, True)
            self.assertTrue(receipt['all_vm_exits_confirmed'])
            self.assertTrue(helper.closed)
            self.assertFalse(receipt['success'])
            self.assertEqual(len(receipt['errors']), 1)

    def test_partial_constructor_pid_is_not_treated_as_confirmed_exit(self):
        helper = Helper(2)
        with tempfile.TemporaryDirectory() as directory:
            run = Path(directory)
            child = run / 'constructor-failed'
            child.mkdir()
            (child / 'pid.json').write_text(json.dumps({'pid': 1}))
            receipt = cleanup_resources([], [helper], [], [helper], run, False, True)
            self.assertFalse(helper.closed)
            self.assertTrue(receipt['quarantined'])
            self.assertEqual(receipt['unknown_vm_receipts'][0]['pid'], 1)

    def test_failed_run_without_cleanup_authorization_keeps_children(self):
        vm, helper = VM(1), Helper(2)
        with tempfile.TemporaryDirectory() as directory:
            receipt = cleanup_resources([vm], [helper], [vm], [helper], Path(directory), False, False)
            self.assertFalse(vm.attempted)
            self.assertFalse(helper.closed)
            self.assertTrue(receipt['quarantined'])


if __name__ == '__main__':
    unittest.main()
