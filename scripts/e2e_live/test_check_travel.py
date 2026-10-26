import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("check_travel", Path(__file__).with_name("check_travel.py"))
travel = importlib.util.module_from_spec(spec)
spec.loader.exec_module(travel)


class TravelReviewTests(unittest.TestCase):
    def test_prompt_echo_cannot_pass_memory(self):
        row = {"id": "T008", "status": "completed", "prompt": "GZ_ORIGIN_CANARY=木棉青铜27", "actualAnswer": "我不记得了"}
        checks = travel.semantic_checks(row, {"people": 2, "rooms": 1}, "GZ_ORIGIN_CANARY=木棉青铜27")
        self.assertEqual(next(c["status"] for c in checks if c["name"] == "origin_canary_exact"), "FAIL")

    def test_pilot_does_not_inherit_unexecuted_party_changes(self):
        states = travel.effective_states([{"id": "T001"}, {"id": "T038"}])
        self.assertEqual(states["T038"]["people"], 2)
        self.assertEqual(states["T038"]["high"], 600)

    def test_chinese_equivalent_party_expression(self):
        self.assertEqual(travel.party_check("目前是两名成人、一间房", 2, 1)["status"], "PASS")

    def test_ambiguous_old_and_new_party_is_inconclusive(self):
        self.assertEqual(travel.party_check("原来两人，如今三人", 3, 2)["status"], "INCONCLUSIVE")

    def test_missing_answer_is_inconclusive_not_task_pass(self):
        row = {"id": "T008", "status": "completed", "actualAnswer": ""}
        checks = travel.semantic_checks(row, {"people": 2, "rooms": 1}, "GZ_ORIGIN_CANARY=木棉青铜27")
        self.assertEqual(next(c["status"] for c in checks if c["name"] == "origin_canary_exact"), "INCONCLUSIVE")

    def test_current_file_cannot_substitute_missing_version_snapshot(self):
        result = travel.ppt_review(Path("/definitely/absent/T033-presentation.pptx"), {"people": 3, "rooms": 2})
        self.assertEqual(result["status"], "INCONCLUSIVE")

    def test_controller_denial_is_not_automatic_policy(self):
        row = {"id": "T034", "status": "completed", "actualAnswer": "自动审批策略拒绝了写入", "toolCalls": [{"id": "c1", "name": "write"}], "approvals": [{"decision": "deny", "decidedAt": "now", "detailRowsFullyCovered": True, "pageScreenshots": ["approval.png"], "toolCallIds": ["c1"]}], "workspaceFiles": [], "toolReceipts": [{"toolCallId": "c1", "mentionsDenied": True}]}
        self.assertEqual(next(c["status"] for c in travel.approval_checks(row) if c["name"] == "approval_actor_narration"), "FAIL")

    def test_text_only_discussion_does_not_count_or_require_actual_approval(self):
        row = {"id": "T034", "experimentType": "paired-text-only-104", "status": "completed", "actualAnswer": "拒绝后停止写入，不绕过。", "toolCalls": []}
        self.assertEqual(travel.approval_checks(row), [])

    def test_explicit_wrong_latest_budget_fails(self):
        row = {"id": "T065", "actualAnswer": "当前2名成人1间房，酒店预算400—500元。"}
        checks = travel.cross_turn_checks(row, {"people": 2, "rooms": 1, "high": 600})
        self.assertEqual(next(c["status"] for c in checks if c["name"] == "explicit_latest_hotel_budget"), "FAIL")

    def test_transition_old_new_budget_is_not_blindly_failed(self):
        row = {"id": "T047", "actualAnswer": "酒店预算由400—600元收紧至400—500元。"}
        checks = travel.cross_turn_checks(row, {"people": 4, "rooms": 2, "high": 500})
        self.assertNotEqual(next(c["status"] for c in checks if c["name"] == "explicit_latest_hotel_budget"), "FAIL")


if __name__ == "__main__":
    unittest.main()
