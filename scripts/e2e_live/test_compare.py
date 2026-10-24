import importlib.util
from pathlib import Path
import unittest
import tempfile


spec = importlib.util.spec_from_file_location("compare", Path(__file__).with_name("compare.py"))
compare = importlib.util.module_from_spec(spec)
spec.loader.exec_module(compare)


class EvidenceTests(unittest.TestCase):
    def test_resume_clones_original_store_without_appending_to_it(self):
        with tempfile.TemporaryDirectory() as root:
            source, destination = Path(root) / "old", Path(root) / "new"
            source.mkdir()
            original = b'{"actual":"durable record"}\n'
            (source / "session.jsonl").write_bytes(original)
            inventory = compare.clone_resume_store(source, destination)
            self.assertEqual(inventory[0]["sha256"], compare.live.sha256(original))
            with (destination / "session.jsonl").open("ab") as f:
                f.write(b'{"resumed":true}\n')
            self.assertEqual((source / "session.jsonl").read_bytes(), original)

    def test_independent_task_review_requires_actual_answer_hash(self):
        row = {"id": "T1", "actualAnswer": "real answer"}
        review = {"arms": {"main": {"T1": {"taskStatus": "PASS", "actualAnswerSha256": compare.prompt_hash("other answer")}}}}
        self.assertEqual(compare.bound_task_review(review, "main", row)["taskStatus"], "INCONCLUSIVE")
        review["arms"]["main"]["T1"]["actualAnswerSha256"] = compare.prompt_hash("real answer")
        self.assertEqual(compare.bound_task_review(review, "main", row)["taskStatus"], "PASS")
    def test_failed_attempt_is_not_continuous_success(self):
        rows = [{"status": "completed", "elapsedSeconds": 2}, {"status": "terminal_error", "elapsedSeconds": 1}, {"status": "completed", "elapsedSeconds": 4}]
        result = compare.summarize(rows)
        self.assertEqual(result["attempted"], 3)
        self.assertEqual(result["completed"], 2)
        self.assertEqual(result["longestContinuousSuccess"], 1)
        self.assertEqual(result["completedSecondsMedian"], 3)

    def test_pair_excludes_fast_failures(self):
        a = [{"id": "T1", "status": "completed", "elapsedSeconds": 5}, {"id": "T2", "status": "terminal_error", "elapsedSeconds": 1}]
        b = [{"id": "T1", "status": "completed", "elapsedSeconds": 4}, {"id": "T2", "status": "completed", "elapsedSeconds": 8}]
        self.assertEqual(compare.paired_speed(a, b)["successfulPairs"], 1)
        self.assertEqual(compare.paired_speed(a, b)["developMinusMainMedianSeconds"], 1)

    def test_headline_speed_excludes_tools_and_retries(self):
        def row(label, duration, tools=None, requests=None):
            return {"id": label, "status": "completed", "elapsedSeconds": duration, "kind": "short-text", "toolCalls": tools or [], "requestSequences": requests or [1]}
        a = [row("T1", 5), row("T2", 20, [{"name": "read"}]), row("T3", 30, requests=[1, 2])]
        b = [row("T1", 4), row("T2", 10), row("T3", 5)]
        result = compare.paired_speed(a, b)
        self.assertEqual(result["successfulPairs"], 3)
        self.assertEqual(result["pureShortTextSuccessfulPairs"], 1)
        self.assertEqual(result["pureShortTextDevelopMinusMainMedianSeconds"], 1)

    def test_receipts_do_not_inherit_previous_turn_success(self):
        rows = [{"toolCalls": [{"id": "new", "name": "write"}], "toolReceipts": [{"toolCallId": "old", "mentionsDenied": True}]}]
        self.assertEqual(compare.new_receipts(rows), [])

    def test_same_prompt_delivered_without_whitespace_rewriting(self):
        prompt = "广州  行程\n附加要求"
        self.assertEqual(compare.prompt_hash(prompt), compare.live.sha256(prompt.encode("utf-8")))

    def test_nonstream_request_is_not_final_user_answer(self):
        rows = [{"stream": False, "assistantText": "internal summary"}, {"stream": True, "assistantText": "actual answer"}]
        self.assertEqual(compare.actual_answer(rows), "actual answer")

    def test_unexecuted_rounds_are_not_failed_attempts(self):
        rows = [{"status": "completed", "elapsedSeconds": 2}, {"status": "process_exit", "elapsedSeconds": 3}, {"status": "not_run", "elapsedSeconds": None}]
        result = compare.summarize(rows)
        self.assertEqual((result["attempted"], result["completed"], result["failed"], result["notRun"]), (2, 1, 1, 1))

    def test_marker_is_classified_from_latest_user_only(self):
        bridge = compare.TimedForwarder("http://127.0.0.1:1", "test")
        try:
            bridge.label_turn("T002", ["TURN_GZ_T002"], ["TURN_GZ_T002"])
            body = compare.json.dumps({"stream": True, "messages": [{"role": "user", "content": "TURN_GZ_T001 old"}, {"role": "user", "content": "TURN_GZ_T002 new"}]}).encode()
            index, _ = bridge.begin("POST", "/v1/chat/completions", body)
            row = bridge.snapshot()["requests"][index - 1]
            self.assertEqual(row["inputMarkerMatches"], ["TURN_GZ_T002"])
            self.assertEqual(row["requestKind"], "interactive-user-turn")
        finally:
            bridge.server_close()


if __name__ == "__main__":
    unittest.main()
