"""Small, offline summary contracts; fixtures contain no task/answer content."""
import json
from pathlib import Path
import tempfile
import unittest

from summarize_trials import summarize


class SummaryContracts(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.next_id = 0

    def trial(self, arm='go', score=1, **changes):
        self.next_id += 1
        directory = self.root / str(self.next_id)
        directory.mkdir()
        result = {'task_id': 'synthetic-task', 'arm': arm,
                  'go_cli_revision': 'revision-D', 'go_cli_binary_sha256': 'binary-D',
                  'go_cli_source_patch_sha256': 'synthetic-source-diff',
                  'pi_version': '1.4.0', 'pi_lock_sha256': 'synthetic-pi-lock',
                  'node_version': 'synthetic-node', 'profile_id': 'same-profile',
                  'model': 'same-model', 'reasoning_effort': 'high',
                  'max_output_tokens': 16384, 'exit_code': 0, 'timed_out': False,
                  'instruction_sha256': 'synthetic-instruction',
                  'task_toml_sha256': 'synthetic-task-toml',
                  'elapsed_seconds': 10, 'patch_sha256': 'patch', 'image_id': 'image',
                  'usage': {'attempts': 2, 'known_input_tokens': 100,
                            'known_output_tokens': 10, 'known_cached_input_tokens': 80,
                            'known_reasoning_tokens': 3, 'unknown_usage_attempts': 0,
                            'unknown_cached_input_attempts': 0,
                            'unknown_reasoning_attempts': 0, 'cost_usd': None}}
        result.update(changes)
        grade = {'task_id': result['task_id'], 'source_trial': str(directory),
                 'source_patch_sha256': result['patch_sha256'], 'image_id': result['image_id'],
                 'score': score, 'elapsed_seconds': 4, 'clean_image': True,
                 'only_model_patch_replayed': True,
                 'official_replay_sha256': 'synthetic-replay',
                 'verifier_sha256': {'test.sh': 'synthetic-verifier'}}
        run = directory / 'result.json'
        graded = directory / 'grade.json'
        run.write_text(json.dumps(result))
        graded.write_text(json.dumps(grade))
        return run, graded

    def test_success_failure_and_walls_are_separate_without_ranking(self):
        data = summarize([self.trial(score=1), self.trial(score=0)])
        group = data['groups'][0]
        self.assertEqual(group['attempt_count'], 2)
        self.assertEqual(group['solve_diagnostic']['eligible_count'], 2)
        self.assertEqual(group['solve_diagnostic']['pass_rate'], 0.5)
        self.assertEqual(group['solve_diagnostic']['successes']['agent_wall_seconds']['sum'], 10)
        self.assertEqual(group['solve_diagnostic']['failures']['grader_wall_seconds']['sum'], 4)
        self.assertIsNone(group['all_attempts']['usage']['cost_usd']['sum'])
        self.assertNotIn('winner', data)
        self.assertIsNone(data['ranking'])

    def test_versions_and_profiles_do_not_merge_pi_ignores_bundled_go_identity(self):
        pairs = [self.trial(), self.trial(go_cli_binary_sha256='other-binary'),
                 self.trial(profile_id='other-profile'), self.trial(arm='pi'),
                 self.trial(arm='pi', go_cli_revision='irrelevant-Go-revision')]
        data = summarize(pairs)
        self.assertEqual(len(data['groups']), 4)
        self.assertEqual([g['attempt_count'] for g in data['groups'] if g['arm'] == 'pi'], [2])

    def test_missing_version_artifacts_are_unknown_and_never_merged(self):
        for changes in ({'go_cli_binary_sha256': None},
                        {'go_cli_source_patch_sha256': None},
                        {'arm': 'pi', 'pi_lock_sha256': None},
                        {'arm': 'pi', 'node_version': None}):
            with self.subTest(changes=changes):
                data = summarize([self.trial(**changes), self.trial(**changes)])
                self.assertEqual(len(data['groups']), 2)
                self.assertTrue(all(not row['version']['identity_complete'] for row in data['trials']))

    def test_environment_and_grader_assets_are_part_of_group_identity(self):
        normal = self.trial()
        image = self.trial(image_id='different-image')
        verifier = self.trial()
        grade = json.loads(verifier[1].read_text())
        grade['verifier_sha256'] = {'test.sh': 'different-verifier'}
        verifier[1].write_text(json.dumps(grade))
        replay = self.trial()
        grade = json.loads(replay[1].read_text())
        grade['official_replay_sha256'] = 'different-replay'
        replay[1].write_text(json.dumps(grade))
        self.assertEqual(len(summarize([normal, image, verifier, replay])['groups']), 4)

    def test_unknown_asset_identity_is_per_trial_not_inferred(self):
        for changes in ({'instruction_sha256': None}, {'task_toml_sha256': None},
                        {'image_id': None}):
            with self.subTest(changes=changes):
                data = summarize([self.trial(**changes), self.trial(**changes)])
                self.assertEqual(len(data['groups']), 2)
                self.assertTrue(all(not row['assets']['identity_complete'] for row in data['trials']))

    def test_unknown_detail_does_not_invalidate_known_input_output(self):
        pair = self.trial()
        run = json.loads(pair[0].read_text())
        run['usage']['unknown_cached_input_attempts'] = 1
        run['usage']['unknown_reasoning_attempts'] = 1
        pair[0].write_text(json.dumps(run))
        row = summarize([pair])['trials'][0]
        self.assertTrue(row['eligible_solve_diagnostic'])
        self.assertEqual(row['usage']['input_tokens'], 100)
        self.assertIsNone(row['usage']['cached_input_tokens'])
        self.assertIsNone(row['usage']['reasoning_tokens'])

    def test_token_total_does_not_double_count_cache_or_reasoning(self):
        pair = self.trial()
        row = summarize([pair])['trials'][0]
        self.assertEqual(row['usage']['total_tokens'], 110)
        self.assertEqual(row['usage']['known_total_tokens_lower_bound'], 110)
        self.assertEqual(row['usage']['unknown_usage_attempts'], 0)
        source = json.loads(pair[0].read_text())
        source['usage']['unknown_cached_input_attempts'] = 1
        source['usage']['unknown_reasoning_attempts'] = 1
        pair[0].write_text(json.dumps(source))
        self.assertEqual(summarize([pair])['trials'][0]['usage']['total_tokens'], 110)

    def test_partial_usage_reports_lower_bound_not_exact_total(self):
        pair = self.trial()
        source = json.loads(pair[0].read_text())
        source['usage']['unknown_usage_attempts'] = 1
        pair[0].write_text(json.dumps(source))
        data = summarize([pair])
        usage = data['trials'][0]['usage']
        self.assertIsNone(usage['total_tokens'])
        self.assertEqual(usage['known_total_tokens_lower_bound'], 110)
        self.assertEqual(usage['unknown_usage_attempts'], 1)
        self.assertEqual(data['groups'][0]['all_attempts']['usage']['known_total_tokens_lower_bound']['sum'], 110)

    def test_missing_or_partial_usage_is_unknown_and_excluded(self):
        for usage in (None, {'known_input_tokens': 100, 'known_output_tokens': 10,
                             'unknown_usage_attempts': 1, 'attempts': 2}):
            with self.subTest(usage=usage):
                row = summarize([self.trial(usage=usage)])['trials'][0]
                self.assertFalse(row['eligible_solve_diagnostic'])
                self.assertTrue(row['failure_flags']['measurement'])
                self.assertIsNone(row['usage']['input_tokens'])

    def test_excluded_attempts_keep_raw_score_and_consumption(self):
        pairs = [self.trial(stop_class='budget_stop'),
                 self.trial(infrastructure_error='SyntheticError'),
                 self.trial(stop_class='measurement_stop'),
                 self.trial(exit_code=1)]
        data = summarize(pairs)
        self.assertEqual([t['official_score'] for t in data['trials']], [1, 1, 1, 1])
        self.assertTrue(all(not t['eligible_solve_diagnostic'] for t in data['trials']))
        self.assertEqual(data['trials'][-1]['result_class'], 'runtime_unclassified')
        group = data['groups'][0]
        self.assertEqual(group['solve_diagnostic']['excluded']['count'], 4)
        self.assertEqual(group['all_attempts']['usage']['input_tokens']['sum'], 400)
        self.assertIsNone(group['solve_diagnostic']['pass_rate'])

    def test_runtime_and_common_budget_failures_stay_in_primary_denominator(self):
        pairs = [self.trial(score=0, exit_code=1),
                 self.trial(score=0, stop_class='budget_stop'),
                 self.trial(score=0, timed_out=True),
                 self.trial(score=0, stop_class='api_contract_stop'),
                 self.trial(score=0, stop_class='measurement_stop'), self.trial(score=1)]
        data = summarize(pairs)
        primary = data['groups'][0]['end_to_end_delivery']
        self.assertEqual(primary['scored_count'], 6)
        self.assertEqual(primary['failure_count'], 5)
        self.assertEqual(primary['success_count'], 1)
        self.assertEqual(primary['delivery_rate'], 1 / 6)
        self.assertEqual([r['delivery_score'] for r in data['trials']], [0, 0, 0, 0, 0, 1])

    def test_true_infra_and_ungraded_unknown_but_missing_detail_can_deliver(self):
        pair = self.trial(score=0, infrastructure_error='SyntheticError')
        run, _ = self.trial()
        detail = self.trial()
        source = json.loads(detail[0].read_text())
        source['usage']['unknown_cached_input_attempts'] = 1
        detail[0].write_text(json.dumps(source))
        rows = summarize([pair, (run, None), detail])['trials']
        self.assertEqual([r['delivery_score'] for r in rows], [None, None, 1])

    def test_missing_grade_is_not_zero_and_fixture_is_not_capability(self):
        run, _ = self.trial()
        row = summarize([(run, None)])['trials'][0]
        self.assertIsNone(row['official_score'])
        self.assertFalse(row['eligible_solve_diagnostic'])
        row = summarize([self.trial(offline_fixture=True)])['trials'][0]
        self.assertFalse(row['eligible_solve_diagnostic'])

    def test_grade_identity_mismatch_and_duplicates_rejected(self):
        for field, value in [('task_id', 'other-task'),
                             ('source_patch_sha256', 'other-patch'),
                             ('image_id', 'other-image'),
                             ('source_trial', str(self.root / 'wrong-path'))]:
            pair = self.trial()
            grade = json.loads(pair[1].read_text())
            grade[field] = value
            pair[1].write_text(json.dumps(grade))
            with self.subTest(field=field), self.assertRaises(ValueError):
                summarize([pair])
        pair = self.trial()
        with self.assertRaises(ValueError):
            summarize([pair, pair])

    def test_inputs_unchanged_and_only_safe_fields_emitted(self):
        pair = self.trial()
        run = json.loads(pair[0].read_text())
        run['private_api_key'] = 'DO-NOT-EMIT'
        run['model_answer'] = 'DO-NOT-EMIT'
        pair[0].write_text(json.dumps(run))
        before = [p.read_bytes() for p in pair]
        data = summarize([pair])
        self.assertNotIn('DO-NOT-EMIT', json.dumps(data))
        self.assertEqual([p.read_bytes() for p in pair], before)


if __name__ == '__main__':
    unittest.main()
