#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Reserve experiment budget durably before isolated paid Claude calls."""
import argparse
from decimal import Decimal, ROUND_CEILING
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import time
from candidates import checked
from shadow import digest, write_new


def micros(value):
    d=Decimal(str(value))
    if not d.is_finite() or d<0:raise ValueError('invalid reported cost')
    return int((d*1000000).to_integral_value(rounding=ROUND_CEILING))


def connect(path,protocol):
    db=sqlite3.connect(path,timeout=30,isolation_level=None)
    db.execute('CREATE TABLE IF NOT EXISTS experiment (id TEXT PRIMARY KEY)')
    db.execute('CREATE TABLE IF NOT EXISTS calls (id TEXT PRIMARY KEY,prompt TEXT NOT NULL,reserved INTEGER NOT NULL,charged INTEGER NOT NULL,status TEXT NOT NULL)')
    db.execute('BEGIN IMMEDIATE')
    try:
        existing=db.execute('SELECT id FROM experiment').fetchall()
        if not existing:db.execute('INSERT INTO experiment VALUES (?)',(protocol['id'],))
        elif existing!=[(protocol['id'],)]:raise ValueError('budget ledger belongs to a different protocol')
        db.execute('COMMIT')
    except BaseException:db.execute('ROLLBACK');db.close();raise
    return db


def reserve(db,protocol,call_id,prompt_hash):
    budget=protocol['budget'];amount=micros(budget['call_reservation_usd'])
    if amount<=0 or amount<micros(budget['cli_max_budget_usd']):raise ValueError('invalid reservation')
    db.execute('BEGIN IMMEDIATE')
    try:
        if db.execute('SELECT 1 FROM calls WHERE id=?',(call_id,)).fetchone():raise ValueError('call already reserved; reconcile instead of retrying')
        count,total=db.execute('SELECT COUNT(*),COALESCE(SUM(charged),0) FROM calls').fetchone()
        if db.execute("SELECT 1 FROM calls WHERE status='over_reservation'").fetchone():raise ValueError('provider exceeded reservation; stop for reconciliation')
        if count>=budget['maximum_calls'] or total+amount>micros(budget['limit_usd']):raise ValueError('experiment budget exhausted')
        db.execute('INSERT INTO calls VALUES (?,?,?,?,?)',(call_id,prompt_hash,amount,amount,'reserved'))
        db.execute('COMMIT')
    except BaseException:db.execute('ROLLBACK');raise


def settle(db,call_id,cost):
    charge=micros(cost)
    db.execute('BEGIN IMMEDIATE')
    try:
        row=db.execute('SELECT reserved,status FROM calls WHERE id=?',(call_id,)).fetchone()
        if row is None or row[1]!='reserved':raise ValueError('call not pending')
        db.execute('UPDATE calls SET charged=?,status=? WHERE id=?',(charge,'settled' if charge<=row[0] else 'over_reservation',call_id))
        db.execute('COMMIT')
    except BaseException:db.execute('ROLLBACK');raise


def receipt(db,protocol):
    rows=[dict(zip(['id','prompt_sha256','reserved_microusd','charged_microusd','status'],r)) for r in db.execute('SELECT * FROM calls ORDER BY id')]
    return {'protocol_id':protocol['id'],'limit_usd':protocol['budget']['limit_usd'],'calls':rows,
            'charged_or_reserved_usd':sum(r['charged_microusd'] for r in rows)/1000000,
            'reported_paid_usd':sum(r['charged_microusd'] for r in rows if r['status']!='reserved')/1000000,
            'limitation':'Provider-reported cost, not billing attestation. Unpriced/interrupted calls retain full reservation.'}


def main():
    p=argparse.ArgumentParser(description=__doc__)
    for k in ('protocol','db','prompt','output'):p.add_argument('--'+k,type=Path,required=True)
    p.add_argument('--call-id',required=True);p.add_argument('--claude',type=Path,required=True)
    a=p.parse_args();protocol=checked(a.protocol);prompt=a.prompt.read_bytes()
    if len(prompt)>protocol['budget']['prompt_byte_limit']:raise ValueError('prompt over frozen budget')
    a.output.mkdir(parents=True,exist_ok=False)
    db=connect(a.db,protocol);reserve(db,protocol,a.call_id,digest(prompt))
    env=dict(os.environ,CLAUDE_CODE_MAX_OUTPUT_TOKENS=str(protocol['budget']['output_token_limit']))
    command=[str(a.claude),'-p','--model','opus','--tools','','--strict-mcp-config','--mcp-config','{"mcpServers":{}}',
             '--setting-sources','','--settings','{"disableAllHooks":true}','--no-session-persistence',
             '--output-format','json','--max-budget-usd',str(protocol['budget']['cli_max_budget_usd'])]
    started=time.time()
    try:
        proc=subprocess.run(command,input=prompt,capture_output=True,env=env,timeout=240)
        (a.output/'response.json').write_bytes(proc.stdout);(a.output/'stderr.txt').write_bytes(proc.stderr)
        result=json.loads(proc.stdout)
        if 'total_cost_usd' in result:settle(db,a.call_id,result['total_cost_usd'])
        write_new(a.output/'call.json',{'protocol_id':protocol['id'],'call_id':a.call_id,'prompt_sha256':digest(prompt),
            'started_at_unix':started,'elapsed_seconds':time.time()-started,'exit_code':proc.returncode,
            'response_sha256':digest(proc.stdout),'model_usage':result.get('modelUsage'),
            'runner_sha256':digest(Path(__file__).read_bytes())})
        if proc.returncode or result.get('is_error'):raise RuntimeError('review failed; no automatic retry')
    finally:
        write_new(a.output/'budget.json',receipt(db,protocol));db.close()


if __name__=='__main__':main()
