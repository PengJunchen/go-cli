"""Offline, allow-listed summaries of run_trial result.json and clean grade.json."""
import argparse
import hashlib
import json
import math
from pathlib import Path


PROFILE_FIELDS = (
    'profile_id', 'model', 'reasoning_effort', 'max_output_tokens',
    'task_timeout_seconds', 'max_upstream_requests', 'cpus', 'memory_bytes',
    'network', 'storage_hard_limit_enforced', 'go_automatic_personal_memory',
    'compaction','audit_policy_id','audit_policy_fingerprint',
)
METRICS = ('input_tokens', 'output_tokens', 'cached_input_tokens',
           'reasoning_tokens', 'total_tokens', 'known_total_tokens_lower_bound',
           'unknown_usage_attempts', 'attempts', 'cost_usd')
AMORTIZED_METRICS = ('input_tokens', 'output_tokens', 'cached_input_tokens',
                     'reasoning_tokens', 'total_tokens', 'cost_usd')


def number(value):
    if type(value) not in (int, float) or value < 0 or not math.isfinite(value):
        return None
    return value


def count(value):
    return value if type(value) is int and value >= 0 else None


def read_record(path):
    data = Path(path).read_bytes()
    record = json.loads(data)
    if not isinstance(record, dict):
        raise ValueError('expected a JSON object')
    return record, hashlib.sha256(data).hexdigest()


def normalized_usage(record):
    source = record.get('usage')
    source = source if isinstance(source, dict) else {}
    attempts = count(source.get('attempts'))
    complete = attempts is not None and count(source.get('unknown_usage_attempts')) == 0
    independent_fields='unknown_input_attempts'in source and'unknown_output_attempts'in source
    result = {'attempts': attempts, 'cost_usd': number(source.get('cost_usd'))}
    for target, known, unknown in (
        ('input_tokens', 'known_input_tokens', 'unknown_usage_attempts'),
        ('output_tokens', 'known_output_tokens', 'unknown_usage_attempts'),
        ('cached_input_tokens', 'known_cached_input_tokens', 'unknown_cached_input_attempts'),
        ('reasoning_tokens', 'known_reasoning_tokens', 'unknown_reasoning_attempts'),
    ):
        value = count(source.get(known))
        result[known] = value  # Preserve partial observations separately.
        key=({'input_tokens':'unknown_input_attempts','output_tokens':'unknown_output_attempts'}.get(target,unknown)if independent_fields else unknown)
        field_complete=(attempts is not None and source.get('ledger_complete')is not False and count(source.get(key))==0)if independent_fields else(complete and count(source.get(unknown))==0)
        result[target] = value if field_complete else None
    result['unknown_usage_attempts'] = count(source.get('unknown_usage_attempts'))
    result['total_tokens'] = (result['input_tokens'] + result['output_tokens']
                              if result['input_tokens'] is not None and result['output_tokens'] is not None else None)
    result['known_total_tokens_lower_bound'] = (
        result['known_input_tokens'] + result['known_output_tokens']
        if result['known_input_tokens'] is not None and result['known_output_tokens'] is not None else None)
    return result


def version_identity(record, trial_id):
    arm = record['arm']
    if arm == 'go':
        keys = ('go_cli_revision', 'go_cli_source_patch_sha256', 'go_cli_binary_sha256')
    elif arm == 'pi':
        # A common payload can bundle a different Go binary without changing Pi.
        keys = ('pi_version', 'pi_lock_sha256', 'node_version')
    else:
        raise ValueError('arm must be go or pi')
    identity = {key: record.get(key) for key in keys}
    missing = [key for key in keys if not isinstance(identity[key], str) or not identity[key]]
    identity['identity_complete'] = not missing
    identity['missing_identity_fields'] = missing
    if missing:
        identity['unidentified_trial'] = trial_id  # Never merge unidentified versions.
    return identity


def asset_identity(result, grade, trial_id):
    identity = {key: result.get(key) for key in
                ('image_id', 'instruction_sha256', 'task_toml_sha256')}
    identity['grader_image_id'] = grade.get('image_id')
    identity['official_replay_sha256'] = grade.get('official_replay_sha256')
    missing = [key for key, value in identity.items() if not isinstance(value, str) or not value]
    verifier = grade.get('verifier_sha256')
    valid_verifier = (isinstance(verifier, dict) and bool(verifier) and
                      all(isinstance(key, str) and isinstance(value, str) and value
                          for key, value in verifier.items()))
    identity['verifier_sha256'] = verifier if valid_verifier else None
    if not valid_verifier:
        missing.append('verifier_sha256')
    identity['grader_profile'] = {key: grade.get(key) for key in
                                  ('verifier_timeout_seconds', 'verifier_network', 'cpus', 'memory_bytes')}
    identity['identity_complete'] = not missing
    identity['missing_identity_fields'] = missing
    if missing:
        identity['unidentified_trial'] = trial_id
    return identity


def load_trial(result_path, grade_path, trial_id):
    result, result_hash = read_record(result_path)
    if not isinstance(result.get('task_id'), str) or not result['task_id']:
        raise ValueError('missing task identity')
    grade, grade_hash = ({}, None) if grade_path is None else read_record(grade_path)
    if grade:
        for grade_key, result_key in (
            ('task_id', 'task_id'), ('source_patch_sha256', 'patch_sha256'),
        ):
            if not result.get(result_key) or grade.get(grade_key) != result[result_key]:
                raise ValueError('grade identity mismatch: ' + grade_key)
        if result.get('image_id') is not None and grade.get('image_id') is not None and result['image_id'] != grade['image_id']:
            raise ValueError('grade identity mismatch: image_id')
        if not grade.get('source_trial') or Path(grade['source_trial']).resolve() != Path(result_path).resolve().parent:
            raise ValueError('grade source_trial path mismatch')
    score = grade.get('score')
    if score is not None and (type(score) is not int or score not in (0, 1)):
        raise ValueError('official score must be binary or null')
    usage = normalized_usage(result)
    tags = {str(result.get(key) or '').lower() for key in ('failure_class', 'stop_class')}
    infrastructure = bool(result.get('infrastructure_error') or grade.get('infrastructure_error'))
    infrastructure |= result.get('artifact_collection_exit_code', 0) != 0
    budget = 'budget_stop' in tags or result.get('timed_out') is True
    measurement = ('measurement_stop' in tags or bool(result.get('open_attempts')) or
                   usage['input_tokens'] is None or usage['output_tokens'] is None)
    completion_unknown=result.get('api_completion_unknown')is True
    api_terminal=result.get('api_terminal_error')is True or 'api_terminal_error' in tags
    api_contract = ('api_contract_stop' in tags or
                    (bool(result.get('wire_contract_rejections')) and not budget and not measurement))
    infrastructure |= bool(tags & {'infrastructure_error', 'infrastructure_failure', 'infra_error'})
    runtime = result.get('native_terminal_failure')is True or bool(tags & {'harness_error', 'runtime_error', 'runtime_unclassified'})
    normal_exit = type(result.get('exit_code')) is int and result['exit_code'] == 0
    completion_unknown=completion_unknown and normal_exit and not any((infrastructure,budget,api_contract,api_terminal,runtime))
    if not normal_exit and not any((infrastructure, budget, measurement, api_contract,api_terminal)):
        runtime = True
    grade_valid = (type(score) is int and grade.get('clean_image') is True and
                   grade.get('only_model_patch_replayed') is True and
                   not grade.get('ungradable_reason') and not grade.get('infrastructure_error'))
    flags = {'infrastructure': bool(infrastructure), 'budget': budget,
             'measurement': measurement, 'api_contract': bool(api_contract),'api_terminal':api_terminal,'completion_unknown':completion_unknown,
             'runtime': runtime, 'fixture': result.get('offline_fixture') is True,
             'ungraded': score is None, 'invalid_grade': score is not None and not grade_valid}
    eligible = grade_valid and not any(flags.values()) and normal_exit
    # Runtime/cap/API failures are failures of the Harness being evaluated.
    # Missing usage alone affects measurement, not a valid completed delivery.
    if infrastructure:
        delivery, delivery_status = None, 'unknown_infrastructure'
    elif flags['fixture']:
        delivery, delivery_status = None, 'not_scored_fixture'
    elif not grade_valid:
        delivery = None
        delivery_status = 'unknown_ungraded' if score is None else 'unknown_invalid_grade'
    elif completion_unknown:
        delivery,delivery_status=None,'unknown_native_completion'
    elif not normal_exit or runtime or budget or api_contract or api_terminal or 'measurement_stop' in tags:
        delivery, delivery_status = 0, 'failed'
    else:
        delivery = score
        delivery_status = ('delivered_measurement_incomplete' if score and measurement else
                           'delivered' if score else 'failed')
    if infrastructure:
        category = 'infrastructure_failure'
    elif budget:
        category = 'budget_stop'
    elif completion_unknown:
        category='unknown_native_completion'
    elif api_terminal:
        category='api_terminal_error'
    elif runtime and result.get('native_terminal_failure')is True:
        category='native_runtime_error'
    elif measurement:
        category = 'measurement_failure'
    elif api_contract:
        category = 'api_contract_stop'
    elif runtime:
        category = 'harness_error' if 'harness_error' in tags else 'runtime_unclassified'
    elif flags['fixture']:
        category = 'offline_fixture'
    elif score is None:
        category = 'ungraded'
    elif not grade_valid:
        category = 'invalid_grade'
    else:
        category = 'success' if score else 'solve_failure'
    return {
        'trial_id': trial_id, 'task_id': result['task_id'], 'arm': result['arm'],
        'version': version_identity(result, trial_id),
        'assets': asset_identity(result, grade, trial_id),
        'profile': {key: result.get(key) for key in PROFILE_FIELDS},
        'official_score': score, 'grade_valid': grade_valid,
        'delivery_score': delivery, 'delivery_status': delivery_status,
        'eligible_solve_diagnostic': bool(eligible), 'solve_diagnostic_score': score if eligible else None,
        'result_class': category, 'failure_flags': flags,
        'exit_code': result.get('exit_code'), 'timed_out': result.get('timed_out'),
        'agent_wall_seconds': number(result.get('elapsed_seconds')),
        'grader_wall_seconds': number(grade.get('elapsed_seconds')),
        'usage': usage, 'result_sha256': result_hash, 'grade_sha256': grade_hash,
    }


def metric(values):
    known = [value for value in values if value is not None]
    complete = len(known) == len(values) and bool(values)
    return {'values': values, 'count': len(values), 'known_count': len(known),
            'known_sum': sum(known) if known else None,
            'sum': sum(known) if complete else None,
            'mean': sum(known) / len(known) if complete else None}


def bucket(rows):
    return {'count': len(rows), 'trial_ids': [row['trial_id'] for row in rows],
            'agent_wall_seconds': metric([row['agent_wall_seconds'] for row in rows]),
            'grader_wall_seconds': metric([row['grader_wall_seconds'] for row in rows]),
            'usage': {key: metric([row['usage'][key] for row in rows]) for key in METRICS}}


def summarize(pairs):
    rows = []
    seen = set()
    for index, (result, grade) in enumerate(pairs, 1):
        resolved = Path(result).resolve()
        if resolved in seen:
            raise ValueError('duplicate result input')
        seen.add(resolved)
        rows.append(load_trial(result, grade, 'trial-%04d' % index))
    grouped = {}
    for row in rows:
        identity = {key: row[key] for key in ('task_id', 'arm', 'version', 'profile', 'assets')}
        key = json.dumps(identity, sort_keys=True)
        grouped.setdefault(key, (identity, []))[1].append(row)
    groups = []
    for identity, attempts in grouped.values():
        successes = [row for row in attempts if row['solve_diagnostic_score'] == 1]
        failures = [row for row in attempts if row['solve_diagnostic_score'] == 0]
        excluded = [row for row in attempts if not row['eligible_solve_diagnostic']]
        all_attempts = bucket(attempts)
        eligible_count = len(successes) + len(failures)
        delivered = [row for row in attempts if row['delivery_score'] == 1]
        failed_delivery = [row for row in attempts if row['delivery_score'] == 0]
        unknown_delivery = [row for row in attempts if row['delivery_score'] is None]
        delivery_count = len(delivered) + len(failed_delivery)
        groups.append(identity | {
            'attempt_count': len(attempts),
            'end_to_end_delivery': {
                'scored_count': delivery_count, 'success_count': len(delivered),
                'failure_count': len(failed_delivery), 'unknown_count': len(unknown_delivery),
                'delivery_rate': len(delivered) / delivery_count if delivery_count else None,
                'successes': bucket(delivered), 'failures': bucket(failed_delivery),
                'unknown': bucket(unknown_delivery),
            },
            'solve_diagnostic': {
                'eligible_count': eligible_count,
                'pass_rate': len(successes) / eligible_count if eligible_count else None,
                'successes': bucket(successes), 'failures': bucket(failures),
                'excluded': bucket(excluded),
            },
            'all_attempts': all_attempts,
            'amortized_per_delivery': {
                key: (all_attempts['usage'][key]['sum'] / len(delivered)
                      if delivered and all_attempts['usage'][key]['sum'] is not None else None)
                for key in AMORTIZED_METRICS},
        })
    return {'schema': 'harness-trial-summary-v1', 'trials': rows, 'groups': groups,
            'ranking': None,
            'notes': ['Raw official ledger retains every attempt. Primary end_to_end_delivery counts valid graded runtime, common-budget, timeout and API/measurement-stop failures as failures.',
                      'Only invalid infrastructure, ungraded/invalid grades and offline fixtures are unknown in the primary delivery ledger; unknowns remain visible.',
                      'eligible_solve_diagnostic and clean/excluded buckets are a secondary pure-solving diagnostic, never the main Harness score or ranking.',
                      'Groups separate version, task, arm, profile, immutable task/image assets and official grader hashes. Missing asset identities are isolated per trial, never inferred; small samples are diagnostic, not a ranking.',
                      'Agent and grader time are separate. Total tokens = input + output only; cache is an input subset and reasoning is usually an output subset, never added again.',
                      'Exact total requires complete input/output usage for every request. Partial observations give known_total_tokens_lower_bound plus unknown_usage_attempts; missing detail fields do not invalidate an otherwise known total.',
                      'Unknown totals remain null; known_sum is partial observation, not a full total. Dollars are reported only, never estimated.']}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--trial', nargs=2, action='append', required=True,
                        metavar=('RESULT_JSON', 'GRADE_JSON_OR_DASH'))
    parser.add_argument('--output', type=Path, help='new JSON file; omitted prints to stdout')
    args = parser.parse_args()
    try:
        data = summarize([(Path(run), None if grade == '-' else Path(grade))
                          for run, grade in args.trial])
        serialized = json.dumps(data, ensure_ascii=False, indent=2, allow_nan=False) + '\n'
        if args.output:
            with args.output.open('x', encoding='utf-8') as output:
                output.write(serialized)
        else:
            print(serialized, end='')
    except (OSError, ValueError, TypeError, KeyError) as error:
        parser.error(str(error))


if __name__ == '__main__':
    main()
