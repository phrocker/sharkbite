#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Summarize isolated paired calls; billed cost is confounded by shared caching."""
import argparse
import json
from pathlib import Path
import re
from candidates import checked
from shadow import digest, write_new


def summarize(root):
    packets=checked(root/'packets.json');rows=[];seen=set()
    for packet in packets['packets']:
        key=(packet['pr'],packet['arm'])
        if key in seen or key[1] not in ('full','candidate'):raise ValueError('duplicate or invalid arm')
        seen.add(key);stem=f'pr-{key[0]}-{key[1]}'
        prompt=(root/(stem+'.txt')).read_bytes()
        call=json.loads((root/(stem+'.call.json')).read_text())
        response_bytes=(root/(stem+'.response.json')).read_bytes();response=json.loads(response_bytes)
        if digest(prompt)!=packet['prompt_sha256'] or digest(prompt)!=call['prompt_sha256']:
            raise ValueError('prompt identity mismatch')
        if call['exit_code'] or response.get('is_error') or not response.get('result'):
            raise ValueError('incomplete reviewer call')
        result=response['result'].strip()
        result=re.sub(r'^```(?:json)?\s*','',result);result=re.sub(r'\s*```$','',result)
        finding=json.loads(result)
        if not isinstance(finding['findings'],list) or not isinstance(finding['unknowns'],list):
            raise ValueError('invalid finding schema')
        usage=response['usage']
        rows.append({'pr':key[0],'arm':key[1],'prompt_bytes':len(prompt),
                     'input_tokens_including_cache':sum(usage[k] for k in ('input_tokens','cache_creation_input_tokens','cache_read_input_tokens')),
                     'output_tokens':usage['output_tokens'],'cost_usd':response['total_cost_usd'],
                     'elapsed_seconds':call['elapsed_seconds'],'findings':finding,
                     'usage':usage,'model_usage':response.get('modelUsage'), 'response_sha256':digest(response_bytes)})
    if any((pr,arm) not in seen for pr,_ in seen for arm in ('full','candidate')):raise ValueError('unpaired calls')
    fields=('prompt_bytes','input_tokens_including_cache','output_tokens','cost_usd','elapsed_seconds')
    totals={a:{k:sum(r[k] for r in rows if r['arm']==a) for k in fields} for a in ('full','candidate')}
    return {'rows':rows,'totals':totals,'input_token_reduction':1-totals['candidate']['input_tokens_including_cache']/totals['full']['input_tokens_including_cache'],
            'packet_byte_reduction':1-totals['candidate']['prompt_bytes']/totals['full']['prompt_bytes'],
            'limitations':['One stochastic call per arm per PR; findings are not defect ground truth.',
                          'Shared provider caching and variable output confound billed cost; total input includes cache reads and writes.',
                          'Elapsed sum is call duration, not parallel wall time; concurrency and service load confound speed.',
                          'Both arms contain partial source context; consult omission audit and pinned-source adjudication.'],
            'optimization_enabled':False,'packet_manifest_id':packets['id']}


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--run',type=Path,required=True);p.add_argument('--output',type=Path,required=True)
    a=p.parse_args();report=summarize(a.run);report['id']=digest(report);write_new(a.output,report)

if __name__=='__main__':main()
