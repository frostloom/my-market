"""Re-audit frozen first-turn evidence eligibility without model or database calls.

Existing AI semantic labels are retained verbatim. Quantity detection is a
conservative screening rule, never a factual oracle. Source hashes are pinned
here so rewriting a source and its manifest together cannot bless altered data.
"""
import argparse
import collections
import gzip
import hashlib
import json
from pathlib import Path, PurePosixPath
import re

ROOT = Path(__file__).resolve().parents[1]
EVAL = ROOT.parent.parent / 'eval' / 'ai-gateway'  # R2.6-T2: 评测资产移至仓库顶层 eval/ai-gateway
SOURCE = EVAL / 'reviews/2026-10-09-intent-offline-audit'
OUT = EVAL / 'reviews/2026-10-13-answer-evidence-v1'
ARMS = ('nojev', 'hint', 'fast')
PINNED = {
    'archive-manifest.json': '17244b76e950a2df03a53dc94c1364a63599e1584cb44e4e521e4d363f147a5c',
    'draft-cases.json': '641261f9eca556de63156f33755f126986843fc981cb94f73442df540d254c88',
    'METHOD.md': '69b7a0e0a051b153dafdcf33c1717df85f697b2b02c89578086be3e1a9a69aad',
    'README.md': '12c5f5434608b7e79009ed89db509ef79ffc5c6696ba5394263c2610f2bbecd8',
    'reviews.json': '89458b2f7a66721b910fc7678921511ee3558352b958f93fae056bdabec089ee',
    'sources.json': '3950e831a9e2491de1208709c1eaf7bc2541a087d7d4ea3c7bda9699297ab658',
    'summary.json': 'cac3eabc28eb1d1ddfa5fc75f73536b4b0cf323ccec1464ee8e8004b8e3a30d0',
    'raw/fast.json.gz': 'c46cd814984a6687e649e4656c3483eec834e30fc4babc005554404497f1357f',
    'raw/hint.json.gz': '90d62c1aa706151e5d4a79728316170bbb542fb6e4d5540f2f3a2c3ef254543f',
    'raw/nojev.json.gz': '1b9b08b7227e959dd8c4c47f65a1bd98d19aa6c6cce5a67f4dcc6d8acef38492',
}
STATUSES = ('proven-service-failure', 'existing-semantic-review-fail',
            'missing-historical-authority', 'confirmation-only', 'existing-semantic-review-pass')
NUMBER = r'(?:\d[\d,]*(?:\.\d+)?|[零〇一二两三四五六七八九十百千万亿]+)'
QUANTITY = re.compile(
    r'(?:[¥￥$]\s*' + NUMBER + r'|' + NUMBER +
    r'\s*(?:万|亿)?\s*(?:元|块|token(?:s)?|个|次|条|天|日|月|年|小时|分钟|秒|%|％|GB|MB)|'
    r'\d{4}[-/]\d{1,2}[-/]\d{1,2})', re.IGNORECASE)


# --- r26-canonical: begin ---
# R2.6 迁移垫片：本哨兵块之间的全部内容（含本函数与改写表）在 canonical 字节中
# 整体剥离，因此 canonical_code_bytes() 还原出的字节与冻结 archive-manifest 记录
# 的生成器 SHA256（前布局原文）可比对；任何未登记的改动都会使还原哈希不一致而
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

def evidence_path(relative):
    """冻结 sources.json 记录 R2.6 前布局路径（eval/... 相对 ROOT，历史上有 Windows
    分隔符）；eval/ 前缀重锚到顶层 EVAL 下，其余仍在 ROOT 下。"""
    relative = str(relative).replace('\\', '/')
    if relative.startswith('eval/'):
        return EVAL / relative[len('eval/'):]
    return ROOT / relative

def frozen_source_label(path):
    """保持冻结 sources.json 的 frozen_source_directory 标签（前布局形式）逐字节稳定。"""
    try:
        return path.relative_to(ROOT).as_posix()
    except ValueError:
        return 'eval/' + path.relative_to(EVAL).as_posix()

R26_BEGIN = '# --- r26-canonical: begin ---'
R26_END = '# --- r26-canonical: end ---'
MIGRATION_LINES = [
    ("EVAL = ROOT.parent.parent / 'eval' / 'ai-gateway'  # R2.6-T2: 评测资产移至仓库顶层 eval/ai-gateway\n", ''),
    ("SOURCE = EVAL / 'reviews/2026-10-09-intent-offline-audit'\n",
     "SOURCE = ROOT / 'eval/reviews/2026-10-09-intent-offline-audit'\n"),
    ("OUT = EVAL / 'reviews/2026-10-13-answer-evidence-v1'\n",
     "OUT = ROOT / 'eval/reviews/2026-10-13-answer-evidence-v1'\n"),
    ("        original = evidence_path(sources[arm]['path']).resolve()\n"
     "        require(original.is_relative_to(ROOT.resolve()) or original.is_relative_to(EVAL.resolve()),\n"
     "                'Evidence path escapes roots')\n"
     "        require(original.read_bytes() == raw, 'Original source bytes changed')\n",
     "        original = safe_path(ROOT, sources[arm]['path'])\n"
     "        require(original.read_bytes() == raw, 'Original source bytes changed')\n"),
    ("    return rows, {'frozen_source_directory': frozen_source_label(SOURCE),",
     "    return rows, {'frozen_source_directory': SOURCE.relative_to(ROOT).as_posix(),"),
    ("sha(canonical_code_bytes())", "sha(Path(__file__).read_bytes())"),
]
# --- r26-canonical: end ---
def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def encoded(value):
    return (json.dumps(value, ensure_ascii=False, indent=2) + '\n').encode('utf-8')


def read(path):
    return json.loads(path.read_text(encoding='utf-8'))


def write_json(path, value):
    path.write_bytes(encoded(value))


def safe_path(root, relative):
    relative = str(relative).replace('\\', '/')
    parts = PurePosixPath(relative).parts
    require(relative and ':' not in relative and
            not PurePosixPath(relative).is_absolute() and '..' not in parts,
            'Unsafe evidence path: ' + relative)
    candidate = (root / relative).resolve()
    require(candidate.is_relative_to(root.resolve()), 'Evidence path escapes root')
    return candidate


def index_predictions(predictions, definitions):
    result = {}
    for pred in predictions:
        cid = pred['Sample']['id']
        require(cid in definitions and cid not in result, 'Unknown/duplicate source ID: ' + cid)
        require(pred['Sample'] == definitions[cid], 'Changed original sample: ' + cid)
        result[cid] = pred
    require(set(result) == set(definitions), 'Incomplete prediction coverage')
    return result


def load_sources(source=SOURCE):
    # Check every pinned byte before trusting any paths or IDs from JSON.
    for name, digest in PINNED.items():
        path = safe_path(source, name)
        require(path.is_file() and sha(path.read_bytes()) == digest, 'Pinned source mismatch: ' + name)
    old_manifest = read(source / 'archive-manifest.json')
    entries = old_manifest['files']
    require(len(entries) == len({e['path'] for e in entries}), 'Duplicate source manifest path')
    require({e['path'] for e in entries} == set(PINNED) - {'archive-manifest.json'}, 'Source manifest file set')
    for entry in entries:
        require(sha(safe_path(source, entry['path']).read_bytes()) == entry['sha256'], 'Source manifest hash')
    drafts = read(source / 'draft-cases.json')
    require(len(drafts) == 1220 and len({d['id'] for d in drafts}) == 1220, 'Definition coverage')
    definitions = {d['id']: d['original'] for d in drafts}
    sources = read(source / 'sources.json')
    require(set(sources) == set(ARMS), 'Unknown source arm')
    predictions = {}
    for arm in ARMS:
        raw = (source / 'raw' / (arm + '.json.gz')).read_bytes()
        plain = gzip.decompress(raw)
        require(sha(raw) == sources[arm]['compressed_sha256'] and
                sha(plain) == sources[arm]['original_sha256'], 'Compressed/original source hash')
        original = evidence_path(sources[arm]['path']).resolve()
        require(original.is_relative_to(ROOT.resolve()) or original.is_relative_to(EVAL.resolve()),
                'Evidence path escapes roots')
        require(original.read_bytes() == raw, 'Original source bytes changed')
        report = json.loads(plain)
        predictions[arm] = index_predictions(report['Preds'] + report['AdvPreds'], definitions)
    rows = read(source / 'reviews.json')
    require(len(rows) == 1220 and len({r['id'] for r in rows}) == 1220, 'Review IDs duplicate/incomplete')
    require({r['id'] for r in rows} == set(definitions), 'Review ID set changed')
    for row in rows:
        cid = row['id']
        require(row['original'] == definitions[cid], 'Review original changed: ' + cid)
        require(row['review']['id'] == cid and set(row['historical']) == set(ARMS) and
                set(row['review']['arms']) == set(ARMS), 'Review arm/ID mismatch')
        for arm in ARMS:
            require(row['historical'][arm] == predictions[arm][cid], 'Historical prediction/reply changed')
            verdict = row['review']['arms'][arm]
            require(not verdict['numeric_truth_verified'] and not verdict['full_task_verified'],
                    'Unexpected historical authority claim')
    return rows, {'frozen_source_directory': frozen_source_label(SOURCE),
                  'pinned_files_sha256': PINNED, 'original_raw_sources': sources}


def normalize(cid, arm, pred, review):
    mentions = [{'text': m.group(), 'start': m.start(), 'end': m.end()}
                for m in QUANTITY.finditer(pred.get('Reply', ''))]
    service_failure = pred['HTTPCode'] != 200 or bool(pred.get('Err'))
    verdict = review['reply_verdict']
    require(verdict in ('pass', 'fail', 'unverifiable', 'service_failure'), 'Unknown reply verdict')
    if service_failure:
        status = 'proven-service-failure'
    elif verdict in ('fail', 'service_failure'):
        status = 'existing-semantic-review-fail'
    elif pred.get('Pending') and verdict == 'pass':
        status = 'confirmation-only'
    elif mentions or verdict == 'unverifiable':
        status = 'missing-historical-authority'
    else:
        status = 'existing-semantic-review-pass'
    return {'id': cid, 'arm': arm, 'turn_scope': 'historical-first-turn-only',
            'original': pred['Sample'], 'model_reply': pred.get('Reply', ''),
            'prediction_sha256': sha(encoded(pred)), 'http_code': pred['HTTPCode'],
            'error': pred.get('Err', ''), 'tool_names': pred.get('AllTools', []),
            'pending': pred.get('Pending', False), 'prior_ai_review': review,
            'status': status, 'quantity_mentions': mentions,
            'prior_reply_pass_quantity_without_authority': verdict == 'pass' and bool(mentions) and not service_failure,
            'independent_historical_authority': 'not-saved-or-recovered',
            'fact_evidence_eligibility': 'not_evaluated',
            'numeric_truth_verified': False, 'full_task_verified': False}


def derive(rows):
    return [normalize(row['id'], arm, row['historical'][arm], row['review']['arms'][arm])
            for row in sorted(rows, key=lambda r: r['id']) for arm in ARMS]


def summary(rows):
    result = {'historical_unique_cases': len({r['id'] for r in rows}), 'first_turn_outputs': len(rows),
              'new_model_calls': 0, 'human_blind_gold_cases': 0,
              'historical_fast_is_current_v2': False, 'full_task_accuracy': None,
              'numeric_truth_accuracy': None, 'independently_fact_verified_outputs': 0, 'arms': {}}
    for arm in ARMS:
        selected = [r for r in rows if r['arm'] == arm]
        counts = collections.Counter(r['status'] for r in selected)
        flagged = [r['id'] for r in selected if r['prior_reply_pass_quantity_without_authority']]
        result['arms'][arm] = {'outputs': len(selected), 'statuses': {s: counts[s] for s in STATUSES},
            'prior_action_verdicts': dict(sorted(collections.Counter(r['prior_ai_review']['action_verdict'] for r in selected).items())),
            'prior_reply_verdicts': dict(sorted(collections.Counter(r['prior_ai_review']['reply_verdict'] for r in selected).items())),
            'quantity_screen_candidates': sum(bool(r['quantity_mentions']) for r in selected),
            'prior_reply_pass_quantity_without_authority': len(flagged),
            'prior_reply_pass_quantity_without_authority_ids': flagged}
    return result


def products():
    source_rows, sources = load_sources()
    rows = derive(source_rows)
    return {'rows.json': rows, 'summary.json': summary(rows), 'sources.json': sources}


def build(out=OUT):
    if out.exists():
        raise FileExistsError('Refusing to overwrite evidence archive: ' + str(out))
    values = products()
    out.mkdir(parents=True, exist_ok=False)
    for name, value in values.items():
        # Exclusive creation also protects against a competing writer.
        with (out / name).open('xb') as target:
            target.write(encoded(value))
    manifest = {'generator_sha256': sha(canonical_code_bytes()),
                'files': {name: sha(encoded(value)) for name, value in values.items()}}
    with (out / 'archive-manifest.json').open('xb') as target:
        target.write(encoded(manifest))
    verify(out)


def verify(out=OUT):
    expected = products()
    manifest = read(out / 'archive-manifest.json')
    require(manifest['generator_sha256'] == sha(canonical_code_bytes()), 'Generator changed')
    require(set(manifest['files']) == set(expected), 'Output manifest paths changed')
    actual_files = {p.relative_to(out).as_posix() for p in out.rglob('*') if p.is_file()}
    require(actual_files == set(expected) | {'archive-manifest.json'}, 'Unexpected/missing output files')
    for name, value in expected.items():
        raw = safe_path(out, name).read_bytes()
        require(raw == encoded(value), 'Recomputed evidence differs: ' + name)
        require(sha(raw) == manifest['files'][name], 'Output hash mismatch: ' + name)
    print('PASS: 1220 unique cases, 3660 first-turn outputs; pinned raw/definition/review SHA chain and rederived rows/counts. Zero model calls; no numeric truth or full-task score.')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=('build', 'verify'))
    args = parser.parse_args()
    (build if args.command == 'build' else verify)()
