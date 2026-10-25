"""Offline contracts for the operator-driven PTY harness; never starts a CLI."""

import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


spec = importlib.util.spec_from_file_location("live_dynamic", Path(__file__).with_name("run.py"))
dynamic = importlib.util.module_from_spec(spec)
spec.loader.exec_module(dynamic)
live = dynamic.load_legacy()


class StorageTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.workspace = self.root / "workspace"
        self.workspace.mkdir()
        self.app = dynamic.App.__new__(dynamic.App)
        self.app.root = self.root
        self.app.scenarios = {"ask": {"workspace": self.workspace}}
        self.config = {
            "session": {"store_path": str(self.root / "sessions")},
            "history": {"path": str(self.root / "history")},
            "memory": {"store_path": str(self.root / "memory")},
            "production": {"audit": {"path": str(self.root / "audit")}},
            "model_registry": {"enabled": False, "cache_path": str(self.root / "cache" / "models.json")},
        }

    def save(self):
        (self.workspace / ".go-cli.json").write_text(json.dumps(self.config))

    def test_explicit_task_storage_accepts_default_enabled_audit(self):
        self.save()
        self.app.check_storage("ask")

    def test_missing_or_relative_main_storage_rejected(self):
        original = copy.deepcopy(self.config)
        for section, key in (("session", "store_path"), ("history", "path"), ("memory", "store_path")):
            for value in (None, "relative"):
                with self.subTest(section=section, value=value):
                    self.config = copy.deepcopy(original)
                    self.config[section][key] = value
                    self.save()
                    with self.assertRaises(ValueError):
                        self.app.check_storage("ask")

    def test_missing_or_escaping_default_enabled_audit_rejected(self):
        for value in (None, str(self.root.parent / "unowned-audit")):
            with self.subTest(value=value):
                self.config["production"]["audit"]["path"] = value
                self.save()
                with self.assertRaises(ValueError):
                    self.app.check_storage("ask")

    def test_explicit_disabled_audit_is_preserved_by_prepare(self):
        self.config["production"]["audit"] = {"enabled": False}
        self.save()
        self.app.prepare()
        prepared = json.loads((self.workspace / ".go-cli.json").read_text())
        self.assertFalse(prepared["production"]["audit"]["enabled"])
        self.assertEqual(prepared["model_registry"]["cache_path"], str(self.root / "cache" / "models.dev.json"))
        self.app.check_storage("ask")

    def test_session_storage_file_rejected(self):
        (self.root / "sessions").write_text("not a directory")
        self.save()
        with self.assertRaisesRegex(ValueError, "must be a directory"):
            self.app.check_storage("ask")

    def test_shadow_yaml_rejected(self):
        self.save()
        for name in (".go-cli.yaml", ".go-cli.yml"):
            with self.subTest(name=name):
                (self.workspace / name).write_text("{}")
                with self.assertRaisesRegex(ValueError, "shadow"):
                    self.app.check_storage("ask")
                (self.workspace / name).unlink()

    def test_missing_registry_cache_rejected_even_when_locally_disabled(self):
        # A false scalar may not clear an inherited true config value.
        del self.config["model_registry"]["cache_path"]
        self.save()
        with self.assertRaisesRegex(ValueError, "model_registry.cache_path"):
            self.app.check_storage("ask")

    def test_escaping_registry_cache_rejected(self):
        self.config["model_registry"]["cache_path"] = str(self.root.parent / "unowned-cache")
        self.save()
        with self.assertRaises(ValueError):
            self.app.check_storage("ask")


class MetadataTests(unittest.TestCase):
    def body(self):
        return {"model": "codex/gpt-6.1-sol", "stream": True, "messages": [], "tools": [
            {"type": "function", "function": {"name": "read", "description": "private fixture description", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}}}},
            {"type": "function", "function": {"name": "tool_search", "parameters": {"type": "object"}}},
        ]}

    def test_request_catalog_projects_names_and_hashes_without_schema_body(self):
        body = self.body()
        metadata = live.request_metadata(json.dumps(body).encode())
        self.assertEqual([row["name"] for row in metadata["advertisedTools"]], ["read", "tool_search"])
        self.assertTrue(all(len(row["schemaSha256"]) == 64 for row in metadata["advertisedTools"]))
        self.assertEqual(len(metadata["advertisedToolsSha256"]), 64)
        self.assertNotIn("private fixture description", json.dumps(metadata))
        changed = copy.deepcopy(body)
        changed["tools"][0]["function"]["parameters"]["required"] = ["path"]
        changed_metadata = live.request_metadata(json.dumps(changed).encode())
        self.assertNotEqual(metadata["advertisedTools"][0]["schemaSha256"], changed_metadata["advertisedTools"][0]["schemaSha256"])

    def test_bridge_snapshot_retains_the_actual_advertised_catalog(self):
        bridge = live.LiveForwarder("http://127.0.0.1:1", "owned-offline-session")
        self.addCleanup(bridge.server_close)
        bridge.begin("POST", "/v1/chat/completions", json.dumps(self.body()).encode())
        row = bridge.snapshot()["requests"][0]
        self.assertEqual([tool["name"] for tool in row["advertisedTools"]], ["read", "tool_search"])
        self.assertEqual(len(row["advertisedToolsSha256"]), 64)

    def test_correlated_projection_keeps_catalog_and_decision_attribution(self):
        metadata = live.request_metadata(json.dumps(self.body()).encode())
        receipt = {"toolCallId": "write-1", "tool": "write", "decisionSource": "user", "decisionChoice": "deny"}
        request = {"sequence": 1, "method": "POST", "path": "/v1/chat/completions", "toolReceipts": [receipt],
                   "advertisedTools": metadata.get("advertisedTools"), "advertisedToolsSha256": metadata.get("advertisedToolsSha256")}
        row = live.correlate_provider_evidence({"sessionId": "owned", "requests": [request]}, {"usage": []})["inference"][0]
        self.assertEqual(row["toolReceipts"][0]["decisionSource"], "user")
        self.assertEqual(row["toolReceipts"][0]["decisionChoice"], "deny")
        self.assertEqual(row["advertisedTools"], metadata["advertisedTools"])
        self.assertEqual(row["advertisedToolsSha256"], metadata["advertisedToolsSha256"])

    def test_private_literal_check_decodes_json_escapes_without_exposing_values(self):
        body = self.body()
        body["messages"] = [{"role": "user", "content": "PRIVATE_SELECTED_LITERAL"}]
        encoded = json.dumps(body).replace("PRIVATE_SELECTED_LITERAL", r"\u0050RIVATE_SELECTED_LITERAL").encode()
        metadata = live.request_metadata(encoded, expected_fixture_facts={"mcp-command": "PRIVATE_SELECTED_LITERAL", "mcp-env": "PRIVATE_ABSENT_LITERAL"})
        self.assertTrue(metadata["fixtureFactsChecked"])
        self.assertEqual(metadata["fixtureFactsPresent"], ["mcp-command"])
        self.assertNotIn("PRIVATE_SELECTED_LITERAL", json.dumps(metadata))
        self.assertNotIn("PRIVATE_ABSENT_LITERAL", json.dumps(metadata))

    def test_private_literal_check_covers_schema_and_reports_absence_only_if_armed(self):
        body = self.body()
        body["tools"][0]["function"]["description"] = "PRIVATE_SCHEMA_LITERAL"
        metadata = live.request_metadata(json.dumps(body).encode(), expected_fixture_facts={"mcp-env": "PRIVATE_SCHEMA_LITERAL"})
        self.assertEqual(metadata["fixtureFactsPresent"], ["mcp-env"])
        absent = live.request_metadata(json.dumps(self.body()).encode(), expected_fixture_facts={"mcp-env": "PRIVATE_SCHEMA_LITERAL"})
        self.assertTrue(absent["fixtureFactsChecked"])
        self.assertEqual(absent["fixtureFactsPresent"], [])
        unarmed = live.request_metadata(json.dumps(self.body()).encode())
        self.assertFalse(unarmed["fixtureFactsChecked"])
        self.assertEqual(unarmed["fixtureFactsPresent"], [])

    def test_private_literal_evidence_survives_bridge_and_correlated_projection(self):
        bridge = live.LiveForwarder("http://127.0.0.1:1", "owned-private-literal-session", expected_fixture_facts={"mcp-env": "PRIVATE_ABSENT_LITERAL"})
        self.addCleanup(bridge.server_close)
        bridge.begin("POST", "/v1/chat/completions", json.dumps(self.body()).encode())
        snapshot = bridge.snapshot()
        row = snapshot["requests"][0]
        self.assertTrue(row["fixtureFactsChecked"])
        self.assertEqual(row["fixtureFactsPresent"], [])
        projected = live.correlate_provider_evidence(snapshot, {"usage": []})["inference"][0]
        self.assertTrue(projected["fixtureFactsChecked"])
        self.assertEqual(projected["fixtureFactsPresent"], [])

    def test_private_fixture_configuration_requires_only_labels_and_text(self):
        self.assertEqual(dynamic.private_fixture_facts({}), {})
        self.assertEqual(dynamic.private_fixture_facts({"private_fixture_facts": {"mcp-env": "PRIVATE_LITERAL"}}), {"mcp-env": "PRIVATE_LITERAL"})
        for invalid in ({"mcp-env": {"PRIVATE_LITERAL": True}}, {"mcp-env": ""}, ["PRIVATE_LITERAL"]):
            with self.subTest(invalid=invalid):
                with self.assertRaises(ValueError) as caught:
                    dynamic.private_fixture_facts({"private_fixture_facts": invalid})
                self.assertNotIn("PRIVATE_LITERAL", str(caught.exception))


if __name__ == "__main__":
    unittest.main()
