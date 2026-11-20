#!/usr/bin/env python3
"""Prepare local runtime seed overlay; no service or database calls."""
import argparse
import copy
import hashlib
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
EVAL = ROOT.parent / 'eval' / 'ai-gateway'  # R2.6-T2: 评测资产移至仓库顶层 eval/ai-gateway
SOURCE = EVAL / 'task-definitions/2026-10-12-full-dev-v1/profiles.json'
OUT = EVAL / 'runtime-profiles/2026-10-13-six-profiles-v1'


# --- r26-canonical: begin ---
# R2.6 迁移垫片：本哨兵块之间的全部内容（含本函数与改写表）在 canonical 字节中
# 整体剥离，因此 canonical_code_bytes() 还原出的字节与冻结 manifest 记录的生成器
# SHA256（前布局原文）可比对；任何未登记的改动都会使还原哈希不一致而校验失败。
# 只改消费侧解析，不改写冻结数据。
def canonical_code_bytes(path=None):
    text = Path(path or __file__).read_bytes().decode('utf-8')
    lines, skip = [], False
    for line in text.split('\n'):
        if line == R26_BEGIN:
            skip = True
        elif line == R26_END:
            skip = False
        elif not skip:
            lines.append(line)
    text = '\n'.join(lines)
    for migrated, frozen in MIGRATION_LINES:
        if migrated not in text:
            raise ValueError('r26 canonical rewrite drifted from frozen form: ' + migrated[:60])
        text = text.replace(migrated, frozen)
    return text.encode('utf-8')

R26_BEGIN = '# --- r26-canonical: begin ---'
R26_END = '# --- r26-canonical: end ---'
MIGRATION_LINES = [
    ("EVAL = ROOT.parent / 'eval' / 'ai-gateway'  # R2.6-T2: 评测资产移至仓库顶层 eval/ai-gateway\n",
     "EVAL = ROOT.parent.parent / 'eval' / 'ai-gateway'  # R2.6-T2: 评测资产移至仓库顶层 eval/ai-gateway\n"),
    ("EVAL = ROOT.parent.parent / 'eval' / 'ai-gateway'  # R2.6-T2: 评测资产移至仓库顶层 eval/ai-gateway\n", ''),
    ("SOURCE = EVAL / 'task-definitions/2026-10-12-full-dev-v1/profiles.json'\n",
     "SOURCE = ROOT / 'eval/task-definitions/2026-10-12-full-dev-v1/profiles.json'\n"),
    ("OUT = EVAL / 'runtime-profiles/2026-10-13-six-profiles-v1'\n",
     "OUT = ROOT / 'eval/runtime-profiles/2026-10-13-six-profiles-v1'\n"),
    ("        'generator_canonical_sha256': sha(canonical_code_bytes().replace(b'\\r\\n', b'\\n')),",
     "        'generator_canonical_sha256': sha(Path(__file__).read_bytes().replace(b'\\r\\n', b'\\n')),"),
]
# --- r26-canonical: end ---
def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def encode(value):
    return (json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + '\n').encode('utf8')


def build(source):
    out = copy.deepcopy(source)
    if source['schema'] != 'customer-task-profiles.v1' or len(source['accounts']) != 6:
        raise ValueError('Unsupported profile set')
    for p in out['accounts'].values():
        balance = p['balance_cent']
        consumed = sum(b['amount_cent'] for b in p['cash_bill_fixture'])
        if balance < 0 or consumed != 250 or (balance + 200) % 100 or p['ledger_balance_cent'] != balance:
            raise ValueError('Seed invariant mismatch')
        p['initial_quota_cent'] = 50
        p['paid_order']['amount_cent'] = balance + 200
        p['paid_order']['amount_money'] = (balance + 200) // 100
        p['bill_seed_rule'] = 'Create tenant with initial_quota=50cent and project it to private Redis; create/pay integer-yuan recharge via real store methods; stage declared synthetic settled bills and project authoritative ledger balance.'
    out['schema'] = 'customer-runtime-profiles.v1'
    out['kind'] = 'synthetic_development_runtime_seed_overlay'
    out['overlay_changes'] = ['initial_quota=50cent', 'paid_order.amount_cent=balance_cent+200',
                              'No changed wallet/consumption/subscription or task terminal expectations.']
    out['runtime_adapter_ready'] = False
    return out


def artifacts():
    raw = SOURCE.read_bytes()
    result = {'profiles.json': encode(build(json.loads(raw))),
              'summary.json': encode({'profiles': 6, 'model_calls': 0, 'jev_calls': 0,
                                      'database_instantiated_during_export': 0,
                                      'historical_truth_recovered': False, 'full_conversation_adapter_ready': False})}
    result['manifest.json'] = encode({'source_profile_sha256': sha(raw),
        'generator_canonical_sha256': sha(canonical_code_bytes().replace(b'\r\n', b'\n')),
        'files': {name: sha(data) for name, data in result.items()}})
    return result


def export(output):
    result = artifacts()
    output = Path(output)
    output.mkdir(parents=True, exist_ok=False)
    for name, data in result.items():
        with (output / name).open('xb') as f:
            f.write(data)


def verify(output):
    output = Path(output)
    expected = artifacts()
    if {p.name for p in output.iterdir()} != set(expected):
        raise ValueError('Artifact set changed')
    for name, data in expected.items():
        if (output / name).read_bytes() != data:
            raise ValueError('Source/generator/artifact changed: ' + name)
    return json.loads(expected['summary.json'])


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('command', choices=['export', 'verify'])
    p.add_argument('--output', type=Path, default=OUT)
    args = p.parse_args()
    if args.command == 'export':
        export(args.output)
    print(json.dumps(verify(args.output)))


if __name__ == '__main__':
    main()
