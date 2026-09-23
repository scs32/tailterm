import ast
import argparse
import re
import hashlib
import json
import os
import pathlib
import subprocess
from datetime import datetime, timezone

if (os.environ.get('TAILTERM_AGENT'), os.environ.get('TAILTERM_RUN')) != ('agt_3760a4361c514a03', 'run_1a67a6c9be7c7136'):
    raise SystemExit('Execute only in the current db-handler session; do not override identity.')

parser = argparse.ArgumentParser()
parser.add_argument('--preflight-receipt-sha256', required=True)
args = parser.parse_args()
assert re.fullmatch(r'[0-9a-f]{64}', args.preflight_receipt_sha256)
base = pathlib.Path(__file__).resolve().parent
root = base / 'operator'
source = (root / 'scripts/truenas_release_preflight.py').read_bytes()
assert hashlib.sha256(source).hexdigest() == '82fd43e1edf6c9f3de89101c06b374cc1b3c0430e372676f5822740b01560d46'
plan_bytes = (base / 'backup-plan.json').read_bytes()
assert hashlib.sha256(plan_bytes).hexdigest() == '29a6229ea70d929c75ebf37b8f6463e480c89eed3c35c9b3ce45a579ebc319c6'
plan = json.loads(plan_bytes)
receipt_bytes = (base / 'handler-preflight-receipt.json').read_bytes()
assert hashlib.sha256(receipt_bytes).hexdigest() == args.preflight_receipt_sha256
receipt = json.loads(receipt_bytes)
names = {'_json_value', '_profile_evidence', '_database_evidence', '_open_read_only'}
functions = [ast.get_source_segment(source.decode(), n) for n in ast.parse(source).body if isinstance(n, ast.FunctionDef) and n.name in names]
assert len(functions) == 4
worker = '''import hashlib,json,os,pathlib,socket,sqlite3,sys
from typing import Any
PROFILE_TABLES = ['profile_meta','profiles','profile_history']
class PreflightFailure(Exception): pass
'''
worker += '\n\n'.join(functions)
worker += '\nplan = ' + repr(plan) + '\n'
worker += '''
assert socket.gethostname() == plan['targetHost'], 'host mismatch; no database access'
assert os.path.realpath(sys.executable) == plan['targetExecutable'], 'executable mismatch; no database access'
paths = [pathlib.Path(plan['sourceDatabase']), pathlib.Path(plan['backupDestination'])]
assert all(p.resolve(strict=True) == p and p.is_file() for p in paths), 'noncanonical database path'
result = {'host':socket.gethostname(),'executable':os.path.realpath(sys.executable),'mutation':False}
schemas = {}
for label,path in zip(['live','backup'],paths):
    db = _open_read_only(path)
    try:
        db.execute('BEGIN')
        evidence = _database_evidence(db)
        schema = db.execute("SELECT type,name,tbl_name,sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name").fetchall()
        payload = json.dumps(schema,separators=(',',':'),ensure_ascii=False).encode()
        evidence['schema'] = {'objects':len(schema),'sha256':hashlib.sha256(payload).hexdigest()}
        schemas[label] = {(row[0],row[1]):row for row in schema}
        if label == 'live':
            evidence['phaseSuccessorObligationRows'] = db.execute("SELECT count(*) FROM phase_successor_obligations").fetchone()[0]
            evidence['phaseSuccessorLeadTransferRows'] = db.execute("SELECT count(*) FROM phase_successor_lead_transfers").fetchone()[0]
        result[label] = evidence
    finally:
        db.close()
result['profilesEqual'] = result['live']['profiles'] == result['backup']['profiles']
result['schemaEqual'] = result['live']['schema'] == result['backup']['schema']
added = sorted(set(schemas['live'])-set(schemas['backup']))
removed = sorted(set(schemas['backup'])-set(schemas['live']))
changed = sorted(k for k in set(schemas['backup']) & set(schemas['live']) if schemas['backup'][k] != schemas['live'][k])
result['schemaDelta'] = {'added':added,'removed':removed,'changed':changed}
expected_added = [('index','phase_successor_obligations_lead'),('index','phase_successor_obligations_successor'),('table','phase_successor_lead_transfers'),('table','phase_successor_obligations')]
result['expectedMigrationObjects'] = added==expected_added and not removed and not changed
result['integrityPassed'] = all(result[k]['integrity']=='ok' and result[k]['foreignKeyViolations']==0 for k in ['live','backup'])
print(json.dumps(result))
'''
result = subprocess.run(['ssh','-o','BatchMode=yes','-o','ConnectTimeout=10','truenas','/usr/bin/python3.11','-'],input=worker,text=True,capture_output=True,timeout=60)
record = {'order':5835,'observedAt':datetime.now(timezone.utc).isoformat(),'exitCode':result.returncode,'methodSourceSha256':hashlib.sha256(source).hexdigest(),'databaseMutation':False}
if result.returncode:
    record['error'] = result.stderr[-1000:]
else:
    record['evidence'] = json.loads(result.stdout)
target = base / 'handler-postcheck.json'
with target.open('x') as f:
    os.chmod(target,0o600)
    json.dump(record,f,indent=2)
    f.write('\n')
print(json.dumps(record))
print(json.dumps({'receiptSha256':hashlib.sha256(target.read_bytes()).hexdigest()}))
raise SystemExit(result.returncode)
