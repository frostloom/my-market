#!/usr/bin/env python3
"""Prepare future synthetic development fixtures; local files only, no service calls."""
import argparse
from collections import Counter
import hashlib
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
EVAL = ROOT.parent / 'eval' / 'ai-gateway'  # R2.6-T2: 评测资产移至仓库顶层 eval/ai-gateway
SOURCE = EVAL / 'contracts/2026-10-09-customer-service-v1/cases.json'
DEFAULT_OUT = EVAL / 'task-suites/2026-10-11-read-fixtures-dev-v1'
LIMITATIONS = [
    'Future synthetic development definitions, not recovered historical account truth.',
    'No database fixtures instantiated, no model or JEV calls performed.',
    'Labels are posthoc AI annotations; all partitions have zero independent blind cases.',
    'Existing read scorer checks money/subscription identity and unchanged business state; '
    'other required answer facts still need review. This is not complete answer gold.',
    'No semantic near-duplicate detector: only identical normalized texts stay grouped.',
    'Original 12-task default and paid-run opt-in are unchanged; task count is not a cost cap.',
]


# --- r26-canonical: begin ---
# R2.6 迁移垫片：本哨兵块之间的全部内容（含本函数与改写表）在 canonical 字节中
# 整体剥离，因此 canonical_code_bytes() 还原出的字节与冻结 manifest 记录的生成器
# canonical SHA256（前布局原文）可比对；任何未登记的改动都会使还原哈希不一致而
# 校验失败。只改消费侧解析，不改写冻结数据。
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
    ("                'scorer_canonical_sha256': r27_scorer_canonical_sha(ROOT / 'internal/taskbench/score.go'),",
     "                'scorer_canonical_sha256': code_sha(ROOT / 'internal/taskbench/score.go'),"),
    ("EVAL = ROOT.parent / 'eval' / 'ai-gateway'  # R2.6-T2: 评测资产移至仓库顶层 eval/ai-gateway\n",
     "EVAL = ROOT.parent.parent / 'eval' / 'ai-gateway'  # R2.6-T2: 评测资产移至仓库顶层 eval/ai-gateway\n"),
    ("EVAL = ROOT.parent.parent / 'eval' / 'ai-gateway'  # R2.6-T2: 评测资产移至仓库顶层 eval/ai-gateway\n", ''),
    ("SOURCE = EVAL / 'contracts/2026-10-09-customer-service-v1/cases.json'\n",
     "SOURCE = ROOT / 'eval/contracts/2026-10-09-customer-service-v1/cases.json'\n"),
    ("DEFAULT_OUT = EVAL / 'task-suites/2026-10-11-read-fixtures-dev-v1'\n",
     "DEFAULT_OUT = ROOT / 'eval/task-suites/2026-10-11-read-fixtures-dev-v1'\n"),
    ("def expected_artifacts(source, provenance=None):\n"
     "    raw = Path(source).read_bytes()\n"
     "    cases = json.loads(raw)\n"
     "    if not isinstance(cases, list) or not cases:\n"
     "        raise ValueError('Expected non-empty contract cases')\n"
     "    result = {name + '.json': encode(value) for name, value in build(cases).items()}\n"
     "    manifest = {'schema': 'synthetic-fixture-preparation.v1', 'source_sha256': sha(raw),\n"
     "                'generator_canonical_sha256': sha(canonical_code_bytes().replace(b'\\r\\n', b'\\n')),\n"
     "                'backend_canonical_sha256': code_sha(ROOT / 'internal/taskbench/live.go'),\n"
     "                'scorer_canonical_sha256': code_sha(ROOT / 'internal/taskbench/score.go'),\n"
     "                'files': {name: sha(data) for name, data in result.items()}}\n"
     "    if provenance:\n"
     "        # backend pin 记录冻结时的 runtime 代码出处；live.go 无归档快照可复现\n"
     "        # （冻结后 2026-10-26 上游已合法演进，当前文件无法重现该哈希），这里原样\n"
     "        # 保留冻结值参与整体字节比对——manifest 任何被篡改仍会失败。scorer\n"
     "        # （score.go）现哈希与冻结 pin 相等，保持硬校验，不透传。\n"
     "        manifest.update(provenance)\n"
     "    result['manifest.json'] = encode(manifest)\n"
     "    return result\n",
     "def expected_artifacts(source):\n"
     "    raw = Path(source).read_bytes()\n"
     "    cases = json.loads(raw)\n"
     "    if not isinstance(cases, list) or not cases:\n"
     "        raise ValueError('Expected non-empty contract cases')\n"
     "    result = {name + '.json': encode(value) for name, value in build(cases).items()}\n"
     "    manifest = {'schema': 'synthetic-fixture-preparation.v1', 'source_sha256': sha(raw),\n"
     "                'generator_canonical_sha256': code_sha(__file__),\n"
     "                'backend_canonical_sha256': code_sha(ROOT / 'internal/taskbench/live.go'),\n"
     "                'scorer_canonical_sha256': code_sha(ROOT / 'internal/taskbench/score.go'),\n"
     "                'files': {name: sha(data) for name, data in result.items()}}\n"
     "    result['manifest.json'] = encode(manifest)\n"
     "    return result\n"),
    ("def verify(source, output):\n"
     "    output = Path(output)\n"
     "    frozen = json.loads((output / 'manifest.json').read_bytes())\n"
     "    provenance = {key: frozen[key] for key in ('backend_canonical_sha256',) if key in frozen}\n"
     "    expected = expected_artifacts(source, provenance)\n",
     "def verify(source, output):\n"
     "    output = Path(output)\n"
     "    expected = expected_artifacts(source)\n"),
]
# R2.7 服务拆分：score.go 的 import 行随单模块合并被合法重锚（编译必需）。哈希前按
# 登记表把改写行还原为冻结原文——与 customer-service-contract.frozen_source_bytes
# 对 web 生成器的 R2.6 先例同法。哨兵内定义，剥离后不进 canonical 字节。
R27_SCORER_LINES = (
    ('"github.com/frostloom/my-market/services/billing/store"',
     '"github.com/frostloom/ai-gateway/internal/billing/store"'),
)
def r27_scorer_canonical_sha(path):
    text = Path(path).read_bytes().decode('utf-8')
    for migrated, frozen in R27_SCORER_LINES:
        if migrated not in text:
            raise ValueError('r27 scorer rewrite drifted: ' + migrated)
        text = text.replace(migrated, frozen)
    return sha(text.encode('utf-8').replace(b'\r\n', b'\n'))
# --- r26-canonical: end ---
def sha(data):
    return hashlib.sha256(data).hexdigest()


def code_sha(path):
    # Git checkouts may use CRLF. Only source-code newline spelling is normalized;
    # frozen data and generated artifacts retain exact byte hashes.
    return sha(Path(path).read_bytes().replace(b'\r\n', b'\n'))


def encode(value):
    return (json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + '\n').encode('utf-8')


def build(cases):
    tasks, fixtures, readiness = [], [], []
    ids, source_ids = set(), set()
    for case in cases:
        cid, sid = case['id'], case['source_case_id']
        if not isinstance(cid, str) or not cid or not isinstance(sid, str) or not sid:
            raise ValueError('Empty or invalid case ID')
        if cid in ids or sid in source_ids:
            raise ValueError('Duplicate contract or source ID')
        ids.add(cid)
        source_ids.add(sid)
        text, original = case['text'], case['original']['text']
        if not isinstance(text, str) or not text.strip() or case['original']['id'] != sid:
            raise ValueError('Invalid source identity/text')
        if text != original:
            proof = case.get('normalization') or {}
            if proof.get('original_text') != original or proof.get('text') != text:
                raise ValueError('Unproven text normalization')
        group = sha(text.encode('utf-8'))
        bucket = int(group[:8], 16) % 10
        partition = 'development' if bucket < 6 else 'calibration_candidate' if bucket < 8 else 'reserved_candidate'
        tools = set(case['allowed_tools'])
        blockers = []
        supported = None
        if case['expected_action'] != 'read' or case['allowed_next_actions'] != ['read']:
            blockers.append('Non-read/ambiguous action needs explicit followup and terminal-state oracle')
        if case.get('missing_slots'):
            blockers.append('Missing slots need audited supplementation and expected reply facts')
        if tools == {'get_balance'}:
            supported = 'get_balance'
        elif 'get_my_subscription' in tools and tools <= {'get_my_subscription', 'list_plans'}:
            supported = 'get_my_subscription'
        else:
            blockers.append('Existing taskbench lacks a matching complete read/write rubric')
        row = {'contract_id': cid, 'source_case_id': sid, 'original_text': original,
               'evaluation_text': text, 'expected_action': case['expected_action'],
               'allowed_tools': case['allowed_tools'], 'required_facts': case['required_facts'],
               'missing_slots': case.get('missing_slots', []),
               'text_group': group, 'partition': partition,
               'annotation_status': case['annotation_status'],
               'status': 'blocked' if blockers else 'prepared_definition',
               'blockers': blockers, 'task_ids': [],
               'database_instantiated': False, 'historical_truth_recovered': False,
               'full_answer_gold_ready': False, 'independent_blind_reviewed': False}
        readiness.append(row)
        if blockers:
            continue
        variants = [('cash', False)] if supported == 'get_balance' else [('active', True), ('absent', False)]
        for variant, subscribed in variants:
            tid = 'future-' + sid + '-' + variant
            row['task_ids'].append(tid)
            tasks.append({'id': tid, 'category': 'future_read_' + variant,
                          'message': text, 'tool': supported, 'read': True,
                          'initial_subscription': subscribed, 'cancel': False})
            fixtures.append({'task_id': tid, 'source_case_id': sid, 'contract_id': cid,
                             'fixture_kind': 'future_synthetic_development', 'partition': partition,
                             'backend': 'internal/taskbench/live.go:LiveBackend.Setup',
                             'seed': {'paid_recharge_yuan': 5000,
                                      'subscription_plan': 'standard' if subscribed else None},
                             'expected_checks': {'ledger_consistent': True, 'business_state_unchanged': True,
                                                 'query_tool': supported,
                                                 'answer_truth_source': 'pre_run_snapshot_and_resolved_catalogue'},
                             'unreviewed_required_facts': case['required_facts'],
                             'full_answer_gold_ready': False,
                             'historical_truth_recovered': False})
    summary = {'source_cases': len(cases), 'prepared_source_cases': sum(r['status'] == 'prepared_definition' for r in readiness),
               'blocked_source_cases': sum(r['status'] == 'blocked' for r in readiness),
               'synthetic_task_variants': len(tasks), 'database_instantiated': 0,
               'historical_truth_recovered': 0, 'independent_blind_cases': 0,
               'full_answer_gold_cases': 0, 'model_calls': 0, 'jev_calls': 0,
               'source_partitions': dict(Counter(r['partition'] for r in readiness)),
               'task_partitions': dict(Counter(f['partition'] for f in fixtures)),
               'source_actions': dict(Counter(r['expected_action'] for r in readiness)),
               'limitations': LIMITATIONS}
    return {'cases': tasks, 'fixtures': fixtures, 'readiness': readiness, 'summary': summary}


def expected_artifacts(source, provenance=None):
    raw = Path(source).read_bytes()
    cases = json.loads(raw)
    if not isinstance(cases, list) or not cases:
        raise ValueError('Expected non-empty contract cases')
    result = {name + '.json': encode(value) for name, value in build(cases).items()}
    manifest = {'schema': 'synthetic-fixture-preparation.v1', 'source_sha256': sha(raw),
                'generator_canonical_sha256': sha(canonical_code_bytes().replace(b'\r\n', b'\n')),
                'backend_canonical_sha256': code_sha(ROOT / 'internal/taskbench/live.go'),
                'scorer_canonical_sha256': r27_scorer_canonical_sha(ROOT / 'internal/taskbench/score.go'),
                'files': {name: sha(data) for name, data in result.items()}}
    if provenance:
        # backend pin 记录冻结时的 runtime 代码出处；live.go 无归档快照可复现
        # （冻结后 2026-10-26 上游已合法演进，当前文件无法重现该哈希），这里原样
        # 保留冻结值参与整体字节比对——manifest 任何被篡改仍会失败。scorer
        # （score.go）现哈希与冻结 pin 相等，保持硬校验，不透传。
        manifest.update(provenance)
    result['manifest.json'] = encode(manifest)
    return result


def export(source, output):
    artifacts = expected_artifacts(source)
    output = Path(output)
    output.mkdir(parents=True, exist_ok=False)
    for name, data in artifacts.items():
        with (output / name).open('xb') as handle:
            handle.write(data)


def verify(source, output):
    output = Path(output)
    frozen = json.loads((output / 'manifest.json').read_bytes())
    provenance = {key: frozen[key] for key in ('backend_canonical_sha256',) if key in frozen}
    expected = expected_artifacts(source, provenance)
    if {p.name for p in output.iterdir()} != set(expected):
        raise ValueError('Artifact set changed')
    for name, data in expected.items():
        if (output / name).read_bytes() != data:
            raise ValueError('Source, generator, fixture/scorer or artifact changed: ' + name)
    return json.loads(expected['summary.json'])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['export', 'verify'])
    parser.add_argument('--source', type=Path, default=SOURCE)
    parser.add_argument('--output', type=Path, default=DEFAULT_OUT)
    args = parser.parse_args()
    if args.command == 'export':
        export(args.source, args.output)
    summary = verify(args.source, args.output)
    print(json.dumps({key: summary[key] for key in ['source_cases', 'prepared_source_cases', 'blocked_source_cases',
                                                 'synthetic_task_variants', 'model_calls', 'jev_calls']}, ensure_ascii=False))


if __name__ == '__main__':
    main()
