import unittest
from pathlib import Path
from grade_trial import grade_inputs

class GradeContract(unittest.TestCase):
    def test_only_patch_crosses_into_fresh_grader(self):
        task=Path('/frozen/task');source=Path('/evidence/trial')
        result=grade_inputs(task,source)
        self.assertEqual(result['patch'],source/'agent/model.patch')
        self.assertEqual(result['tests'],task/'tests')
        self.assertFalse(result['reuse_agent_container'])
        self.assertEqual(result['timeout_seconds'],3000)

if __name__=='__main__':unittest.main()
