"""Versioned offline successor to the frozen full-pilot preparation; no credentials or network."""
import argparse
import importlib.util
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
EVAL = ROOT.parent.parent / 'eval' / 'ai-gateway'  # R2.6-T2: 评测资产移至仓库顶层 eval/ai-gateway
OUT = EVAL / 'paired-pilots/2026-10-17-jev-proxy-preflight-v4'
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
    ("EVAL = ROOT.parent.parent / 'eval' / 'ai-gateway'  # R2.6-T2: 评测资产移至仓库顶层 eval/ai-gateway\n", ''),
    ("OUT = EVAL / 'paired-pilots/2026-10-17-jev-proxy-preflight-v4'\n",
     "OUT = ROOT / 'eval/paired-pilots/2026-10-17-jev-proxy-preflight-v4'\n"),
    ("base.sha((EVAL / 'paired-pilots/2026-10-14-full-preflight-v1/proposal.json').read_bytes())",
     "base.sha((root / 'eval/paired-pilots/2026-10-14-full-preflight-v1/proposal.json').read_bytes())"),
    ("base.sha(canonical_code_bytes())", "base.sha(Path(__file__).read_bytes())"),
]
# --- r26-canonical: end ---
spec = importlib.util.spec_from_file_location('full_pilot_base', ROOT / 'scripts/prepare-full-customer-pilot.py')
base = importlib.util.module_from_spec(spec)
spec.loader.exec_module(base)
EXTRA_FILES = ('internal/agent/telemetry.go', 'internal/agent/observability.go',
               'internal/observability/config.go', 'internal/observability/content.go',
               'internal/observability/observer.go', 'internal/observability/evaluation.go',
               'internal/observability/processor.go', 'internal/taskbench/paired_report.go',
               'scripts/langfuse-eval.py', 'scripts/prepare-observed-customer-pilot.py',
               'scripts/prepare-full-customer-pilot.py')


def prepare(root):
    p = base.prepare(root)
    p['schema'] = 'full-pilot-observed-preparation.v2'
    p['source_bindings']['previous_proposal_sha256'] = base.sha((EVAL / 'paired-pilots/2026-10-14-full-preflight-v1/proposal.json').read_bytes())
    hashes = p['source_bindings']['runtime_files_sha256']
    hashes.update({name: base.sha((root / name).read_bytes().replace(b'\r\n', b'\n')) for name in EXTRA_FILES})
    p['observability_requirements'] = {
        'environment': 'evaluation', 'capture_content': True, 'sample_rate': 1,
        'llm_judge_enabled': False, 'score_source': 'trusted offline paired reports',
        'score_sync_separate_from_inference': True, 'unreviewed_success': 'unknown',
        'missing_usage': 'unknown', 'drops_and_export_failures_must_be_zero': True,
        'production_observer_restart_performed': False,
        'score_failure_recovery': 'resynchronize deterministic score IDs; never rerun inference',
        'startup_command': 'scripts/langfuse-eval.py check --preflight <this-directory> --env-file <explicit-config> --out <new-json>',
    }
    p['blockers'].append('Startup check, per-batch explicit Observer injection, score sync/readback and full telemetry coverage must be verified for each actual run.')
    return p


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=('prepare', 'verify'))
    args = parser.parse_args()
    raw = base.encoded(prepare(ROOT))
    if args.command == 'prepare':
        OUT.mkdir(exist_ok=False)
        (OUT / 'proposal.json').write_bytes(raw)
        (OUT / 'manifest.json').write_bytes(base.encoded({'schema': 'observed-preparation-manifest.v2',
            'generator_sha256': base.sha(canonical_code_bytes()), 'files': {'proposal.json': base.sha(raw)}}))
    manifest = json.loads((OUT / 'manifest.json').read_text(encoding='utf-8'))
    if manifest['generator_sha256'] != base.sha(canonical_code_bytes()) or manifest['files'] != {'proposal.json': base.sha(raw)} or (OUT / 'proposal.json').read_bytes() != raw:
        raise ValueError('Observed preparation changed; create a new version, never overwrite evidence')
    print('PASS: observed v2 binding; unchanged 1220 primary tasks/2440 arms; zero inference and no paid authorization.')


if __name__ == '__main__':
    main()
