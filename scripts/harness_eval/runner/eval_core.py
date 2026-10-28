"""Frozen single-model CLI profiles; no task solving or prompt augmentation."""
MODEL = 'codex/gpt-6.1-sol'
OUTPUT_LIMIT = 16384
GO_REVISION = '5736e8828c80913fd4a32dc7b6e22eb82e5ec2b1'
PROFILE_ID = 'main-5736e88-personal-memory-off-v1'
PI_VERSION = '1.4.0'


def go_config(endpoint):
    return {'provider': {'name':'openai','base_url':endpoint},
        'model': {'name':MODEL,'max_tokens':OUTPUT_LIMIT},
        'agent': {'thinking':'high'},
        'small_model': {'model':''},
        'memory': {'enabled':False},
        'git': {'enabled':False}, 'extensions': {'enabled':False},
        'model_cycler': {'enabled':False},
        'session': {'store_path':'/logs/agent/go-session'},
        'tracing': {'enabled':True,'exporter':'jsonl','file_path':'/logs/agent/go-trace.jsonl'},
        'production': {'model_timeout':{'total':'5m','stream_idle':'2m'}}}


def pi_config(endpoint):
    return {'providers': {'eval': {'baseUrl':endpoint,'api':'openai-completions',
        'apiKey':'${EVAL_GATEWAY_KEY}','authHeader':True,
        'models':[{'id':MODEL,'reasoning':True,'input':['text'],
                   'contextWindow':500000,'maxTokens':OUTPUT_LIMIT}]}}}


def cli_argv(arm,prompt):
    if arm=='go':
        return ['/opt/eval/go-cli','prompt','--provider','openai','--model',MODEL,
                '--approve','auto','--no-sandbox','--output','json',prompt]
    if arm=='pi':
        return ['/opt/eval/node/bin/node','/opt/eval/pi/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js',
                '--print','--mode','json','--provider','eval','--model',MODEL,'--thinking','high',
                '--no-extensions','--no-skills','--no-mcp','--no-prompt-templates',
                '--no-approve','--offline',
                '--session-dir','/logs/agent/pi-sessions',prompt]
    raise ValueError('unknown arm')


def usage_totals(rows):
    result={'known_input_tokens':0,'known_output_tokens':0,'known_cached_input_tokens':0,
            'known_reasoning_tokens':0,'unknown_usage_attempts':0,'attempts':0,'cost_usd':None,
            'unknown_cached_input_attempts':0,'unknown_reasoning_attempts':0}
    for row in rows:
        if row.get('phase')!='finish':continue
        result['attempts']+=1
        usage=row.get('usage')
        if not isinstance(usage,dict) or not isinstance(usage.get('prompt_tokens'),int) or not isinstance(usage.get('completion_tokens'),int):
            result['unknown_usage_attempts']+=1
            result['unknown_cached_input_attempts']+=1
            result['unknown_reasoning_attempts']+=1
            continue
        result['known_input_tokens']+=usage['prompt_tokens']
        result['known_output_tokens']+=usage['completion_tokens']
        for field, detail, metric, unknown in [
            ('prompt_tokens_details','cached_tokens','known_cached_input_tokens','unknown_cached_input_attempts'),
            ('completion_tokens_details','reasoning_tokens','known_reasoning_tokens','unknown_reasoning_attempts')]:
            value=(usage.get(field) or {}).get(detail)
            if isinstance(value,int):result[metric]+=value
            else:result[unknown]+=1
    return result
