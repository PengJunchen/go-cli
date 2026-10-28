import json
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import run_trial


class ImagePinContract(unittest.TestCase):
    pin = 'ghcr.io/scaleapi/swe-bench_pro-v2@sha256:' + 'a' * 64
    image_id = 'sha256:' + 'b' * 64
    task_tag = 'ghcr.io/scaleapi/swe-bench_pro-v2:task-example'

    def metadata(self, **changes):
        return dict({'Id': self.image_id, 'RepoDigests': [self.pin]}, **changes)

    def test_agent_inspects_digest_pin_and_returns_actual_immutable_id(self):
        with patch.object(run_trial, 'docker', return_value=SimpleNamespace(stdout=json.dumps([self.metadata()]))) as docker:
            ref, metadata = run_trial.resolve_locked_image({'pinned_image': self.pin}, self.task_tag)
        docker.assert_called_once_with('image', 'inspect', self.pin)
        self.assertEqual(ref, self.pin)
        self.assertEqual(metadata['Id'], self.image_id)

    def test_missing_or_mutable_pin_fails_before_docker_lookup(self):
        for entry in [{}, {'pinned_image': self.task_tag}, {'pinned_image': self.pin[:-1]}]:
            with self.subTest(entry=entry), patch.object(run_trial, 'docker') as docker:
                with self.assertRaises(ValueError):
                    run_trial.resolve_locked_image(entry, self.task_tag)
                docker.assert_not_called()

    def test_wrong_repository_or_local_digest_binding_is_rejected(self):
        with patch.object(run_trial, 'docker') as docker:
            with self.assertRaises(ValueError):
                run_trial.resolve_locked_image({'pinned_image': self.pin.replace('ghcr.io/scaleapi', 'example.invalid/other')}, self.task_tag)
            docker.assert_not_called()
        for metadata in [self.metadata(RepoDigests=[]), self.metadata(Id='mutable-tag')]:
            with self.subTest(metadata=metadata), patch.object(run_trial, 'docker', return_value=SimpleNamespace(stdout=json.dumps([metadata]))):
                with self.assertRaises(ValueError):
                    run_trial.resolve_locked_image({'pinned_image': self.pin}, self.task_tag)

    def test_grader_uses_recorded_id_even_after_tag_changes(self):
        with patch.object(run_trial, 'docker', return_value=SimpleNamespace(stdout=json.dumps([self.metadata()]))) as docker:
            metadata = run_trial.resolve_recorded_image(self.image_id)
        docker.assert_called_once_with('image', 'inspect', self.image_id)
        self.assertEqual(metadata['Id'], self.image_id)
        with patch.object(run_trial, 'docker') as docker:
            with self.assertRaises(ValueError):
                run_trial.resolve_recorded_image(self.task_tag)
            docker.assert_not_called()

    def test_grader_rejects_different_actual_id(self):
        with patch.object(run_trial, 'docker', return_value=SimpleNamespace(stdout=json.dumps([self.metadata(Id='sha256:' + 'c' * 64)]))):
            with self.assertRaises(ValueError):
                run_trial.resolve_recorded_image(self.image_id)


if __name__ == '__main__':
    unittest.main()
